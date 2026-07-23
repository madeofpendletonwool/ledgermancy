package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
)

// TestReportQueriesExecute runs the aggregation queries the assistant and the
// reports pages depend on against a real Postgres with seeded data.
//
// It exists because a query can be syntactically fine to sqlc yet fail at
// execution — exactly how the recurring-charges CTE shipped referencing a
// column its subquery no longer projected. Nothing executed these queries, so
// the break only surfaced when a user asked about subscriptions. This test
// closes that gap: every query here must actually run and return sane rows.
//
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' go test ./internal/db/
func TestReportQueriesExecute(t *testing.T) {
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

	// Idempotent; ensures the schema exists regardless of test ordering.
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	q := dbgen.New(pool)

	householdID := uuid.New()
	userID := uuid.New()
	itemID := uuid.New()
	acctID := uuid.New()
	foodID := uuid.New()
	entID := uuid.New()

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed exec: %v\n%s", err, sql)
		}
	}

	exec(`INSERT INTO households (id, name) VALUES ($1, 'Report Test')`, householdID)
	t.Cleanup(func() {
		// Cascades to items, accounts, transactions and household categories.
		_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id = $1`, householdID)
	})
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
	      VALUES ($1, $2, $3, 'x', 'Tester')`, userID, householdID, userID.String()+"@example.test")
	exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, products, status)
	      VALUES ($1, $2, $3, '\x00', '{transactions}', 'active')`, itemID, userID, itemID.String())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type)
	      VALUES ($1, $2, $3, 'Checking', 'depository')`, acctID, itemID, acctID.String())
	exec(`INSERT INTO categories (id, household_id, name, slug) VALUES ($1, $2, 'Food & Drink', 'food-drink')`, foodID, householdID)
	exec(`INSERT INTO categories (id, household_id, name, slug) VALUES ($1, $2, 'Entertainment', 'entertainment')`, entID, householdID)

	addTx := func(amount, date, name, merchant, key string, cat uuid.UUID) {
		exec(`INSERT INTO transactions
		        (account_id, amount, currency, date, name, merchant_name, merchant_key, category_id, source)
		      VALUES ($1, $2, 'USD', $3, $4, $5, $6, $7, 'plaid')`,
			acctID, amount, date, name, merchant, key, cat)
	}

	// A regular monthly subscription: 4 charges, ~30-day cadence, zero spread —
	// squarely inside the recurring heuristic.
	addTx("15.00", "2026-03-01", "NETFLIX", "Netflix", "netflix", entID)
	addTx("15.00", "2026-03-31", "NETFLIX", "Netflix", "netflix", entID)
	addTx("15.00", "2026-04-30", "NETFLIX", "Netflix", "netflix", entID)
	addTx("15.00", "2026-05-30", "NETFLIX", "Netflix", "netflix", entID)
	// Three dining charges in June: the "how many times did I eat out" case.
	addTx("12.00", "2026-06-05", "CHIPOTLE", "Chipotle", "chipotle", foodID)
	addTx("15.50", "2026-06-12", "CHIPOTLE", "Chipotle", "chipotle", foodID)
	addTx("9.25", "2026-06-20", "CHIPOTLE", "Chipotle", "chipotle", foodID)

	yearStart := mustDate(t, "2026-01-01")
	yearEnd := mustDate(t, "2026-12-31")

	// 1. Recurring — the query that shipped broken. Must run and find Netflix.
	recurring, err := q.GetRecurringMerchants(ctx, dbgen.GetRecurringMerchantsParams{
		HouseholdID: householdID, UserID: userID, Date: yearStart,
	})
	if err != nil {
		t.Fatalf("GetRecurringMerchants: %v", err)
	}
	var netflix *dbgen.GetRecurringMerchantsRow
	for i := range recurring {
		if recurring[i].Merchant == "Netflix" {
			netflix = &recurring[i]
		}
	}
	if netflix == nil {
		t.Fatalf("expected Netflix in recurring merchants, got %+v", recurring)
	}
	if netflix.Occurrences != 4 {
		t.Errorf("Netflix occurrences = %d, want 4", netflix.Occurrences)
	}

	// 2. spend_by_category — the count the chat tool now surfaces.
	byCat, err := q.GetSpendingByCategory(ctx, dbgen.GetSpendingByCategoryParams{
		HouseholdID: householdID, UserID: userID, Date: yearStart, Date_2: yearEnd,
	})
	if err != nil {
		t.Fatalf("GetSpendingByCategory: %v", err)
	}
	foodCount := int64(-1)
	for _, c := range byCat {
		if c.CategoryName == "Food & Drink" {
			foodCount = c.TransactionCount
			if !c.Total.Equal(decimal.RequireFromString("36.75")) {
				t.Errorf("Food & Drink total = %s, want 36.75", c.Total)
			}
		}
	}
	if foodCount != 3 {
		t.Errorf("Food & Drink count = %d, want 3", foodCount)
	}

	// 3. list_transactions backing — count/total exact and reconciling with
	//    spend_by_category; matching by name and by slug; limit truncates the
	//    list but not the count.
	foodName := "Food & Drink"
	sum, err := q.SumFilteredTransactions(ctx, dbgen.SumFilteredTransactionsParams{
		HouseholdID: householdID, UserID: userID, Date: yearStart, Date_2: yearEnd, Category: &foodName,
	})
	if err != nil {
		t.Fatalf("SumFilteredTransactions (name): %v", err)
	}
	if sum.TransactionCount != 3 {
		t.Errorf("filtered count (name) = %d, want 3", sum.TransactionCount)
	}
	if !sum.Total.Equal(decimal.RequireFromString("36.75")) {
		t.Errorf("filtered total = %s, want 36.75", sum.Total)
	}
	if sum.TransactionCount != foodCount {
		t.Errorf("filtered count %d does not reconcile with spend_by_category count %d",
			sum.TransactionCount, foodCount)
	}

	slug := "food-drink"
	sumSlug, err := q.SumFilteredTransactions(ctx, dbgen.SumFilteredTransactionsParams{
		HouseholdID: householdID, UserID: userID, Date: yearStart, Date_2: yearEnd, Category: &slug,
	})
	if err != nil {
		t.Fatalf("SumFilteredTransactions (slug): %v", err)
	}
	if sumSlug.TransactionCount != 3 {
		t.Errorf("filtered count (slug) = %d, want 3", sumSlug.TransactionCount)
	}

	list, err := q.ListFilteredTransactions(ctx, dbgen.ListFilteredTransactionsParams{
		HouseholdID: householdID, UserID: userID, Date: yearStart, Date_2: yearEnd, Category: &foodName, Lim: 2,
	})
	if err != nil {
		t.Fatalf("ListFilteredTransactions: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("listed rows = %d, want 2 (limit)", len(list))
	}
	for _, r := range list {
		if r.CategoryName != "Food & Drink" {
			t.Errorf("row category = %q, want Food & Drink", r.CategoryName)
		}
	}

	// 4. The remaining aggregation tools must at least execute cleanly.
	if _, err := q.GetSpendingSummary(ctx, dbgen.GetSpendingSummaryParams{
		HouseholdID: householdID, UserID: userID, Date: yearStart, Date_2: yearEnd,
	}); err != nil {
		t.Errorf("GetSpendingSummary: %v", err)
	}
	if _, err := q.GetMonthlyTrend(ctx, dbgen.GetMonthlyTrendParams{
		HouseholdID: householdID, UserID: userID, Date: yearStart, Date_2: yearEnd,
	}); err != nil {
		t.Errorf("GetMonthlyTrend: %v", err)
	}
	if _, err := q.GetCategoryAverages(ctx, dbgen.GetCategoryAveragesParams{
		HouseholdID: householdID, UserID: userID, Date: yearStart, Date_2: yearEnd,
	}); err != nil {
		t.Errorf("GetCategoryAverages: %v", err)
	}
	if _, err := q.GetSpendingByDay(ctx, dbgen.GetSpendingByDayParams{
		HouseholdID: householdID, UserID: userID, Date: yearStart, Date_2: yearEnd,
	}); err != nil {
		t.Errorf("GetSpendingByDay: %v", err)
	}
	if _, err := q.GetTopMerchants(ctx, dbgen.GetTopMerchantsParams{
		HouseholdID: householdID, UserID: userID, Date: yearStart, Date_2: yearEnd, Limit: 10,
	}); err != nil {
		t.Errorf("GetTopMerchants: %v", err)
	}
}

func mustDate(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatalf("parse date %q: %v", s, err)
	}
	return d
}
