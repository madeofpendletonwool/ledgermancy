package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/ai"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/config"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/mailer"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/reporting"
)

// The in-app digest, end to end against a real Postgres (doc 25).
//
// Every assertion here is one of the doc's verification points, and each one is
// a behaviour that had no coverage because the digest previously existed only as
// a push:
//
//   - a household with NO notification channel still gets an entry — the whole
//     point of the feature;
//
//   - the stored figures are immutable, even after the underlying transactions
//     change;
//
//   - a second sweep in the same period does not write a second entry;
//
//   - two members with different institution sharing get different, correctly
//     scoped entries, and neither can read the other's;
//
//   - a mail failure does not fail the job.
//
//     TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' go test ./internal/jobs/
func TestDigestEntries(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	client := riverClientForTest(t, ctx, pool)

	q := dbgen.New(pool)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, sql)
		}
	}

	// The worker reads its own clock, so the seeded spending has to land inside
	// the window it will choose. A weekly cadence reports month-to-date, so
	// "earlier today" is inside every window this test exercises.
	today := time.Now().UTC()
	inWindow := today.Format(time.DateOnly)
	if today.Day() == 1 {
		// On the 1st a month-to-date window is a single day; that is still fine,
		// but a monthly cadence would look at last month. This test only uses the
		// weekly cadence, so no adjustment is needed — noted so a future reader
		// does not "fix" it.
		t.Log("running on the 1st: the month-to-date window is one day wide")
	}

	householdID := uuid.New()
	alice, bob := uuid.New(), uuid.New()
	sharedItem, aliceOnlyItem := uuid.New(), uuid.New()
	sharedAcct, privateAcct := uuid.New(), uuid.New()
	groceries := uuid.New()

	exec(`INSERT INTO households (id, name) VALUES ($1, 'Digest Test')`, householdID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id = $1`, householdID)
	})
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name, role)
	      VALUES ($1, $2, $3, 'x', 'Alice', 'owner')`, alice, householdID, alice.String()+"@example.test")
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name, role)
	      VALUES ($1, $2, $3, 'x', 'Bob', 'member')`, bob, householdID, bob.String()+"@example.test")

	// One shared institution both members see, and one private to Alice. This is
	// what makes their digests legitimately different, and is why an entry is
	// per-user rather than per-household.
	exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, products, status, is_shared)
	      VALUES ($1, $2, $3, '\x00', '{transactions}', 'active', TRUE)`,
		sharedItem, alice, sharedItem.String())
	exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, products, status, is_shared)
	      VALUES ($1, $2, $3, '\x00', '{transactions}', 'active', FALSE)`,
		aliceOnlyItem, alice, aliceOnlyItem.String())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type)
	      VALUES ($1, $2, $3, 'Joint checking', 'depository')`, sharedAcct, sharedItem, sharedAcct.String())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type)
	      VALUES ($1, $2, $3, 'Alice private', 'depository')`, privateAcct, aliceOnlyItem, privateAcct.String())
	exec(`INSERT INTO categories (id, household_id, name, slug)
	      VALUES ($1, $2, 'Groceries', 'digest-test-groceries')`, groceries, householdID)

	sharedTxn := uuid.New()
	exec(`INSERT INTO transactions
	        (id, account_id, amount, currency, date, name, merchant_name, merchant_key, category_id, source)
	      VALUES ($1, $2, '40.00', 'USD', $3, 'MARKET', 'Market', 'market', $4, 'plaid')`,
		sharedTxn, sharedAcct, inWindow, groceries)
	exec(`INSERT INTO transactions
	        (account_id, amount, currency, date, name, merchant_name, merchant_key, category_id, source)
	      VALUES ($1, '25.00', 'USD', $2, 'CORNER SHOP', 'Corner Shop', 'corner-shop', $3, 'plaid')`,
		privateAcct, inWindow, groceries)

	// AI disabled: the deterministic path is the one that must work everywhere,
	// and it is what a keyless install runs.
	worker := &DigestWorker{
		Queries: q,
		AI:      ai.New(config.AIConfig{}),
		Client:  client,
		AppURL:  "https://ledger.example.test",
	}
	run := func(t *testing.T, userID uuid.UUID, force bool) {
		t.Helper()
		if err := worker.Work(ctx, &river.Job[DigestArgs]{
			Args: DigestArgs{UserID: userID, HouseholdID: householdID, Force: force},
		}); err != nil {
			t.Fatalf("digest worker: %v", err)
		}
	}
	entriesFor := func(t *testing.T, userID uuid.UUID) []dbgen.DigestEntry {
		t.Helper()
		rows, err := q.ListDigestEntries(ctx, dbgen.ListDigestEntriesParams{
			UserID: userID, Limit: 50, Offset: 0,
		})
		if err != nil {
			t.Fatalf("list digest entries: %v", err)
		}
		return rows
	}

	var aliceEntry dbgen.DigestEntry

	t.Run("a user with no notification channel still gets an in-app entry", func(t *testing.T) {
		// Alice has set NOTHING: no channel, no digest.enabled, no digest.in_app.
		// Before doc 25 this produced nothing at all, which is the bug.
		run(t, alice, false)

		rows := entriesFor(t, alice)
		if len(rows) != 1 {
			t.Fatalf("got %d entries for a channel-less user, want 1", len(rows))
		}
		aliceEntry = rows[0]

		// And nothing was pushed, because there was nowhere to push to.
		var deliveries int
		if err := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM digest_deliveries WHERE user_id = $1`, alice).Scan(&deliveries); err != nil {
			t.Fatalf("count deliveries: %v", err)
		}
		if deliveries != 0 {
			t.Errorf("recorded %d push deliveries for a user with no channel, want 0", deliveries)
		}

		p := decodePayload(t, aliceEntry.Payload)
		// Alice sees both institutions: $40 shared + $25 private.
		if p.Spending != "$65.00" {
			t.Errorf("Alice's spending = %q, want $65.00 (shared + her own private item)", p.Spending)
		}
		if p.TransactionCount != 2 {
			t.Errorf("transaction_count = %d, want 2", p.TransactionCount)
		}
	})

	t.Run("figures are immutable once stored", func(t *testing.T) {
		// The core invariant. Change the underlying data inside the period, then
		// re-run the digest: what Alice already read must not move.
		exec(`UPDATE transactions SET amount = '999.00' WHERE id = $1`, sharedTxn)
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(),
				`UPDATE transactions SET amount = '40.00' WHERE id = $1`, sharedTxn)
		})

		run(t, alice, false)

		rows := entriesFor(t, alice)
		if len(rows) != 1 {
			t.Fatalf("a second run wrote %d entries, want the original 1", len(rows))
		}
		p := decodePayload(t, rows[0].Payload)
		if p.Spending != "$65.00" {
			t.Errorf("stored spending changed to %q after the data moved; a digest must "+
				"still say what it said when it was read", p.Spending)
		}
		if rows[0].ID != aliceEntry.ID {
			t.Error("the entry was replaced rather than left alone")
		}
	})

	t.Run("a forced send does not rewrite an already-stored period", func(t *testing.T) {
		// "Send one now" is allowed to push again, but it must not mutate the
		// stored copy — write-once is unconditional.
		run(t, alice, true)

		rows := entriesFor(t, alice)
		if len(rows) != 1 {
			t.Fatalf("a forced run wrote %d entries, want 1", len(rows))
		}
		if p := decodePayload(t, rows[0].Payload); p.Spending != "$65.00" {
			t.Errorf("forced send rewrote the stored figures to %q", p.Spending)
		}
	})

	t.Run("entries are per-user and correctly scoped", func(t *testing.T) {
		run(t, bob, false)

		rows := entriesFor(t, bob)
		if len(rows) != 1 {
			t.Fatalf("got %d entries for Bob, want 1", len(rows))
		}
		p := decodePayload(t, rows[0].Payload)
		// Bob sees the shared institution only. If this ever equals Alice's
		// figure, her private account has leaked into his digest.
		if p.Spending != "$40.00" {
			t.Errorf("Bob's spending = %q, want $40.00 (the shared item alone)", p.Spending)
		}

		// And Bob cannot read Alice's entry by id.
		if _, err := q.GetDigestEntry(ctx, dbgen.GetDigestEntryParams{
			ID: aliceEntry.ID, UserID: bob,
		}); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("Bob read Alice's digest entry (err = %v); entries must be scoped by user", err)
		}
	})

	t.Run("unread counts track the mark-read", func(t *testing.T) {
		counts, err := q.CountDigestEntries(ctx, bob)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		if counts.Total != 1 || counts.Unread != 1 {
			t.Fatalf("counts = %+v, want total 1 / unread 1", counts)
		}

		entry := entriesFor(t, bob)[0]
		if err := q.MarkDigestEntryRead(ctx, dbgen.MarkDigestEntryReadParams{
			ID: entry.ID, UserID: bob,
		}); err != nil {
			t.Fatalf("mark read: %v", err)
		}
		counts, err = q.CountDigestEntries(ctx, bob)
		if err != nil {
			t.Fatalf("recount: %v", err)
		}
		if counts.Unread != 0 {
			t.Errorf("unread = %d after marking read, want 0", counts.Unread)
		}

		// Another member's mark-read must not touch it.
		if err := q.MarkDigestEntryRead(ctx, dbgen.MarkDigestEntryReadParams{
			ID: entry.ID, UserID: alice,
		}); err != nil {
			t.Fatalf("cross-user mark read errored: %v", err)
		}
	})

	t.Run("the sweep considers a user who has configured nothing", func(t *testing.T) {
		candidates, err := q.ListDigestCandidateUsers(ctx)
		if err != nil {
			t.Fatalf("list candidates: %v", err)
		}
		var found *dbgen.ListDigestCandidateUsersRow
		for i := range candidates {
			if candidates[i].UserID == alice {
				found = &candidates[i]
			}
		}
		if found == nil {
			t.Fatal("a user with no digest preferences at all is not a sweep candidate; " +
				"the in-app digest would never be generated for them")
		}
		if !found.InAppEnabled {
			t.Error("in_app_enabled defaulted to false; it must default ON")
		}
		if found.PushEnabled {
			t.Error("push_enabled defaulted to true; pushing to someone who never asked is not a safe default")
		}
		if found.EmailEnabled {
			t.Error("email_enabled defaulted to true")
		}
		if found.Cadence != "weekly" {
			t.Errorf("cadence defaulted to %q, want weekly", found.Cadence)
		}
	})

	t.Run("a child is never a sweep candidate", func(t *testing.T) {
		// A child login can write its own user-scoped preferences, so without the
		// role filter a child could switch a household spending recap on for
		// themselves — past every adult-only guard in the HTTP layer.
		child := uuid.New()
		exec(`INSERT INTO users (id, household_id, email, password_hash, display_name, role)
		      VALUES ($1, $2, $3, 'x', 'Kid', 'child')`, child, householdID, child.String()+"@example.test")
		exec(`INSERT INTO preferences (scope, user_id, key, value)
		      VALUES ('user', $1, 'digest.enabled', 'true'::jsonb)`, child)

		candidates, err := q.ListDigestCandidateUsers(ctx)
		if err != nil {
			t.Fatalf("list candidates: %v", err)
		}
		for _, c := range candidates {
			if c.UserID == child {
				t.Fatal("a child login is a digest candidate")
			}
		}
	})

	t.Run("a mail failure does not fail the job", func(t *testing.T) {
		// Bob opts into email against a mailer that always fails. The job must
		// still succeed: the entry is already stored, and retrying the whole
		// digest because a mail server is down would be worse than no email.
		exec(`INSERT INTO preferences (scope, user_id, key, value)
		      VALUES ('user', $1, 'digest.email', 'true'::jsonb)`, bob)
		exec(`DELETE FROM digest_entries WHERE user_id = $1`, bob)

		failing := &failingMailer{}
		worker.Mail = failing
		t.Cleanup(func() { worker.Mail = nil })

		run(t, bob, false)

		if !failing.called {
			t.Error("email was never attempted despite the opt-in")
		}
		if len(entriesFor(t, bob)) != 1 {
			t.Error("the entry did not survive a failed email")
		}
	})

	t.Run("an unconfigured mailer is never called", func(t *testing.T) {
		// SMTP off is the default, and a user who ticked the box on a deployment
		// with no mail server must not produce a send attempt.
		exec(`DELETE FROM digest_entries WHERE user_id = $1`, bob)

		spy := &spyMailer{Sender: mailer.New(config.SMTPConfig{})}
		worker.Mail = spy
		t.Cleanup(func() { worker.Mail = nil })

		run(t, bob, false)

		if spy.called {
			t.Error("an unconfigured mailer was asked to send")
		}
		if len(entriesFor(t, bob)) != 1 {
			t.Error("the entry was not written")
		}
	})
}

// TestDigestInProgressNarrativeNotServedStale is the regression test for the
// "$0.00 in and out" bug. A weekly digest reports the current month-to-date, a
// window that ends at "now" and advances every day. The shared monthly cache is
// frozen at write time and only refreshed weekly, so a recap written when the
// month was empty must NOT be served beside the freshly-computed figures — the
// narrative and the payload have to agree by construction.
//
// The scenario mirrors the report: a stale monthly_summaries row for the current
// month says everything is zero, while real activity has since landed in-window.
// The digest must regenerate the narrative for its own window rather than reuse
// the cache, and it must leave that shared cache untouched (a partial month
// cannot overwrite the canonical full-month recap).
func TestDigestInProgressNarrativeNotServedStale(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	client := riverClientForTest(t, ctx, pool)
	q := dbgen.New(pool)

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, sql)
		}
	}

	// The worker reads its own clock, so seed inside the month-to-date window it
	// will choose: "earlier today" is always in range for a weekly digest.
	today := time.Now().UTC()
	inWindow := today.Format(time.DateOnly)

	householdID := uuid.New()
	alice := uuid.New()
	item, acct, groceries := uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO households (id, name) VALUES ($1, 'Stale Narrative Test')`, householdID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id = $1`, householdID)
	})
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name, role)
	      VALUES ($1, $2, $3, 'x', 'Alice', 'owner')`, alice, householdID, alice.String()+"@example.test")
	exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, products, status, is_shared)
	      VALUES ($1, $2, $3, '\x00', '{transactions}', 'active', TRUE)`, item, alice, item.String())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type)
	      VALUES ($1, $2, $3, 'Checking', 'depository')`, acct, item, acct.String())
	exec(`INSERT INTO categories (id, household_id, name, slug)
	      VALUES ($1, $2, 'Groceries', 'stale-narrative-groceries')`, groceries, householdID)
	exec(`INSERT INTO transactions
	        (account_id, amount, currency, date, name, merchant_name, merchant_key, category_id, source)
	      VALUES ($1, '900.85', 'USD', $2, 'MARKET', 'Market', 'market', $3, 'plaid')`,
		acct, inWindow, groceries)

	// The stale cache: a recap for the current month written when nothing had
	// posted yet — the exact text the bug report quotes.
	staleText := "STALE: money in and money out are both $0.00."
	curMonth := firstOfMonth(today)
	exec(`INSERT INTO monthly_summaries (household_id, month, summary, model)
	      VALUES ($1, $2, $3, 'stale-model')`, householdID, curMonth, staleText)

	// A stub Messages-API endpoint that returns one unmistakable fresh sentence,
	// so the stored narrative proves the model was called for this window rather
	// than the cache being read.
	freshText := "FRESH: regenerated for the current month-to-date window."
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":           "msg_test",
			"type":         "message",
			"role":         "assistant",
			"content":      []map[string]string{{"type": "text", "text": freshText}},
			"stop_reason":  "end_turn",
			"usage":        map[string]int{"input_tokens": 1, "output_tokens": 1},
		})
	}))
	t.Cleanup(srv.Close)

	worker := &DigestWorker{
		Queries: q,
		AI:      ai.New(config.AIConfig{APIKey: "test-key", BaseURL: srv.URL, Model: "test-model"}),
		Client:  client,
		AppURL:  "https://ledger.example.test",
	}
	if err := worker.Work(ctx, &river.Job[DigestArgs]{
		Args: DigestArgs{UserID: alice, HouseholdID: householdID, Force: true},
	}); err != nil {
		t.Fatalf("digest worker: %v", err)
	}

	rows, err := q.ListDigestEntries(ctx, dbgen.ListDigestEntriesParams{
		UserID: alice, Limit: 50, Offset: 0,
	})
	if err != nil {
		t.Fatalf("list digest entries: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d entries, want 1", len(rows))
	}
	entry := rows[0]

	// The headline assertion: the stored narrative is the freshly generated one,
	// not the stale "$0.00" cache. If this fails, the weekly digest is reusing a
	// frozen recap beside figures that have since moved.
	if entry.Narrative == nil {
		t.Fatal("stored narrative is nil; the digest should have regenerated it for the in-progress window")
	}
	if got := *entry.Narrative; got != freshText {
		t.Errorf("stored narrative = %q, want the freshly generated %q (the stale cache must not be reused "+
			"for a month-to-date window)", got, freshText)
	}

	// The figures must describe the real in-window activity, confirming the
	// narrative and the payload now cover the same window.
	p := decodePayload(t, entry.Payload)
	if p.Spending != "$900.85" {
		t.Errorf("payload spending = %q, want $900.85", p.Spending)
	}

	// The shared monthly cache must be left exactly as the digest found it: a
	// partial month neither reads from nor warms the canonical full-month recap.
	cached, err := q.GetMonthlySummary(ctx, dbgen.GetMonthlySummaryParams{
		HouseholdID: householdID, Month: curMonth,
	})
	if err != nil {
		t.Fatalf("re-read cached summary: %v", err)
	}
	if cached.Summary != staleText {
		t.Errorf("shared cache summary was overwritten to %q; an in-progress digest must not warm the cache",
			cached.Summary)
	}
	if cached.Model != "stale-model" {
		t.Errorf("shared cache model was overwritten to %q", cached.Model)
	}
}

// failingMailer stands in for a mail server that is reachable enough to try and
// broken enough to fail.
type failingMailer struct{ called bool }

func (m *failingMailer) Enabled() bool { return true }
func (m *failingMailer) Send(context.Context, mailer.Message) error {
	m.called = true
	return errors.New("connection refused")
}

// spyMailer records whether Send was reached, delegating the Enabled() decision
// to a real mailer so the gating under test is the production one.
type spyMailer struct {
	mailer.Sender
	called bool
}

func (m *spyMailer) Send(ctx context.Context, msg mailer.Message) error {
	m.called = true
	return m.Sender.Send(ctx, msg)
}

func decodePayload(t *testing.T, raw []byte) reporting.DigestPayload {
	t.Helper()
	var p reporting.DigestPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("decode stored payload: %v", err)
	}
	return p
}

// riverClientForTest brings River's own schema up and returns an insert-only
// client, so the digest worker can enqueue a push exactly as it does in
// production rather than against a stub.
func riverClientForTest(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *river.Client[pgx.Tx] {
	t.Helper()
	driver := riverpgxv5.New(pool)
	migrator, err := rivermigrate.New(driver, nil)
	if err != nil {
		t.Fatalf("river migrator: %v", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		t.Fatalf("river migrate: %v", err)
	}
	client, err := river.NewClient(driver, &river.Config{})
	if err != nil {
		t.Fatalf("river client: %v", err)
	}
	return client
}
