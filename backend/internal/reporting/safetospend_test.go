package reporting

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
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

	// With no obligations known, the bill-aware view must be identical to the
	// ordinary one rather than a different number for the same facts.
	check("upcoming_obligations (none)", sts2.UpcomingObligations, "0.00")
	check("fixed_costs_after_bills (none)", sts2.FixedCostsAfterBills, "1000.00")
	check("safe_to_spend_after_bills (none)", sts2.AmountAfterBills, "3600.00")
	if sts2.ObligationCoverage != 0 {
		t.Errorf("obligation_coverage = %d, want 0", sts2.ObligationCoverage)
	}
}

// TestSafeToSpendDoesNotDoubleCountBills is the guard on the riskiest part of
// the bill calendar.
//
// The fixed-cost input is a SIX-MONTH TRAILING AVERAGE of what the household
// actually paid, so the rent is already inside it. Adding the upcoming rent
// obligation on top would subtract the same $1,000 twice and hand the user a
// figure $1,000 more pessimistic than the truth. The rule this asserts: a fixed
// cost counts exactly once, as its obligations where they are known and as the
// trailing typical figure only where they are not.
//
// The household here is deliberately the same shape as TestBuildSafeToSpend —
// $5,000 income, $1,000 of rent, a $400 dining budget — so the two figures can
// be compared directly.
func TestSafeToSpendDoesNotDoubleCountBills(t *testing.T) {
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
	rentCat := uuid.New()
	insuranceCat := uuid.New()
	diningCat := uuid.New()

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, sql)
		}
	}
	exec(`INSERT INTO households (id, name) VALUES ($1, 'Double count')`, householdID)
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
	exec(`INSERT INTO categories (id, household_id, name, slug, is_fixed) VALUES ($1, $2, 'Rent', 'rent', TRUE)`, rentCat, householdID)
	exec(`INSERT INTO categories (id, household_id, name, slug, is_fixed) VALUES ($1, $2, 'Insurance', 'insurance', TRUE)`, insuranceCat, householdID)
	exec(`INSERT INTO categories (id, household_id, name, slug) VALUES ($1, $2, 'Dining', 'dining')`, diningCat, householdID)

	tx := func(amount, date string, cat uuid.UUID) {
		exec(`INSERT INTO transactions (account_id, amount, currency, date, name, category_id, source)
		      VALUES ($1, $2, 'USD', $3, 'x', $4, 'plaid')`, acctID, amount, date, cat)
	}
	// Six prior full months: $5,000 income, $1,000 rent and $200 insurance each.
	// Trailing fixed is therefore $1,200/month across two fixed categories.
	for _, d := range []string{"2026-01", "2026-02", "2026-03", "2026-04", "2026-05", "2026-06"} {
		tx("-5000.00", d+"-15", incomeCat)
		tx("1000.00", d+"-10", rentCat)
		tx("200.00", d+"-12", insuranceCat)
	}

	if _, err := q.UpsertBudget(ctx, dbgen.UpsertBudgetParams{
		HouseholdID: householdID, CategoryID: diningCat,
		Amount: decimal.RequireFromString("400.00"), Period: "monthly",
	}); err != nil {
		t.Fatalf("UpsertBudget: %v", err)
	}

	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)

	// Baseline, no obligations: 5000 − 1200 − 400 = 3400.
	base, err := BuildSafeToSpend(ctx, q, householdID, now)
	if err != nil {
		t.Fatalf("BuildSafeToSpend: %v", err)
	}
	check := func(label string, got decimal.Decimal, want string) {
		t.Helper()
		if !got.Equal(decimal.RequireFromString(want)) {
			t.Errorf("%s = %s, want %s", label, got, want)
		}
	}
	check("fixed_costs (baseline)", base.FixedCosts, "1200.00")
	check("safe_to_spend (baseline)", base.Amount, "3400.00")

	// Now the calendar knows about three bills, all due in the rest of July:
	//
	//   rent      $1,000 — a FIXED category that IS in the trailing average.
	//                      It must REPLACE the $1,000 trailing rent figure,
	//                      not stack on top of it.
	//   dining      $60  — a DISCRETIONARY category. That money is already
	//                      inside the $400 budget envelope, so it must not
	//                      touch the fixed component at all.
	//   dues       $500  — no category, and no transaction history: exactly the
	//                      annual-renewal case only manual entry can capture.
	//                      Nothing to displace, so it counts in full.
	obligation := func(label, amount, anchor string, cat *uuid.UUID) {
		exec(`INSERT INTO recurring_obligations
		      (household_id, user_id, label, amount, category_id, interval_count, interval_unit, anchor_date)
		      VALUES ($1, $2, $3, $4, $5, 1, 'month', $6)`,
			householdID, userID, label, amount, cat, anchor)
	}
	obligation("Rent", "1000.00", "2026-07-25", &rentCat)
	obligation("Takeaway", "60.00", "2026-07-20", &diningCat)
	obligation("Club dues", "500.00", "2026-07-28", nil)

	sts, err := BuildSafeToSpend(ctx, q, householdID, now)
	if err != nil {
		t.Fatalf("BuildSafeToSpend (with obligations): %v", err)
	}

	// The original figure must be untouched. The Budgets page and the chat tool
	// both read it, and redefining it under them is how surfaces start
	// disagreeing about the same number.
	check("fixed_costs (unchanged)", sts.FixedCosts, "1200.00")
	check("safe_to_spend (unchanged)", sts.Amount, "3400.00")

	// Every bill still to fall due this month, whatever its category.
	check("upcoming_obligations", sts.UpcomingObligations, "1560.00")

	// The rule, worked through:
	//   rent      → covered fixed category: 1000 in, its 1000 trailing out.
	//   insurance → uncovered fixed category: keeps its 200 trailing figure.
	//   dining    → discretionary: skipped entirely (already budgeted).
	//   dues      → uncategorised: 500 in, displacing nothing.
	// So 1000 + 200 + 500 = 1700 — NOT 1200 + 1560 = 2760, which is what naive
	// addition would produce.
	check("fixed_costs_after_bills", sts.FixedCostsAfterBills, "1700.00")

	// 5000 − 1700 − 400 = 2900. The whole difference from the ordinary $3,400 is
	// the club dues: money genuinely leaving this month that no trailing average
	// could have seen. The rent, which both views know about, moves the figure by
	// nothing at all — which is the point.
	check("safe_to_spend_after_bills", sts.AmountAfterBills, "2900.00")

	if sts.ObligationCoverage != 1 {
		t.Errorf("obligation_coverage = %d, want 1 (rent only)", sts.ObligationCoverage)
	}
}
