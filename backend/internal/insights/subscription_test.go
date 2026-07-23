package insights

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/apex42group/ledgermancy/backend/internal/db"
	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
)

// TestSubscriptionProducer drives the producer against a real Postgres: a
// merchant whose charges rise over time must surface as price_creep with a
// delta equal to recent_avg − early_avg, a small flat merchant as a zombie, and
// a re-run must produce the same dedupe keys (no duplicates once upserted).
//
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' go test ./internal/insights/
func TestSubscriptionProducer(t *testing.T) {
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

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, sql)
		}
	}
	exec(`INSERT INTO households (id, name) VALUES ($1, 'Subs Test')`, householdID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id = $1`, householdID)
	})
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
	      VALUES ($1, $2, $3, 'x', 'Tester')`, userID, householdID, userID.String()+"@example.test")
	exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, products, status)
	      VALUES ($1, $2, $3, '\x00', '{transactions}', 'active')`, itemID, userID, itemID.String())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type)
	      VALUES ($1, $2, $3, 'Checking', 'depository')`, acctID, itemID, acctID.String())
	exec(`INSERT INTO categories (id, household_id, name, slug) VALUES ($1, $2, 'Software', 'software')`, catID, householdID)

	charge := func(merchant, key, amount, date string) {
		exec(`INSERT INTO transactions
		        (account_id, amount, currency, date, name, merchant_name, merchant_key, category_id, source)
		      VALUES ($1, $2, 'USD', $3, $4, $4, $5, $6, 'plaid')`,
			acctID, amount, date, merchant, key, catID)
	}

	// "now" is fixed so the active-window gate is deterministic. Charges land on
	// the 15th of each month, the last within the active window.
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)

	// Streamy: monthly, older half $10, newer half $13 → 30% creep, delta $3.00.
	streamyDates := []string{"2026-02-15", "2026-03-15", "2026-04-15", "2026-05-15", "2026-06-15", "2026-07-15"}
	for i, d := range streamyDates {
		amt := "10.00"
		if i >= 3 {
			amt = "13.00"
		}
		charge("Streamy", "streamy", amt, d)
	}

	// Gymzo: monthly, flat $8 → zombie (small, regular, active), no creep.
	for _, d := range []string{"2026-02-15", "2026-03-15", "2026-04-15", "2026-05-15", "2026-06-15", "2026-07-15"} {
		charge("Gymzo", "gymzo", "8.00", d)
	}

	cands, err := subscriptionProducer{}.Detect(ctx, q, householdID, now)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}

	byKey := make(map[string]Candidate, len(cands))
	for _, c := range cands {
		byKey[c.DedupeKey] = c
	}

	// Streamy → price_creep with the exact delta and both endpoint amounts.
	streamy, ok := byKey["subscription:streamy"]
	if !ok {
		t.Fatalf("expected a subscription candidate for streamy, got %d candidates: %+v", len(cands), cands)
	}
	if got := streamy.Data["flavor"]; got != "price_creep" {
		t.Errorf("streamy flavor = %v, want price_creep", got)
	}
	if got := streamy.Data["early_amount"]; got != "10.00" {
		t.Errorf("streamy early_amount = %v, want 10.00", got)
	}
	if got := streamy.Data["recent_amount"]; got != "13.00" {
		t.Errorf("streamy recent_amount = %v, want 13.00", got)
	}
	if got := streamy.Data["delta"]; got != "3.00" {
		t.Errorf("streamy delta = %v, want 3.00 (recent − early)", got)
	}
	if streamy.Priority != 3 {
		t.Errorf("streamy priority = %d, want 3 (creep outranks zombie)", streamy.Priority)
	}

	// Gymzo → zombie.
	gymzo, ok := byKey["subscription:gymzo"]
	if !ok {
		t.Fatal("expected a subscription candidate for gymzo")
	}
	if got := gymzo.Data["flavor"]; got != "zombie" {
		t.Errorf("gymzo flavor = %v, want zombie", got)
	}
	if gymzo.Priority != 2 {
		t.Errorf("gymzo priority = %d, want 2", gymzo.Priority)
	}

	// Category is intentionally unset by the deterministic producer.
	if got := streamy.Data["category"]; got != "" {
		t.Errorf("category = %v, want empty (classification is a deferred AI step)", got)
	}

	// A second detect run is stable — same dedupe keys, so the engine upserts
	// rather than duplicates.
	again, err := subscriptionProducer{}.Detect(ctx, q, householdID, now)
	if err != nil {
		t.Fatalf("detect (2nd): %v", err)
	}
	if len(again) != len(cands) {
		t.Errorf("second detect produced %d candidates, want %d (stable)", len(again), len(cands))
	}
}
