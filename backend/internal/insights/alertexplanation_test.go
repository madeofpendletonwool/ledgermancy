package insights

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/apex42group/ledgermancy/backend/internal/db"
	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
)

// TestAlertExplanationProducer seeds a merchant with cheap history plus one big
// flagged charge and its alert_event, then confirms the producer raises an
// alert_explanation whose typical_amount equals the SQL baseline excluding the
// flagged transaction, and that a re-run after upserting the explanation
// produces nothing (spent once per event).
//
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' go test ./internal/insights/
func TestAlertExplanationProducer(t *testing.T) {
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
	q := dbgen.New(pool)

	householdID := uuid.New()
	userID := uuid.New()
	itemID := uuid.New()
	acctID := uuid.New()
	catID := uuid.New()
	alertID := uuid.New()
	flaggedTx := uuid.New()
	eventID := uuid.New()

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, sql)
		}
	}
	exec(`INSERT INTO households (id, name) VALUES ($1, 'Explain Test')`, householdID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id = $1`, householdID)
	})
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
	      VALUES ($1, $2, $3, 'x', 'Tester')`, userID, householdID, userID.String()+"@example.test")
	exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, products, status)
	      VALUES ($1, $2, $3, '\x00', '{transactions}', 'active')`, itemID, userID, itemID.String())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type)
	      VALUES ($1, $2, $3, 'Checking', 'depository')`, acctID, itemID, acctID.String())
	exec(`INSERT INTO categories (id, household_id, name, slug) VALUES ($1, $2, 'Dining', 'dining')`, catID, householdID)

	// Cheap history at KFC: 10, 12, 14 → average 12.00 across 3 visits.
	for _, amt := range []string{"10.00", "12.00", "14.00"} {
		exec(`INSERT INTO transactions (account_id, amount, currency, date, name, merchant_name, merchant_key, category_id, source)
		      VALUES ($1, $2, 'USD', '2026-06-15', 'KFC', 'KFC', 'kfc', $3, 'plaid')`, acctID, amt, catID)
	}
	// The flagged charge — excluded from the baseline.
	exec(`INSERT INTO transactions (id, account_id, amount, currency, date, name, merchant_name, merchant_key, category_id, source)
	      VALUES ($1, $2, '500.00', 'USD', '2026-07-20', 'KFC', 'KFC', 'kfc', $3, 'plaid')`, flaggedTx, acctID, catID)

	exec(`INSERT INTO alerts (id, household_id, type, config, enabled) VALUES ($1, $2, 'unusual_merchant', '{}', TRUE)`, alertID, householdID)
	payload, _ := json.Marshal(map[string]string{
		"merchant": "KFC", "merchant_key": "kfc", "amount": "500.00", "date": "2026-07-20",
	})
	exec(`INSERT INTO alert_events (id, alert_id, transaction_id, payload) VALUES ($1, $2, $3, $4::jsonb)`,
		eventID, alertID, flaggedTx, string(payload))

	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)

	cands, err := alertExplanationProducer{}.Detect(ctx, q, householdID, now)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1", len(cands))
	}
	c := cands[0]
	if c.Kind != "alert_explanation" {
		t.Errorf("kind = %q", c.Kind)
	}
	if c.Data["alert_event_id"] != eventID.String() {
		t.Errorf("alert_event_id = %v, want %v", c.Data["alert_event_id"], eventID)
	}
	if got := c.Data["typical_amount"]; got != "12.00" {
		t.Errorf("typical_amount = %v, want 12.00 (avg of 10/12/14, flagged excluded)", got)
	}
	if got := c.Data["visit_count"]; got != "3" {
		t.Errorf("visit_count = %v, want 3", got)
	}
	if c.DedupeKey != "alert_explanation:"+eventID.String() {
		t.Errorf("dedupe_key = %q", c.DedupeKey)
	}

	// Upsert the explanation, then a re-run must find nothing — one model call
	// per event, not one per sweep.
	dataJSON, _ := json.Marshal(c.Data)
	if _, err := q.UpsertInsight(ctx, dbgen.UpsertInsightParams{
		HouseholdID: householdID, Kind: c.Kind, Priority: int16(c.Priority),
		Title: c.Title, Body: c.Body, Data: dataJSON, Period: c.Period, DedupeKey: c.DedupeKey,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	again, err := alertExplanationProducer{}.Detect(ctx, q, householdID, now)
	if err != nil {
		t.Fatalf("detect (2nd): %v", err)
	}
	if len(again) != 0 {
		t.Errorf("second detect produced %d candidates, want 0 (already explained)", len(again))
	}
}
