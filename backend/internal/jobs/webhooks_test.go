package jobs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/crypto"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/testdb"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/webhooks"
)

// The delivery worker's behaviour, against a real database and a real HTTP
// receiver. What is on trial here is the part the pure-Go tests in
// internal/webhooks cannot reach: what happens to the MESSAGE when a delivery
// succeeds, fails transiently, or keeps failing.

type deliveryFixture struct {
	q      *dbgen.Queries
	cipher *crypto.Cipher

	householdID uuid.UUID
	webhookID   uuid.UUID
	secret      string
}

func setupDeliveryFixture(t *testing.T, receiverURL string) (context.Context, *deliveryFixture) {
	t.Helper()

	url := testdb.URL(t)
	ctx := context.Background()

	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// A fixed 32-byte key: the worker has to open a secret it did not seal, and
	// a test that sealed with a random key would only be testing itself.
	cipher, err := crypto.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}

	f := &deliveryFixture{q: dbgen.New(pool), cipher: cipher, householdID: uuid.New()}

	userID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO households (id, name) VALUES ($1, 'Delivery')`, f.householdID); err != nil {
		t.Fatalf("seed household: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id = $1`, f.householdID)
	})
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, household_id, email, password_hash, display_name)
	                             VALUES ($1, $2, $3, 'x', 'Tester')`,
		userID, f.householdID, userID.String()+"@example.test"); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	f.secret, err = webhooks.NewSecret()
	if err != nil {
		t.Fatalf("NewSecret: %v", err)
	}
	sealed, err := cipher.SealString(f.secret)
	if err != nil {
		t.Fatalf("seal secret: %v", err)
	}

	row, err := f.q.CreateWebhook(ctx, dbgen.CreateWebhookParams{
		HouseholdID: f.householdID, UserID: userID, Name: "receiver",
		Url: receiverURL, SecretEncrypted: sealed, Active: true,
		Triggers: []string{webhooks.TriggerInsightCreated},
	})
	if err != nil {
		t.Fatalf("create webhook: %v", err)
	}
	f.webhookID = row.ID

	return ctx, f
}

// enqueue writes one message the way a producer would and returns its id.
func (f *deliveryFixture) enqueue(t *testing.T, ctx context.Context) uuid.UUID {
	t.Helper()

	ids, err := webhooks.Emit(ctx, f.q, f.householdID,
		webhooks.TriggerInsightCreated, uuid.NewString(), time.Now(),
		webhooks.InsightData{InsightID: uuid.NewString(), Kind: "big_charge", Title: "A large charge"})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("Emit wrote %d messages, want 1", len(ids))
	}
	return ids[0]
}

// work runs one delivery attempt, as River would on attempt number n.
func (f *deliveryFixture) work(ctx context.Context, messageID uuid.UUID, attempt int) error {
	worker := &DeliverWebhookWorker{Queries: f.q, Cipher: f.cipher, HTTP: webhooks.NewClient()}
	return worker.Work(ctx, &river.Job[DeliverWebhookArgs]{
		JobRow: &rivertype.JobRow{Attempt: attempt, MaxAttempts: webhooks.MaxAttempts},
		Args:   DeliverWebhookArgs{MessageID: messageID},
	})
}

// A receiver that accepts marks the message sent, records the attempt, and — the
// bit that would be easy to leave out — verifies as a real receiver.
func TestDeliverySucceedsAndIsRecorded(t *testing.T) {
	// The receiver needs the signing secret, which only exists once the fixture
	// has minted it — so the handler closes over the variable and the fixture
	// fills it in below. The server is not serving until the delivery runs.
	var secret string
	var verified bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		verified = webhooks.Verify(secret, r.Header.Get(webhooks.SignatureHeader),
			body, time.Now(), time.Minute) == nil
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, f := setupDeliveryFixture(t, srv.URL)
	secret = f.secret

	messageID := f.enqueue(t, ctx)
	if err := f.work(ctx, messageID, 1); err != nil {
		t.Fatalf("Work: %v", err)
	}

	if !verified {
		t.Error("the receiver could not verify the signature the worker sent")
	}

	msg, err := f.q.GetWebhookMessageForDelivery(ctx, messageID)
	if err != nil {
		t.Fatalf("read message: %v", err)
	}
	if msg.Status != "sent" {
		t.Errorf("status = %q, want sent", msg.Status)
	}
	if msg.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", msg.Attempts)
	}

	attempts, err := f.q.ListWebhookAttempts(ctx, dbgen.ListWebhookAttemptsParams{
		MessageID: messageID, WebhookID: f.webhookID,
	})
	if err != nil {
		t.Fatalf("list attempts: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("recorded %d attempts, want 1", len(attempts))
	}
	if attempts[0].ResponseStatus == nil || *attempts[0].ResponseStatus != 200 {
		t.Errorf("recorded status = %v, want 200", attempts[0].ResponseStatus)
	}
}

// A transient failure has to keep the message deliverable and hand River an
// error, which is what schedules the retry. Losing the event here — marking it
// failed on the first 500, say — is the failure mode this whole table exists to
// prevent.
func TestATransientFailureRetriesAndKeepsTheEvent(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, f := setupDeliveryFixture(t, srv.URL)
	messageID := f.enqueue(t, ctx)

	if err := f.work(ctx, messageID, 1); err == nil {
		t.Fatal("a 502 must return an error so River retries")
	}

	msg, err := f.q.GetWebhookMessageForDelivery(ctx, messageID)
	if err != nil {
		t.Fatalf("read message: %v", err)
	}
	if msg.Status != "pending" {
		t.Fatalf("status = %q after one transient failure, want pending — the event must survive", msg.Status)
	}

	// The retry lands, and the message settles as sent.
	if err := f.work(ctx, messageID, 2); err != nil {
		t.Fatalf("retry: %v", err)
	}
	msg, err = f.q.GetWebhookMessageForDelivery(ctx, messageID)
	if err != nil {
		t.Fatalf("read message: %v", err)
	}
	if msg.Status != "sent" {
		t.Errorf("status = %q after a successful retry, want sent", msg.Status)
	}
	if msg.Attempts != 2 {
		t.Errorf("attempts = %d, want both requests counted", msg.Attempts)
	}
}

// After the budget runs out the message is DEAD-LETTERED rather than dropped:
// marked failed, carrying the reason, with every attempt still behind it. That
// is what makes "it silently stopped working three weeks ago" a visible fact.
func TestDeliveryDeadLettersAfterTheLastAttempt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ctx, f := setupDeliveryFixture(t, srv.URL)
	messageID := f.enqueue(t, ctx)

	for attempt := 1; attempt <= webhooks.MaxAttempts; attempt++ {
		err := f.work(ctx, messageID, attempt)
		if err == nil {
			t.Fatalf("attempt %d: a 500 must be reported as a failure", attempt)
		}

		msg, readErr := f.q.GetWebhookMessageForDelivery(ctx, messageID)
		if readErr != nil {
			t.Fatalf("read message: %v", readErr)
		}
		wantStatus := "pending"
		if attempt == webhooks.MaxAttempts {
			wantStatus = "failed"
		}
		if msg.Status != wantStatus {
			t.Fatalf("attempt %d of %d: status = %q, want %q",
				attempt, webhooks.MaxAttempts, msg.Status, wantStatus)
		}
	}

	msg, err := f.q.GetWebhookMessageForDelivery(ctx, messageID)
	if err != nil {
		t.Fatalf("read message: %v", err)
	}
	if msg.Attempts != webhooks.MaxAttempts {
		t.Errorf("attempts = %d, want %d", msg.Attempts, webhooks.MaxAttempts)
	}

	attempts, err := f.q.ListWebhookAttempts(ctx, dbgen.ListWebhookAttemptsParams{
		MessageID: messageID, WebhookID: f.webhookID,
	})
	if err != nil {
		t.Fatalf("list attempts: %v", err)
	}
	if len(attempts) != webhooks.MaxAttempts {
		t.Errorf("kept %d attempt records, want all %d", len(attempts), webhooks.MaxAttempts)
	}
}

// The headline invariant: a receiver that is not there at all cannot cost the
// event. Nothing answers, every attempt is recorded, and the message is still
// on file afterwards with the reason attached.
func TestADownReceiverNeverLosesTheEvent(t *testing.T) {
	// A real port that nothing is listening on.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	ctx, f := setupDeliveryFixture(t, url)
	messageID := f.enqueue(t, ctx)

	if err := f.work(ctx, messageID, 1); err == nil {
		t.Fatal("a refused connection must be reported as a failure")
	}

	msg, err := f.q.GetWebhookMessageForDelivery(ctx, messageID)
	if err != nil {
		t.Fatalf("the message is gone after a failed delivery: %v", err)
	}
	if msg.Status != "pending" {
		t.Errorf("status = %q, want pending — the event outlives the receiver", msg.Status)
	}
	if len(msg.Payload) == 0 {
		t.Error("the payload did not survive the failed delivery")
	}

	attempts, err := f.q.ListWebhookAttempts(ctx, dbgen.ListWebhookAttemptsParams{
		MessageID: messageID, WebhookID: f.webhookID,
	})
	if err != nil {
		t.Fatalf("list attempts: %v", err)
	}
	if len(attempts) != 1 || attempts[0].Error == nil {
		t.Fatalf("want one attempt carrying the reason nothing answered, got %+v", attempts)
	}
	if attempts[0].ResponseStatus != nil {
		t.Errorf("recorded a response status of %v for a request nothing answered", *attempts[0].ResponseStatus)
	}
}

// A paused webhook stops delivering without failing: the message stays pending,
// so re-enabling the subscription lets the sweep drain what accumulated.
func TestAPausedWebhookHoldsItsMessages(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, f := setupDeliveryFixture(t, srv.URL)
	messageID := f.enqueue(t, ctx)

	if _, err := f.q.UpdateWebhook(ctx, dbgen.UpdateWebhookParams{
		ID: f.webhookID, HouseholdID: f.householdID, Name: "receiver", Url: srv.URL,
		Active: false, Triggers: []string{webhooks.TriggerInsightCreated},
	}); err != nil {
		t.Fatalf("pause webhook: %v", err)
	}

	if err := f.work(ctx, messageID, 1); err != nil {
		t.Fatalf("a paused webhook is not an error: %v", err)
	}
	if called {
		t.Error("a paused webhook was still delivered to")
	}

	msg, err := f.q.GetWebhookMessageForDelivery(ctx, messageID)
	if err != nil {
		t.Fatalf("read message: %v", err)
	}
	if msg.Status != "pending" {
		t.Errorf("status = %q, want pending so re-enabling drains the backlog", msg.Status)
	}
}
