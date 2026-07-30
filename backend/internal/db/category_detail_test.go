package db

import (
	"testing"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/merchants"
)

// The category detail queries answer the same questions about a category that the
// merchant detail queries answer about a merchant, and they have to reconcile with
// the Spending page the same way: a category's headline total must equal its row in
// GetSpendingByCategory, or the two pages disagree about the same money.

func TestCategorySummaryReconcilesWithSpendingByCategory(t *testing.T) {
	ctx, f := setupMerchantFixture(t)

	from, to := mustDate(t, "2026-01-01"), mustDate(t, "2026-12-31")

	byCategory, err := f.q.GetSpendingByCategory(ctx, dbgen.GetSpendingByCategoryParams{
		HouseholdID: f.householdID, UserID: f.userID, Date: from, Date_2: to,
	})
	if err != nil {
		t.Fatalf("GetSpendingByCategory: %v", err)
	}
	var want dbgen.GetSpendingByCategoryRow
	for _, row := range byCategory {
		if row.CategoryID == f.entID {
			want = row
		}
	}
	if want.CategoryName == "" {
		t.Fatal("Entertainment missing from the by-category report")
	}

	got, err := f.q.GetCategorySummary(ctx, dbgen.GetCategorySummaryParams{
		HouseholdID: f.householdID, UserID: f.userID,
		Date: from, Date_2: to, CategoryID: &f.entID,
	})
	if err != nil {
		t.Fatalf("GetCategorySummary: %v", err)
	}
	if !got.Total.Equal(want.Total) {
		t.Errorf("category detail total = %s, the by-category report said %s", got.Total, want.Total)
	}
	if got.TransactionCount != want.TransactionCount {
		t.Errorf("category detail count = %d, the by-category report said %d",
			got.TransactionCount, want.TransactionCount)
	}
	// The fixture's six subscription charges all land here.
	if got.TransactionCount != 6 {
		t.Errorf("count = %d, want the fixture's 6 charges", got.TransactionCount)
	}
	if got.FirstSeen != "2026-01-05" || got.LastSeen != "2026-06-05" {
		t.Errorf("span = %s..%s, want 2026-01-05..2026-06-05", got.FirstSeen, got.LastSeen)
	}
}

// An empty category renders as empty rather than failing: MIN/MAX over no rows are
// NULL, and the query coalesces them to the empty-string sentinel emptyToNil reads.
func TestCategorySummaryOnEmptyCategoryIsZeroed(t *testing.T) {
	ctx, f := setupMerchantFixture(t)

	got, err := f.q.GetCategorySummary(ctx, dbgen.GetCategorySummaryParams{
		HouseholdID: f.householdID, UserID: f.userID,
		// Food & Drink has no charges in the base fixture.
		Date: mustDate(t, "2026-01-01"), Date_2: mustDate(t, "2026-12-31"),
		CategoryID: &f.foodID,
	})
	if err != nil {
		t.Fatalf("GetCategorySummary on an empty category: %v", err)
	}
	if !got.Total.IsZero() || got.TransactionCount != 0 {
		t.Errorf("empty category reported total %s over %d charges", got.Total, got.TransactionCount)
	}
	if got.FirstSeen != "" || got.LastSeen != "" {
		t.Errorf("empty category reported a date span %q..%q", got.FirstSeen, got.LastSeen)
	}
}

// The monthly series is what MonthlyBars draws. Gaps are left out rather than
// zero-filled, matching the merchant query, because the chart re-expands the range.
func TestCategoryMonthlySpendCoversEachChargedMonth(t *testing.T) {
	ctx, f := setupMerchantFixture(t)

	months, err := f.q.GetCategoryMonthlySpend(ctx, dbgen.GetCategoryMonthlySpendParams{
		HouseholdID: f.householdID, UserID: f.userID,
		Date: mustDate(t, "2026-01-01"), Date_2: mustDate(t, "2026-12-31"),
		CategoryID: &f.entID,
	})
	if err != nil {
		t.Fatalf("GetCategoryMonthlySpend: %v", err)
	}
	// Six charges, one per month January through June.
	if len(months) != 6 {
		t.Fatalf("want six charged months, got %d: %+v", len(months), months)
	}
	if got := months[0].Month.Format("2006-01-02"); got != "2026-01-01" {
		t.Errorf("first month = %s, want 2026-01-01 (truncated to the 1st)", got)
	}
	for _, m := range months {
		if m.TransactionCount != 1 {
			t.Errorf("%s has %d charges, want 1", m.Month.Format("2006-01"), m.TransactionCount)
		}
	}
}

// A category page whose merchants were dead text would answer "how much" and stop.
// These rows carry the RESOLVED key, so a grouped merchant is ONE row that links to
// its own detail page — the same canonicalisation GetTopMerchants applies.
func TestTopMerchantsInCategoryGroupsByResolvedKey(t *testing.T) {
	ctx, f := setupMerchantFixture(t)

	from, to := mustDate(t, "2026-01-01"), mustDate(t, "2026-12-31")
	inCategory := func() []dbgen.GetTopMerchantsInCategoryRow {
		t.Helper()
		rows, err := f.q.GetTopMerchantsInCategory(ctx, dbgen.GetTopMerchantsInCategoryParams{
			HouseholdID: f.householdID, UserID: f.userID,
			Date: from, Date_2: to, CategoryID: &f.entID, Lim: 20,
		})
		if err != nil {
			t.Fatalf("GetTopMerchantsInCategory: %v", err)
		}
		return rows
	}

	// Before the merge the biller's two descriptors are two merchants.
	if got := inCategory(); len(got) != 2 {
		t.Fatalf("want the two descriptors as two rows before merging, got %d: %+v", len(got), got)
	}

	entityID, _, err := merchants.Merge(ctx, f.q, f.householdID,
		[]string{"acme plus", "acmeplus bill"}, "Acme Plus", nil)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}

	after := inCategory()
	if len(after) != 1 {
		t.Fatalf("want one row after merging, got %d: %+v", len(after), after)
	}
	if after[0].MerchantKey != entityID.String() {
		t.Errorf("merchant key = %q, want the entity id %q — the row could not link otherwise",
			after[0].MerchantKey, entityID)
	}
	if after[0].Merchant != "Acme Plus" {
		t.Errorf("merchant name = %q, want the canonical name", after[0].Merchant)
	}
	if after[0].TransactionCount != 6 {
		t.Errorf("count = %d, want all 6 charges of the grouped merchant", after[0].TransactionCount)
	}

	// The merchants in a category must add up to the category's own total, or the
	// "where it goes" list contradicts the tile above it.
	summary, err := f.q.GetCategorySummary(ctx, dbgen.GetCategorySummaryParams{
		HouseholdID: f.householdID, UserID: f.userID,
		Date: from, Date_2: to, CategoryID: &f.entID,
	})
	if err != nil {
		t.Fatalf("GetCategorySummary: %v", err)
	}
	if !after[0].Total.Equal(summary.Total) {
		t.Errorf("the category's only merchant totals %s but the category totals %s",
			after[0].Total, summary.Total)
	}
}

// Each charge on a category page links back to its merchant, which is the reverse
// of the merchant page's category column — so the two pages are mutually navigable.
func TestListCategoryTransactionsCarriesTheResolvedMerchant(t *testing.T) {
	ctx, f := setupMerchantFixture(t)

	entityID, _, err := merchants.Merge(ctx, f.q, f.householdID,
		[]string{"acme plus", "acmeplus bill"}, "Acme Plus", nil)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}

	txns, err := f.q.ListCategoryTransactions(ctx, dbgen.ListCategoryTransactionsParams{
		HouseholdID: f.householdID, UserID: f.userID,
		Date: mustDate(t, "2026-01-01"), Date_2: mustDate(t, "2026-12-31"),
		CategoryID: &f.entID, Lim: 500,
	})
	if err != nil {
		t.Fatalf("ListCategoryTransactions: %v", err)
	}
	if len(txns) != 6 {
		t.Fatalf("want the fixture's 6 charges, got %d", len(txns))
	}
	// Newest first, matching the merchant page's charge list.
	if got := txns[0].Date.Format("2006-01-02"); got != "2026-06-05" {
		t.Errorf("first row is %s, want the newest charge 2026-06-05", got)
	}
	for _, txn := range txns {
		if txn.ResolvedMerchantKey != entityID.String() {
			t.Errorf("charge %q carries key %q, want the entity id %q",
				txn.Descriptor, txn.ResolvedMerchantKey, entityID)
		}
		// The display name is the canonical one, while the descriptor keeps the
		// bank's text — the same split the merchant page's charge table shows.
		if txn.Merchant != "Acme Plus" {
			t.Errorf("merchant = %q, want the canonical name", txn.Merchant)
		}
		if txn.Descriptor == "Acme Plus" && txn.Date.Format("2006-01") == "2026-02" {
			t.Errorf("descriptor was canonicalised away: %+v", txn)
		}
	}
}

// A system category (household_id NULL) is readable by every household. Guarding
// the lookup like an edit would leave the built-in categories — where most
// spending lands — as the only ones with no detail page.
func TestGetCategoryByIDAllowsSystemAndOwnCategories(t *testing.T) {
	ctx, f := setupMerchantFixture(t)

	own, err := f.q.GetCategoryByID(ctx, dbgen.GetCategoryByIDParams{
		ID: f.entID, HouseholdID: &f.householdID,
	})
	if err != nil {
		t.Fatalf("GetCategoryByID on the household's own category: %v", err)
	}
	if own.Name != "Entertainment" || own.HouseholdID == nil {
		t.Errorf("got %+v, want the household's Entertainment", own)
	}

	// A seeded system category.
	system, err := f.q.GetCategoryBySlug(ctx, dbgen.GetCategoryBySlugParams{
		Slug: "groceries", HouseholdID: &f.householdID,
	})
	if err != nil {
		t.Fatalf("GetCategoryBySlug(groceries): %v", err)
	}
	if system.HouseholdID != nil {
		t.Skip("groceries is not a system category in this schema")
	}
	got, err := f.q.GetCategoryByID(ctx, dbgen.GetCategoryByIDParams{
		ID: system.ID, HouseholdID: &f.householdID,
	})
	if err != nil {
		t.Fatalf("GetCategoryByID on a system category: %v", err)
	}
	if got.ID != system.ID {
		t.Errorf("got %v, want the system category %v", got.ID, system.ID)
	}
}
