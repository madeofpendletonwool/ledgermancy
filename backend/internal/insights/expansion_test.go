package insights

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/apex42group/ledgermancy/backend/internal/db"
	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
)

// expansionFixture is a seeded household with an income and a spending category,
// plus helpers to add income/spending transactions. Each test gets its own
// household so their data never collides.
type expansionFixture struct {
	q           *dbgen.Queries
	householdID uuid.UUID
	incomeCat   uuid.UUID
	spendCat    uuid.UUID
	acctID      uuid.UUID
	exec        func(sql string, args ...any)
}

func newExpansionFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) expansionFixture {
	t.Helper()
	householdID := uuid.New()
	userID := uuid.New()
	itemID := uuid.New()
	acctID := uuid.New()
	incomeCat := uuid.New()
	spendCat := uuid.New()

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, sql)
		}
	}
	exec(`INSERT INTO households (id, name) VALUES ($1, 'Expansion Test')`, householdID)
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
	exec(`INSERT INTO categories (id, household_id, name, slug) VALUES ($1, $2, 'General', 'general')`, spendCat, householdID)

	return expansionFixture{
		q: dbgen.New(pool), householdID: householdID,
		incomeCat: incomeCat, spendCat: spendCat, acctID: acctID, exec: exec,
	}
}

// income records money in — stored as a negative amount in an is_income
// category, so GetMonthlyTrend's SUM(-amount) reads it as positive income.
func (f expansionFixture) income(amount, date string) {
	f.exec(`INSERT INTO transactions (account_id, amount, currency, date, name, category_id, source)
	        VALUES ($1, $2, 'USD', $3, 'Paycheck', $4, 'plaid')`, f.acctID, "-"+amount, date, f.incomeCat)
}

func (f expansionFixture) spend(amount, date, merchant string) {
	f.exec(`INSERT INTO transactions (account_id, amount, currency, date, name, merchant_name, category_id, source)
	        VALUES ($1, $2, 'USD', $3, $4, $4, $5, 'plaid')`, f.acctID, amount, date, merchant, f.spendCat)
}

func expansionPool(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return ctx, pool
}

func only(t *testing.T, cands []Candidate, err error, kind string) Candidate {
	t.Helper()
	if err != nil {
		t.Fatalf("%s detect: %v", kind, err)
	}
	if len(cands) != 1 {
		t.Fatalf("%s: expected 1 candidate, got %d: %+v", kind, len(cands), cands)
	}
	return cands[0]
}

// income_change: last completed month's income well above the recent average
// surfaces as an "up" change; savings_milestone: the same month's savings rate
// clears a round band it was below the month before.
func TestIncomeChangeAndSavingsMilestone(t *testing.T) {
	ctx, pool := expansionPool(t)
	f := newExpansionFixture(t, ctx, pool)

	// April–June income $5,000 (baseline); July income $8,000 (the jump).
	f.income("5000.00", "2026-04-15")
	f.income("5000.00", "2026-05-15")
	f.income("5000.00", "2026-06-15")
	f.income("8000.00", "2026-07-15")
	// June spend $4,600 → 8% saved (below 10%); July spend $4,000 → 50% saved.
	f.spend("4600.00", "2026-06-20", "Rent")
	f.spend("4000.00", "2026-07-20", "Rent")

	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)

	cands, err := incomeChangeProducer{}.Detect(ctx, f.q, f.householdID, now)
	inc := only(t, cands, err, "income_change")
	if inc.Data["direction"] != "up" {
		t.Errorf("income_change direction = %v, want up", inc.Data["direction"])
	}
	if inc.Data["delta"] != "3000.00" {
		t.Errorf("income_change delta = %v, want 3000.00", inc.Data["delta"])
	}
	if inc.Data["month"] != "2026-07" {
		t.Errorf("income_change month = %v, want 2026-07", inc.Data["month"])
	}
	if inc.Priority < insightPushMinPriorityForTest {
		t.Errorf("income_change priority = %d, want >= %d (should push)", inc.Priority, insightPushMinPriorityForTest)
	}

	cands, err = savingsMilestoneProducer{}.Detect(ctx, f.q, f.householdID, now)
	milestone := only(t, cands, err, "savings_milestone")
	if milestone.Data["band"] != 50 {
		t.Errorf("savings_milestone band = %v, want 50", milestone.Data["band"])
	}
	if milestone.Data["savings_rate"] != "50.0" {
		t.Errorf("savings_milestone savings_rate = %v, want 50.0", milestone.Data["savings_rate"])
	}
}

// large_transaction: a charge far above the household's typical purchase (and
// over the floor) surfaces; an ordinary one does not.
func TestLargeTransactionProducer(t *testing.T) {
	ctx, pool := expansionPool(t)
	f := newExpansionFixture(t, ctx, pool)

	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

	// A baseline of small purchases over the last few months (avg ≈ $30).
	for _, d := range []string{"2026-03-05", "2026-03-18", "2026-04-02", "2026-04-20", "2026-05-06", "2026-05-19", "2026-06-03", "2026-06-21", "2026-07-02", "2026-07-08"} {
		f.spend("30.00", d, "Corner Store")
	}
	// One outsized recent charge (flagged) and one ordinary recent charge (not).
	f.spend("900.00", "2026-07-15", "Big Appliance Co")
	f.spend("60.00", "2026-07-16", "Corner Store")

	cands, err := largeTransactionProducer{}.Detect(ctx, f.q, f.householdID, now)
	got := only(t, cands, err, "large_transaction")
	if got.Data["merchant"] != "Big Appliance Co" {
		t.Errorf("large_transaction merchant = %v, want Big Appliance Co", got.Data["merchant"])
	}
	if got.Data["amount"] != "900.00" {
		t.Errorf("large_transaction amount = %v, want 900.00", got.Data["amount"])
	}
	if got.Priority < insightPushMinPriorityForTest {
		t.Errorf("large_transaction priority = %d, want >= %d (should push)", got.Priority, insightPushMinPriorityForTest)
	}
}

// month_end_projection: a spending run-rate that extrapolates past typical
// income surfaces a mid-month heads-up.
func TestMonthEndProjectionProducer(t *testing.T) {
	ctx, pool := expansionPool(t)
	f := newExpansionFixture(t, ctx, pool)

	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

	// Six prior months of $5,000 income sets the "typical income" baseline.
	for _, d := range []string{"2026-01-15", "2026-02-15", "2026-03-15", "2026-04-15", "2026-05-15", "2026-06-15"} {
		f.income("5000.00", d)
	}
	// $4,000 spent by the 20th → straight-line to ~$6,200 for the month, past the
	// $5,000 typical income → projected shortfall.
	f.spend("2500.00", "2026-07-05", "Rent")
	f.spend("1500.00", "2026-07-18", "Furniture")

	cands, err := monthEndProjectionProducer{}.Detect(ctx, f.q, f.householdID, now)
	got := only(t, cands, err, "month_end_projection")
	if got.Data["expected_income"] != "5000.00" {
		t.Errorf("expected_income = %v, want 5000.00", got.Data["expected_income"])
	}
	// 4000 * 31 / 20 = 6200.00
	if got.Data["projected_spending"] != "6200.00" {
		t.Errorf("projected_spending = %v, want 6200.00", got.Data["projected_spending"])
	}
	if got.Priority != 5 {
		t.Errorf("month_end_projection priority = %d, want 5 (projected overspend)", got.Priority)
	}
}

// insightPushMinPriorityForTest mirrors the jobs-layer push threshold (4) so the
// tests assert the new "urgent" producers actually clear it without importing
// the jobs package.
const insightPushMinPriorityForTest = 4

// budget_trend: a monthly budget exceeded every one of the last three completed
// months surfaces; a budget met in one of them does not.
func TestBudgetTrendProducer(t *testing.T) {
	ctx, pool := expansionPool(t)
	f := newExpansionFixture(t, ctx, pool)

	// A $100/mo budget on the discretionary category.
	if _, err := f.q.UpsertBudget(ctx, dbgen.UpsertBudgetParams{
		HouseholdID: f.householdID, CategoryID: f.spendCat, Amount: decimal.RequireFromString("100.00"),
		Period: "monthly",
	}); err != nil {
		t.Fatalf("UpsertBudget: %v", err)
	}

	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	// Last three completed months (May, June, July) each well over $100.
	f.spend("160.00", "2026-05-10", "Dining")
	f.spend("140.00", "2026-06-10", "Dining")
	f.spend("175.00", "2026-07-10", "Dining")

	cands, err := budgetTrendProducer{}.Detect(ctx, f.q, f.householdID, now)
	got := only(t, cands, err, "budget_trend")
	if got.Data["category_slug"] != "general" {
		t.Errorf("category_slug = %v, want general", got.Data["category_slug"])
	}
	// Overages: 60 + 40 + 75 = 175, average 58.33.
	if got.Data["average_over"] != "58.33" {
		t.Errorf("average_over = %v, want 58.33", got.Data["average_over"])
	}

	// If one month comes in under budget, the streak breaks and nothing fires.
	f2 := newExpansionFixture(t, ctx, pool)
	if _, err := f2.q.UpsertBudget(ctx, dbgen.UpsertBudgetParams{
		HouseholdID: f2.householdID, CategoryID: f2.spendCat, Amount: decimal.RequireFromString("100.00"),
		Period: "monthly",
	}); err != nil {
		t.Fatalf("UpsertBudget: %v", err)
	}
	f2.spend("160.00", "2026-05-10", "Dining")
	f2.spend("40.00", "2026-06-10", "Dining") // under budget → breaks the streak
	f2.spend("175.00", "2026-07-10", "Dining")
	cands2, err := budgetTrendProducer{}.Detect(ctx, f2.q, f2.householdID, now)
	if err != nil {
		t.Fatalf("budget_trend detect (broken streak): %v", err)
	}
	if len(cands2) != 0 {
		t.Errorf("expected no candidates when a month is under budget, got %d", len(cands2))
	}
}
