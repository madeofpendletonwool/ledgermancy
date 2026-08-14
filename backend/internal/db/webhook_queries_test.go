package db

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/testdb"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/webhooks"
)

// The scoping tests for outgoing webhooks.
//
// Everything that decides WHO receives an event lives in the enqueue statements
// in queries/webhooks.sql, so this is where that decision is pinned. Two
// separate promises are on trial:
//
//  1. A webhook only ever receives events from its own household.
//  2. A webhook only ever receives ALERT events its owner could see in the app.
//
// The second is the one that would be easy to get wrong and expensive to
// discover: without it, wiring up a webhook would be a way to read a partner's
// private account.

type webhookFixture struct {
	pool *pgxpool.Pool
	q    *dbgen.Queries

	householdA, householdB uuid.UUID
	// alice and bob share household A; carol is in household B.
	alice, bob, carol uuid.UUID
	// A webhook per person, each subscribed to every trigger, so an event that
	// does not arrive is about scoping rather than about subscription.
	aliceHook, bobHook, carolHook uuid.UUID
	// A transaction on a SHARED account and one on Alice's PRIVATE account.
	sharedTx, alicePrivateTx uuid.UUID
	alertA                   uuid.UUID
}

func setupWebhookFixture(t *testing.T) (context.Context, *webhookFixture) {
	t.Helper()

	url := testdb.URL(t)
	ctx := context.Background()

	pool, err := Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	f := &webhookFixture{
		pool: pool, q: dbgen.New(pool),
		householdA: uuid.New(), householdB: uuid.New(),
		alice: uuid.New(), bob: uuid.New(), carol: uuid.New(),
		alertA: uuid.New(),
	}

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed exec: %v\n%s", err, sql)
		}
	}

	for _, h := range []struct {
		id   uuid.UUID
		name string
	}{{f.householdA, "Webhook A"}, {f.householdB, "Webhook B"}} {
		exec(`INSERT INTO households (id, name) VALUES ($1, $2)`, h.id, h.name)
		id := h.id
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id = $1`, id)
		})
	}

	addUser := func(id, household uuid.UUID) {
		exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
		      VALUES ($1, $2, $3, 'x', 'Tester')`, id, household, id.String()+"@example.test")
	}
	addUser(f.alice, f.householdA)
	addUser(f.bob, f.householdA)
	addUser(f.carol, f.householdB)

	// Two Plaid items in household A: one shared with the household, one Alice
	// keeps to herself. Visibility hangs off the item, which is what
	// account_access resolves.
	sharedItem, privateItem := uuid.New(), uuid.New()
	sharedAcct, privateAcct := uuid.New(), uuid.New()
	exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, products, status, is_shared)
	      VALUES ($1, $2, $3, '\x00', '{transactions}', 'active', TRUE)`, sharedItem, f.alice, sharedItem.String())
	exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, products, status, is_shared)
	      VALUES ($1, $2, $3, '\x00', '{transactions}', 'active', FALSE)`, privateItem, f.alice, privateItem.String())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type)
	      VALUES ($1, $2, $3, 'Joint', 'depository')`, sharedAcct, sharedItem, sharedAcct.String())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type)
	      VALUES ($1, $2, $3, 'Alice only', 'depository')`, privateAcct, privateItem, privateAcct.String())

	f.sharedTx, f.alicePrivateTx = uuid.New(), uuid.New()
	exec(`INSERT INTO transactions (id, account_id, amount, currency, date, name, source)
	      VALUES ($1, $2, '120.00', 'USD', '2026-02-01', 'Groceries', 'plaid')`, f.sharedTx, sharedAcct)
	exec(`INSERT INTO transactions (id, account_id, amount, currency, date, name, source)
	      VALUES ($1, $2, '900.00', 'USD', '2026-02-02', 'Something private', 'plaid')`, f.alicePrivateTx, privateAcct)

	exec(`INSERT INTO alerts (id, household_id, type) VALUES ($1, $2, 'big_spend')`, f.alertA, f.householdA)

	addHook := func(household, user uuid.UUID, name string) uuid.UUID {
		row, err := f.q.CreateWebhook(ctx, dbgen.CreateWebhookParams{
			HouseholdID: household, UserID: user, Name: name,
			Url: "https://example.test/" + name, SecretEncrypted: []byte{0x00},
			Active: true, Triggers: webhooks.Triggers,
		})
		if err != nil {
			t.Fatalf("create webhook %s: %v", name, err)
		}
		return row.ID
	}
	f.aliceHook = addHook(f.householdA, f.alice, "alice")
	f.bobHook = addHook(f.householdA, f.bob, "bob")
	f.carolHook = addHook(f.householdB, f.carol, "carol")

	return ctx, f
}

// TestWebhooksOnlyReceiveTheirOwnHouseholdsEvents is promise (1).
func TestWebhooksOnlyReceiveTheirOwnHouseholdsEvents(t *testing.T) {
	ctx, f := setupWebhookFixture(t)

	ids, err := webhooks.Emit(ctx, f.q, f.householdA,
		webhooks.TriggerInsightCreated, uuid.NewString(), time.Now(), map[string]string{"x": "y"})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	got := f.webhookIDsFor(t, ctx, ids)
	if len(got) != 2 {
		t.Fatalf("household A's insight reached %d webhooks, want both of its own", len(got))
	}
	if !contains(got, f.aliceHook) || !contains(got, f.bobHook) {
		t.Errorf("reached %v, want Alice's (%s) and Bob's (%s)", got, f.aliceHook, f.bobHook)
	}
	if contains(got, f.carolHook) {
		t.Error("another household's webhook received this household's event")
	}
}

// TestPrivateAccountAlertsStayPrivate is promise (2), and it is the one that
// matters: an alert raised by a charge on Alice's private account must not reach
// a webhook Bob created, even though Bob is in the same household and subscribed
// to alert.fired.
func TestPrivateAccountAlertsStayPrivate(t *testing.T) {
	ctx, f := setupWebhookFixture(t)

	// Baseline. An alert on the SHARED account reaches both members' webhooks —
	// without this the assertion below could pass because nothing is ever
	// delivered at all.
	shared := f.raiseAlert(t, ctx, &f.sharedTx)
	if len(shared) != 2 {
		t.Fatalf("a shared-account alert reached %d webhooks, want both members'", len(shared))
	}

	private := f.raiseAlert(t, ctx, &f.alicePrivateTx)
	if len(private) != 1 {
		t.Fatalf("a private-account alert reached %d webhooks, want only the owner's", len(private))
	}
	if private[0] != f.aliceHook {
		t.Fatalf("a private-account alert reached webhook %s, want Alice's (%s)", private[0], f.aliceHook)
	}
}

// An aggregate alert carries no transaction and is household-visible in the app,
// so it must reach every subscriber — the filter must not over-apply and quietly
// drop the alerts that have nothing to hide.
func TestAggregateAlertsReachEverySubscriber(t *testing.T) {
	ctx, f := setupWebhookFixture(t)

	if got := f.raiseAlert(t, ctx, nil); len(got) != 2 {
		t.Fatalf("an aggregate alert reached %d webhooks, want both members'", len(got))
	}
}

// The dedupe key is what lets the periodic sweeps re-examine the same events
// every pass without delivering them again. Two enqueues of one event produce
// one message per subscriber, not two.
func TestReEnqueuingTheSameEventIsIdempotent(t *testing.T) {
	ctx, f := setupWebhookFixture(t)

	eventID := uuid.NewString()
	first, err := webhooks.Emit(ctx, f.q, f.householdA,
		webhooks.TriggerInsightCreated, eventID, time.Now(), map[string]string{"x": "y"})
	if err != nil {
		t.Fatalf("first Emit: %v", err)
	}
	second, err := webhooks.Emit(ctx, f.q, f.householdA,
		webhooks.TriggerInsightCreated, eventID, time.Now(), map[string]string{"x": "y"})
	if err != nil {
		t.Fatalf("second Emit: %v", err)
	}

	if len(first) != 2 {
		t.Fatalf("first pass wrote %d messages, want one per subscriber", len(first))
	}
	if len(second) != 0 {
		t.Fatalf("re-running the same event wrote %d more messages, want none", len(second))
	}
}

// A paused webhook stops receiving. That is the whole reason `active` exists
// rather than deleting the row while a receiver is being fixed.
func TestPausedWebhooksReceiveNothing(t *testing.T) {
	ctx, f := setupWebhookFixture(t)

	if _, err := f.q.UpdateWebhook(ctx, dbgen.UpdateWebhookParams{
		ID: f.bobHook, HouseholdID: f.householdA, Name: "bob", Url: "https://example.test/bob",
		Active: false, Triggers: webhooks.Triggers,
	}); err != nil {
		t.Fatalf("pause webhook: %v", err)
	}

	ids, err := webhooks.Emit(ctx, f.q, f.householdA,
		webhooks.TriggerInsightCreated, uuid.NewString(), time.Now(), map[string]string{})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	got := f.webhookIDsFor(t, ctx, ids)
	if len(got) != 1 || got[0] != f.aliceHook {
		t.Fatalf("a paused webhook was still delivered to: %v", got)
	}
}

// The write-before-deliver invariant, stated as a database fact: the message row
// exists, and is pending, before anything has tried to deliver it. A receiver
// that is down cannot lose the event, because the event was durable first.
func TestAMessageIsDurableBeforeAnyDeliveryIsAttempted(t *testing.T) {
	ctx, f := setupWebhookFixture(t)

	ids, err := webhooks.Emit(ctx, f.q, f.householdA,
		webhooks.TriggerInsightCreated, uuid.NewString(), time.Now(), map[string]string{"a": "b"})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if len(ids) == 0 {
		t.Fatal("Emit wrote nothing")
	}

	msg, err := f.q.GetWebhookMessageForDelivery(ctx, ids[0])
	if err != nil {
		t.Fatalf("GetWebhookMessageForDelivery: %v", err)
	}
	if msg.Status != "pending" {
		t.Errorf("status = %q, want pending before any delivery", msg.Status)
	}
	if msg.Attempts != 0 {
		t.Errorf("attempts = %d, want 0 before any delivery", msg.Attempts)
	}
	if len(msg.Payload) == 0 {
		t.Error("the payload must be frozen at enqueue time, not rebuilt at delivery")
	}

	// And the sweep can find it once it is old enough, which is what makes the
	// event recoverable when the hand-off to the queue itself was lost.
	stranded, err := f.q.ListPendingWebhookMessages(ctx, dbgen.ListPendingWebhookMessagesParams{
		CreatedAt: time.Now().Add(time.Hour), Limit: 10,
	})
	if err != nil {
		t.Fatalf("ListPendingWebhookMessages: %v", err)
	}
	if !contains(stranded, ids[0]) {
		t.Error("the sweep cannot see a pending message, so a lost job would lose the event")
	}
}

// raiseAlert records an alert event (against a transaction, or aggregate when
// txID is nil) and fans it out, returning the webhooks that were given a
// message.
func (f *webhookFixture) raiseAlert(t *testing.T, ctx context.Context, txID *uuid.UUID) []uuid.UUID {
	t.Helper()

	var eventID uuid.UUID
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO alert_events (alert_id, transaction_id, payload)
		 VALUES ($1, $2, '{"amount":"900.00"}') RETURNING id`,
		f.alertA, txID).Scan(&eventID); err != nil {
		t.Fatalf("raise alert event: %v", err)
	}

	ids, err := webhooks.EmitAlertEvent(ctx, f.q, f.householdA, eventID, time.Now(),
		webhooks.AlertData{AlertEventID: eventID.String(), AlertType: "big_spend"})
	if err != nil {
		t.Fatalf("EmitAlertEvent: %v", err)
	}
	return f.webhookIDsFor(t, ctx, ids)
}

// webhookIDsFor resolves message ids to the webhooks they were written for, so
// an assertion can name the subscriber rather than the message.
func (f *webhookFixture) webhookIDsFor(t *testing.T, ctx context.Context, messageIDs []uuid.UUID) []uuid.UUID {
	t.Helper()

	out := make([]uuid.UUID, 0, len(messageIDs))
	for _, id := range messageIDs {
		msg, err := f.q.GetWebhookMessageForDelivery(ctx, id)
		if err != nil {
			t.Fatalf("GetWebhookMessageForDelivery(%s): %v", id, err)
		}
		out = append(out, msg.WebhookID)
	}
	return out
}

func contains(ids []uuid.UUID, want uuid.UUID) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
