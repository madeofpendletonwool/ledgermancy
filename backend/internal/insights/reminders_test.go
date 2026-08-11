package insights

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/ai"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/config"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// TestOverdueBillProducer pins the "was due and we can't find a payment"
// reminder: it raises for a past-due occurrence with no matching transaction,
// and goes quiet once a payment lands in the obligation's category.
//
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' \
//	    go test ./internal/insights/ -run TestOverdueBillProducer
func TestOverdueBillProducer(t *testing.T) {
	ctx := context.Background()
	pool, err := db.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	q := dbgen.New(pool)

	householdID, userID, itemID := uuid.New(), uuid.New(), uuid.New()
	acctID, catID := uuid.New(), uuid.New()

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, sql)
		}
	}
	exec(`INSERT INTO households (id, name) VALUES ($1, 'Overdue Test')`, householdID)
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
	      VALUES ($1, $2, $3, 'x', 'Tester')`, userID, householdID, userID.String()+"@example.test")
	exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, products, status)
	      VALUES ($1, $2, $3, '\x00', '{transactions}', 'active')`, itemID, userID, itemID.String())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type)
	      VALUES ($1, $2, $3, 'Checking', 'depository')`, acctID, itemID, acctID.String())
	exec(`INSERT INTO categories (id, household_id, name, slug) VALUES ($1, $2, 'Electric', 'electric')`, catID, householdID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id = $1`, householdID)
	})

	// A monthly bill anchored 40 days ago: occurrences land at today-40 (outside
	// the 21-day lookback) and today-10 (inside it). The today-10 occurrence is
	// the overdue one the producer should raise.
	today := time.Now().UTC()
	anchor := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC).
		AddDate(0, 0, -40)
	ob, err := q.CreateObligation(ctx, dbgen.CreateObligationParams{
		HouseholdID: householdID, UserID: &userID, IsShared: true,
		Label: "Electric bill", Amount: decimal.RequireFromString("120.00"),
		CategoryID: &catID, IntervalCount: 1, IntervalUnit: "month",
		AnchorDate: anchor,
	})
	if err != nil {
		t.Fatalf("create obligation: %v", err)
	}

	aiOff := ai.New(config.AIConfig{})
	now := time.Now()
	if _, err := Generate(ctx, q, aiOff, householdID, now); err != nil {
		t.Fatalf("generate: %v", err)
	}

	rows, err := q.ListInsights(ctx, dbgen.ListInsightsParams{
		HouseholdID: householdID, IncludeDismissed: true, AsOf: now,
	})
	if err != nil {
		t.Fatalf("list insights: %v", err)
	}
	var raised *dbgen.Insight
	for i := range rows {
		if rows[i].Kind == "overdue_bill" {
			raised = &rows[i]
			break
		}
	}
	if raised == nil {
		t.Fatalf("expected an overdue_bill insight for the unpaid occurrence; got none")
	}
	var data map[string]any
	if err := json.Unmarshal(raised.Data, &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if data["obligation_id"] != ob.ID.String() {
		t.Errorf("obligation_id = %v, want %s", data["obligation_id"], ob.ID)
	}
	if data["amount"] != "120.00" {
		t.Errorf("amount = %v, want 120.00", data["amount"])
	}

	// Now post a matching payment: a transaction in the obligation's category,
	// within the match window, at a near-expected amount. The producer should
	// stop raising the reminder.
	due := today.AddDate(0, 0, -10)
	exec(`INSERT INTO transactions (account_id, amount, currency, date, name, category_id, source)
	      VALUES ($1, $2, 'USD', $3, 'Electric Co', $4, 'plaid')`,
		acctID, "120.00", due.Format("2006-01-02"), catID)

	// Clear the previously-raised insight so the assertion below reads the
	// post-payment generation cleanly.
	if _, err := pool.Exec(ctx, `DELETE FROM insights WHERE household_id = $1`, householdID); err != nil {
		t.Fatalf("clear insights: %v", err)
	}
	if _, err := Generate(ctx, q, aiOff, householdID, time.Now()); err != nil {
		t.Fatalf("regenerate: %v", err)
	}
	rows, err = q.ListInsights(ctx, dbgen.ListInsightsParams{
		HouseholdID: householdID, IncludeDismissed: true, AsOf: time.Now(),
	})
	if err != nil {
		t.Fatalf("relist insights: %v", err)
	}
	for _, r := range rows {
		if r.Kind == "overdue_bill" {
			t.Errorf("overdue_bill should be suppressed once a matching payment posts; still got %q", r.Title)
		}
	}
}

// TestPayoffProgressProducer pins the mortgage-principal reminder: a debt-payoff
// goal whose current monthly payment will not clear the balance by its target
// date is coached with the monthly shortfall.
func TestPayoffProgressProducer(t *testing.T) {
	ctx := context.Background()
	pool, err := db.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	q := dbgen.New(pool)

	householdID, userID, itemID := uuid.New(), uuid.New(), uuid.New()
	cardID := uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, sql)
		}
	}
	exec(`INSERT INTO households (id, name) VALUES ($1, 'Payoff Reminder Test')`, householdID)
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
	      VALUES ($1, $2, $3, 'x', 'Tester')`, userID, householdID, userID.String()+"@example.test")
	exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, products, status)
	      VALUES ($1, $2, $3, '\x00', '{liabilities}', 'active')`, itemID, userID, itemID.String())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type, subtype, current_balance)
	      VALUES ($1, $2, $3, 'Rewards Card', 'credit', 'credit card', 5000.00)`, cardID, itemID, cardID.String())
	exec(`INSERT INTO liabilities (account_id, kind, apr, balance, minimum_payment)
	      VALUES ($1, 'credit', 18.99, 5000.00, 200.00)`, cardID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id = $1`, householdID)
	})

	// Six months out: $200/month will not clear $5000 at 18.99% in time, so the
	// goal is behind and the producer should coach the shortfall.
	target := time.Now().AddDate(0, 6, 0)
	if _, err := q.CreateGoal(ctx, dbgen.CreateGoalParams{
		HouseholdID: householdID, Scope: "household", Kind: "debt_payoff",
		Name: "Clear the card", TargetAmount: decimal.RequireFromString("5000"),
		TargetDate: &target, AccountID: &cardID,
	}); err != nil {
		t.Fatalf("create payoff goal: %v", err)
	}

	aiOff := ai.New(config.AIConfig{})
	now := time.Now()
	if _, err := Generate(ctx, q, aiOff, householdID, now); err != nil {
		t.Fatalf("generate: %v", err)
	}
	rows, err := q.ListInsights(ctx, dbgen.ListInsightsParams{
		HouseholdID: householdID, IncludeDismissed: true, AsOf: now,
	})
	if err != nil {
		t.Fatalf("list insights: %v", err)
	}
	var raised *dbgen.Insight
	for i := range rows {
		if rows[i].Kind == "payoff_progress" {
			raised = &rows[i]
			break
		}
	}
	if raised == nil {
		t.Fatalf("expected a payoff_progress insight for the behind goal; got none")
	}
	var data map[string]any
	if err := json.Unmarshal(raised.Data, &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if data["status"] != "behind" {
		t.Errorf("status = %v, want behind", data["status"])
	}
	shortfall, _ := data["shortfall"].(string)
	if shortfall == "" {
		t.Errorf("expected a non-empty shortfall, got %v", data["shortfall"])
	}
}
