package db

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
)

// TestBudgetRolloverProgress checks the inputs GetBudgetProgress hands the
// envelope math: for a rollover budget started in May, viewed in July, the query
// must report the spend since May (prior_spent), the current-month spend, and
// May as the rollover_start — from which the handler derives the carried
// balance. Seeds: $200/mo budget, $150 spent in May, $100 in June, $80 in July.
//
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' go test ./internal/db/
func TestBudgetRolloverProgress(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := Migrate(ctx, pool); err != nil {
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
	exec(`INSERT INTO households (id, name) VALUES ($1, 'Rollover Test')`, householdID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id = $1`, householdID)
	})
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
	      VALUES ($1, $2, $3, 'x', 'Tester')`, userID, householdID, userID.String()+"@example.test")
	exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, products, status)
	      VALUES ($1, $2, $3, '\x00', '{transactions}', 'active')`, itemID, userID, itemID.String())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type)
	      VALUES ($1, $2, $3, 'Checking', 'depository')`, acctID, itemID, acctID.String())
	exec(`INSERT INTO categories (id, household_id, name, slug) VALUES ($1, $2, 'Fun', 'fun')`, catID, householdID)

	// A rollover budget of $200/mo, effective from May 2026.
	exec(`INSERT INTO budgets (household_id, category_id, amount, owner_scope, period, rollover, effective_from)
	      VALUES ($1, $2, '200.00', 'household', 'monthly', TRUE, '2026-05-01')`, householdID, catID)

	spend := func(amount, date string) {
		exec(`INSERT INTO transactions (account_id, amount, currency, date, name, category_id, source)
		      VALUES ($1, $2, 'USD', $3, 'x', $4, 'plaid')`, acctID, amount, date, catID)
	}
	spend("150.00", "2026-05-20")
	spend("100.00", "2026-06-20")
	spend("80.00", "2026-07-10")

	from := mustDate(t, "2026-07-01")
	to := mustDate(t, "2026-07-31")

	rows, err := q.GetBudgetProgress(ctx, dbgen.GetBudgetProgressParams{
		HouseholdID: householdID, UserID: userID,
		WindowStart: from, WindowEnd: to, Ref: mustDate(t, "2026-07-15"),
	})
	if err != nil {
		t.Fatalf("GetBudgetProgress: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 budget, got %d", len(rows))
	}
	b := rows[0]

	if !b.Rollover {
		t.Error("rollover flag not returned true")
	}
	if !b.Spent.Equal(decimal.RequireFromString("80.00")) {
		t.Errorf("spent (July) = %s, want 80.00", b.Spent)
	}
	if !b.PriorSpent.Equal(decimal.RequireFromString("250.00")) {
		t.Errorf("prior_spent (May+June) = %s, want 250.00", b.PriorSpent)
	}
	if got := b.RolloverStart.Format("2006-01"); got != "2026-05" {
		t.Errorf("rollover_start = %s, want 2026-05", got)
	}

	// Envelope math the handler applies: months May→July inclusive = 3,
	// carryover = 200×(3−1) − 250 = 150, available = 200 + 150 = 350.
	months := (from.Year()-2026)*12 + int(from.Month()) - 5 + 1
	carryover := b.Budgeted.Mul(decimal.NewFromInt(int64(months - 1))).Sub(b.PriorSpent)
	if !carryover.Equal(decimal.RequireFromString("150.00")) {
		t.Errorf("derived carryover = %s, want 150.00", carryover)
	}
}

// TestBudgetWeeklyWindow checks that a weekly budget measures spend against the
// current ISO week of the reference date, ignoring the selected month window and
// last week's spend.
func TestBudgetWeeklyWindow(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := Migrate(ctx, pool); err != nil {
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
	exec(`INSERT INTO households (id, name) VALUES ($1, 'Weekly Test')`, householdID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id = $1`, householdID)
	})
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
	      VALUES ($1, $2, $3, 'x', 'Tester')`, userID, householdID, userID.String()+"@example.test")
	exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, products, status)
	      VALUES ($1, $2, $3, '\x00', '{transactions}', 'active')`, itemID, userID, itemID.String())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type)
	      VALUES ($1, $2, $3, 'Checking', 'depository')`, acctID, itemID, acctID.String())
	exec(`INSERT INTO categories (id, household_id, name, slug) VALUES ($1, $2, 'Coffee', 'coffee')`, catID, householdID)
	exec(`INSERT INTO budgets (household_id, category_id, amount, owner_scope, period)
	      VALUES ($1, $2, '100.00', 'household', 'weekly')`, householdID, catID)

	spend := func(amount, date string) {
		exec(`INSERT INTO transactions (account_id, amount, currency, date, name, category_id, source)
		      VALUES ($1, $2, 'USD', $3, 'x', $4, 'plaid')`, acctID, amount, date, catID)
	}
	// Ref is Wed 2026-07-15; its ISO week is Mon 2026-07-13 .. Sun 2026-07-19.
	spend("30.00", "2026-07-14") // this week — counts
	spend("50.00", "2026-07-06") // last week — must NOT count

	rows, err := q.GetBudgetProgress(ctx, dbgen.GetBudgetProgressParams{
		HouseholdID: householdID, UserID: userID,
		// A month window that would wrongly catch last week's charge if the period
		// were ignored — proving the weekly window overrides it.
		WindowStart: mustDate(t, "2026-07-01"), WindowEnd: mustDate(t, "2026-07-31"),
		Ref: mustDate(t, "2026-07-15"),
	})
	if err != nil {
		t.Fatalf("GetBudgetProgress: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 budget, got %d", len(rows))
	}
	b := rows[0]
	if b.Period != "weekly" {
		t.Errorf("period = %s, want weekly", b.Period)
	}
	if !b.Spent.Equal(decimal.RequireFromString("30.00")) {
		t.Errorf("spent = %s, want 30.00 (only this week's charge)", b.Spent)
	}
	if got := b.PeriodStart.Format("2006-01-02"); got != "2026-07-13" {
		t.Errorf("period_start = %s, want 2026-07-13 (Monday)", got)
	}
	if got := b.PeriodEnd.Format("2006-01-02"); got != "2026-07-19" {
		t.Errorf("period_end = %s, want 2026-07-19 (Sunday)", got)
	}
}
