package reporting

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/apex42group/ledgermancy/backend/internal/db"
	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
)

// TestBuildSafeToSpend drives the calculation against a real Postgres: six
// months of $5,000 income, $1,000/month of fixed bills, and a $400 discretionary
// budget should leave $3,600 safe to spend (no goals). An open-ended goal must
// not change it — its required monthly contribution is zero.
//
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' go test ./internal/reporting/
func TestBuildSafeToSpend(t *testing.T) {
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
	incomeCat := uuid.New()
	fixedCat := uuid.New()
	discCat := uuid.New()

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, sql)
		}
	}
	exec(`INSERT INTO households (id, name) VALUES ($1, 'STS Test')`, householdID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id = $1`, householdID)
	})
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
	      VALUES ($1, $2, $3, 'x', 'Tester')`, userID, householdID, userID.String()+"@example.test")
	exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, products, status)
	      VALUES ($1, $2, $3, '\x00', '{transactions}', 'active')`, itemID, userID, itemID.String())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type)
	      VALUES ($1, $2, $3, 'Checking', 'depository')`, acctID, itemID, acctID.String())
	exec(`INSERT INTO categories (id, household_id, name, slug, is_income) VALUES ($1, $2, 'Paycheck', 'paycheck', TRUE)`, incomeCat, householdID)
	exec(`INSERT INTO categories (id, household_id, name, slug, is_fixed) VALUES ($1, $2, 'Rent', 'rent', TRUE)`, fixedCat, householdID)
	exec(`INSERT INTO categories (id, household_id, name, slug) VALUES ($1, $2, 'Dining', 'dining')`, discCat, householdID)

	tx := func(amount, date string, cat uuid.UUID) {
		exec(`INSERT INTO transactions (account_id, amount, currency, date, name, category_id, source)
		      VALUES ($1, $2, 'USD', $3, 'x', $4, 'plaid')`, acctID, amount, date, cat)
	}

	// Six prior full months (Jan–Jun): $5,000 income and $1,000 fixed each.
	for _, d := range []string{"2026-01", "2026-02", "2026-03", "2026-04", "2026-05", "2026-06"} {
		tx("-5000.00", d+"-15", incomeCat) // income stored negative
		tx("1000.00", d+"-10", fixedCat)
	}

	// A $400 discretionary budget.
	if _, err := q.UpsertBudget(ctx, dbgen.UpsertBudgetParams{
		HouseholdID: householdID, CategoryID: discCat, Amount: decimal.RequireFromString("400.00"),
		Period: "monthly",
	}); err != nil {
		t.Fatalf("UpsertBudget: %v", err)
	}

	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)

	sts, err := BuildSafeToSpend(ctx, q, householdID, now)
	if err != nil {
		t.Fatalf("BuildSafeToSpend: %v", err)
	}

	check := func(label string, got decimal.Decimal, want string) {
		t.Helper()
		if !got.Equal(decimal.RequireFromString(want)) {
			t.Errorf("%s = %s, want %s", label, got, want)
		}
	}
	check("expected_income", sts.ExpectedIncome, "5000.00")
	check("fixed_costs", sts.FixedCosts, "1000.00")
	check("budgeted_discretionary", sts.BudgetedDiscretionary, "400.00")
	check("goal_contributions", sts.GoalContributions, "0.00")
	check("safe_to_spend", sts.Amount, "3600.00")
	if sts.IncomeMonths != 6 {
		t.Errorf("income_months = %d, want 6", sts.IncomeMonths)
	}

	// An open-ended goal (no target date) requires nothing per month, so the
	// figure is unchanged — and the goal loop runs without error.
	exec(`INSERT INTO goals (household_id, scope, kind, name, target_amount)
	      VALUES ($1, 'household', 'savings', 'Rainy day', '10000.00')`, householdID)
	sts2, err := BuildSafeToSpend(ctx, q, householdID, now)
	if err != nil {
		t.Fatalf("BuildSafeToSpend (with goal): %v", err)
	}
	check("goal_contributions (open-ended)", sts2.GoalContributions, "0.00")
	check("safe_to_spend (with goal)", sts2.Amount, "3600.00")
}
