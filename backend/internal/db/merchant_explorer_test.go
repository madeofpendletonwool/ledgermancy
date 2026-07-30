package db

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/merchants"
)

// The merchant explorer's contract, in four parts: it agrees with the top-merchant
// report it sits beside, a search needle never truncates a merchant's total, "new"
// means new, and a merchant with no prior activity reads as zero rather than null.
//
// The search assertion is the one that matters most. The obvious way to write the
// query — filter rows by descriptor, then aggregate — passes every other test here
// and still shows a grouped merchant carrying only the matching fragment's spend,
// which contradicts the page the row links to.

// explore runs ListMerchantSpend over the fixture's year with an optional needle.
func explore(t *testing.T, ctx context.Context, f *merchantFixture, search *string) []dbgen.ListMerchantSpendRow {
	t.Helper()
	rows, err := f.q.ListMerchantSpend(ctx, dbgen.ListMerchantSpendParams{
		HouseholdID: f.householdID,
		UserID:      f.userID,
		Date:        mustDate(t, "2026-01-01"),
		Date_2:      mustDate(t, "2026-12-31"),
		PriorFrom:   mustDate(t, "2025-01-01"),
		PriorTo:     mustDate(t, "2025-12-31"),
		Search:      search,
		Lim:         100,
	})
	if err != nil {
		t.Fatalf("ListMerchantSpend: %v", err)
	}
	return rows
}

func findExplorerRow(rows []dbgen.ListMerchantSpendRow, key string) *dbgen.ListMerchantSpendRow {
	for i := range rows {
		if rows[i].MerchantKey == key {
			return &rows[i]
		}
	}
	return nil
}

// The explorer and the top-merchant report are two aggregations of the same money,
// so they have to agree. Asserted on a MERGED merchant, where they could most
// easily diverge.
func TestMerchantExplorerAgreesWithTopMerchants(t *testing.T) {
	ctx, f := setupMerchantFixture(t)

	entityID, _, err := merchants.Merge(ctx, f.q, f.householdID,
		[]string{"acme plus", "acmeplus bill"}, "Acme Plus", nil)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}

	var want dbgen.GetTopMerchantsRow
	for _, row := range f.topMerchants(t, ctx) {
		if row.MerchantKey == entityID.String() {
			want = row
		}
	}
	if want.MerchantKey == "" {
		t.Fatal("merged merchant missing from top merchants")
	}

	got := findExplorerRow(explore(t, ctx, f, nil), entityID.String())
	if got == nil {
		t.Fatalf("merged merchant missing from the explorer")
	}
	if !got.Total.Equal(want.Total) {
		t.Errorf("explorer total = %s, top merchants said %s", got.Total, want.Total)
	}
	if got.TransactionCount != want.TransactionCount {
		t.Errorf("explorer count = %d, top merchants said %d", got.TransactionCount, want.TransactionCount)
	}
	if got.Merchant != want.Merchant {
		t.Errorf("explorer name = %q, top merchants said %q", got.Merchant, want.Merchant)
	}
	// One row for the merchant, not one per descriptor.
	if n := len(explore(t, ctx, f, nil)); n != 1 {
		t.Errorf("merged merchant should be a single row, got %d rows", n)
	}
}

// Searching ONE descriptor of a grouped merchant must return that merchant with
// ALL of its spend. The fixture bills half its charges under each descriptor, so a
// pre-aggregation filter would report exactly half the total — and the row would
// disagree with the detail page it links to.
func TestMerchantExplorerSearchDoesNotTruncateTotals(t *testing.T) {
	ctx, f := setupMerchantFixture(t)

	entityID, _, err := merchants.Merge(ctx, f.q, f.householdID,
		[]string{"acme plus", "acmeplus bill"}, "Acme Plus", nil)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}

	full := findExplorerRow(explore(t, ctx, f, nil), entityID.String())
	if full == nil {
		t.Fatal("merged merchant missing from the unfiltered explorer")
	}

	// "acmeplus bill" is only three of the six charges, and is not a substring of
	// the canonical name "Acme Plus" — so only the descriptor match can find it.
	needle := "acmeplus bill"
	matched := explore(t, ctx, f, &needle)
	got := findExplorerRow(matched, entityID.String())
	if got == nil {
		t.Fatalf("searching a descriptor of a grouped merchant found nothing: %+v", matched)
	}
	if !got.Total.Equal(full.Total) {
		t.Errorf("searched total = %s, want the merchant's full total %s — the needle truncated it",
			got.Total, full.Total)
	}
	if got.TransactionCount != full.TransactionCount {
		t.Errorf("searched count = %d, want all %d charges", got.TransactionCount, full.TransactionCount)
	}

	// Searching the canonical name works too, and finds the same one row.
	canonical := "acme"
	if rows := explore(t, ctx, f, &canonical); len(rows) != 1 {
		t.Errorf("canonical-name search returned %d rows, want 1: %+v", len(rows), rows)
	}

	// A needle matching nothing is empty, not everything.
	miss := "no such merchant anywhere"
	if rows := explore(t, ctx, f, &miss); len(rows) != 0 {
		t.Errorf("a needle matching nothing returned %d rows", len(rows))
	}
}

// window_total is the concentration denominator, and it must ignore the needle —
// otherwise "your top 10 are N% of spending" changes as the user types.
func TestMerchantExplorerWindowTotalIgnoresSearch(t *testing.T) {
	ctx, f := setupMerchantFixture(t)
	f.addTx(t, ctx, "2026-04-11", "TACO STAND", "taco stand")

	unfiltered := explore(t, ctx, f, nil)
	if len(unfiltered) == 0 {
		t.Fatal("no merchants in the window")
	}
	want := unfiltered[0].WindowTotal

	needle := "taco"
	matched := explore(t, ctx, f, &needle)
	if len(matched) != 1 {
		t.Fatalf("search returned %d rows, want 1", len(matched))
	}
	if !matched[0].WindowTotal.Equal(want) {
		t.Errorf("window_total under search = %s, want %s (the needle must not move it)",
			matched[0].WindowTotal, want)
	}
	// The row's own total is a fraction of the window, so this also proves the
	// denominator is not just the matched row.
	if !matched[0].Total.LessThan(want) {
		t.Errorf("expected the matched row's total %s to be less than the window total %s",
			matched[0].Total, want)
	}
}

// "New" means never seen before the window, not merely absent from the previous
// period. A merchant that charged two years ago and again now is not new.
func TestMerchantExplorerIsNewMeansFirstEverCharge(t *testing.T) {
	ctx, f := setupMerchantFixture(t)

	// Seen long before the window, and again inside it — but never in the
	// immediately preceding year, which is what a prior-window test alone would
	// wrongly call new.
	f.addTx(t, ctx, "2024-03-02", "OLD FRIEND", "old friend")
	f.addTx(t, ctx, "2026-03-02", "OLD FRIEND", "old friend")
	// First charge ever lands inside the window.
	f.addTx(t, ctx, "2026-05-20", "BRAND NEW CO", "brand new co")

	rows := explore(t, ctx, f, nil)

	old := findExplorerRow(rows, "old friend")
	if old == nil {
		t.Fatal("returning merchant missing from the explorer")
	}
	if old.IsNew {
		t.Error("a merchant with a charge two years ago was reported as new")
	}

	fresh := findExplorerRow(rows, "brand new co")
	if fresh == nil {
		t.Fatal("new merchant missing from the explorer")
	}
	if !fresh.IsNew {
		t.Error("a merchant whose first ever charge is inside the window was not reported as new")
	}
}

// prior_total is money, and a merchant absent from the prior window has zero of
// it — not null. The frontend divides by it to get a change percentage, and a null
// arriving as a JSON null would make that arithmetic silently wrong.
func TestMerchantExplorerPriorTotalIsZeroNotNull(t *testing.T) {
	ctx, f := setupMerchantFixture(t)

	rows := explore(t, ctx, f, nil)
	if len(rows) == 0 {
		t.Fatal("no merchants in the window")
	}
	for _, row := range rows {
		// The fixture has no 2025 charges at all, so every prior total is zero.
		if !row.PriorTotal.IsZero() {
			t.Errorf("%s prior_total = %s, want 0", row.MerchantKey, row.PriorTotal)
		}
	}

	// With prior-window charges present, the figure is the prior window's own spend
	// — never the current window's.
	f.addTx(t, ctx, "2025-06-01", "STEADY CO", "steady co")
	f.addTx(t, ctx, "2026-06-01", "STEADY CO", "steady co")
	steady := findExplorerRow(explore(t, ctx, f, nil), "steady co")
	if steady == nil {
		t.Fatal("steady merchant missing from the explorer")
	}
	if steady.PriorTotal.IsZero() {
		t.Error("prior_total is zero for a merchant that charged in the prior window")
	}
	if !steady.PriorTotal.Equal(steady.Total) {
		t.Errorf("prior %s and current %s should each be one $25 charge",
			steady.PriorTotal, steady.Total)
	}
}

// The category filter narrows a merchant to that slice of its spend — unlike the
// search needle, which must not. Both behaviours live in the same query, so it is
// worth pinning that they differ on purpose.
func TestMerchantExplorerCategoryFilterNarrowsSpend(t *testing.T) {
	ctx, f := setupMerchantFixture(t)

	// addTx files its charge under Food & Drink; the base fixture is Entertainment.
	f.addTx(t, ctx, "2026-04-11", "ACME PLUS", "acme plus")

	unfiltered := findExplorerRow(explore(t, ctx, f, nil), "acme plus")
	if unfiltered == nil {
		t.Fatal("merchant missing from the unfiltered explorer")
	}

	rows, err := f.q.ListMerchantSpend(ctx, dbgen.ListMerchantSpendParams{
		HouseholdID: f.householdID, UserID: f.userID,
		Date: mustDate(t, "2026-01-01"), Date_2: mustDate(t, "2026-12-31"),
		PriorFrom: mustDate(t, "2025-01-01"), PriorTo: mustDate(t, "2025-12-31"),
		CategoryID: &f.foodID, Lim: 100,
	})
	if err != nil {
		t.Fatalf("ListMerchantSpend with category: %v", err)
	}
	filtered := findExplorerRow(rows, "acme plus")
	if filtered == nil {
		t.Fatal("merchant missing from the category-filtered explorer")
	}
	if !filtered.Total.LessThan(unfiltered.Total) {
		t.Errorf("category-filtered total %s should be a slice of the full %s",
			filtered.Total, unfiltered.Total)
	}
	if filtered.TransactionCount != 1 {
		t.Errorf("category-filtered count = %d, want the single Food & Drink charge",
			filtered.TransactionCount)
	}
	// And the chip reports the category the spend actually lands in.
	if filtered.CategoryID == nil || *filtered.CategoryID != f.foodID {
		t.Errorf("top category = %v, want the Food & Drink id %v", filtered.CategoryID, f.foodID)
	}
}

// The gone-quiet list is the complement of the live one: the same cadence rules,
// the opposite activity test. A merchant must appear in exactly one of them, or
// the two lists are telling different stories about the same history.
func TestLapsedMerchantsAreTheComplementOfRecurring(t *testing.T) {
	ctx, f := setupMerchantFixture(t)

	if _, _, err := merchants.Merge(ctx, f.q, f.householdID,
		[]string{"acme plus", "acmeplus bill"}, "Acme Plus", nil); err != nil {
		t.Fatalf("Merge: %v", err)
	}

	recurringAt := func(asOf string, lapsed *bool) []dbgen.GetRecurringMerchantsRow {
		t.Helper()
		rows, err := f.q.GetRecurringMerchants(ctx, dbgen.GetRecurringMerchantsParams{
			HouseholdID: f.householdID, UserID: f.userID,
			Date:    mustDate(t, "2024-01-01"),
			Column4: mustDate(t, asOf),
			Lapsed:  lapsed,
		})
		if err != nil {
			t.Fatalf("GetRecurringMerchants(lapsed=%v): %v", lapsed, err)
		}
		return rows
	}
	yes := true

	// Just after the last charge, the subscription is live and not lapsed.
	if len(recurringAt("2026-06-30", nil)) != 1 {
		t.Fatalf("the merged subscription should be live at 2026-06-30")
	}
	if rows := recurringAt("2026-06-30", &yes); len(rows) != 0 {
		t.Errorf("a live subscription also appeared as gone quiet: %+v", rows)
	}

	// A year later it has stopped billing: gone quiet, and no longer live.
	if rows := recurringAt("2027-06-30", nil); len(rows) != 0 {
		t.Errorf("a subscription silent for a year still counted as live: %+v", rows)
	}
	lapsed := recurringAt("2027-06-30", &yes)
	if len(lapsed) != 1 {
		t.Fatalf("want the silent subscription in the gone-quiet list, got %d rows: %+v",
			len(lapsed), lapsed)
	}
	if lapsed[0].Merchant != "Acme Plus" {
		t.Errorf("gone-quiet merchant = %q, want the canonical name", lapsed[0].Merchant)
	}
	// days_quiet is what the card reports, so it has to be measured from the
	// as-of date rather than from today.
	if lapsed[0].DaysQuiet < 300 {
		t.Errorf("days_quiet = %d, want roughly a year", lapsed[0].DaysQuiet)
	}
}

// A row's resolved key is only useful if it addresses the merchant detail view, so
// this walks the whole path a click takes: list a transaction, take its resolved
// key, and look the merchant up by it.
func TestTransactionResolvedKeyOpensTheMerchant(t *testing.T) {
	ctx, f := setupMerchantFixture(t)

	entityID, _, err := merchants.Merge(ctx, f.q, f.householdID,
		[]string{"acme plus", "acmeplus bill"}, "Acme Plus", nil)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}

	from, to := mustDate(t, "2026-01-01"), mustDate(t, "2026-12-31")
	txns, err := f.q.ListVisibleTransactions(ctx, dbgen.ListVisibleTransactionsParams{
		HouseholdID: f.householdID, UserID: f.userID,
		Date: from, Date_2: to, Limit: 100,
	})
	if err != nil {
		t.Fatalf("ListVisibleTransactions: %v", err)
	}
	if len(txns) != 6 {
		t.Fatalf("want the fixture's 6 charges, got %d", len(txns))
	}

	// Every charge of a grouped merchant resolves to the entity, whichever
	// descriptor it arrived under. This is the whole point: the raw key would send
	// half of them to a merchant that no longer exists under that key.
	for _, txn := range txns {
		if txn.ResolvedMerchantKey != entityID.String() {
			t.Fatalf("charge %q resolved to %q, want the entity id %q",
				txn.Name, txn.ResolvedMerchantKey, entityID)
		}
	}

	// And the key really opens the merchant.
	identity, err := f.q.GetMerchantIdentity(ctx, dbgen.GetMerchantIdentityParams{
		HouseholdID: f.householdID, UserID: f.userID,
		ResolvedKey: txns[0].ResolvedMerchantKey,
	})
	if err != nil {
		t.Fatalf("GetMerchantIdentity from a transaction's resolved key: %v", err)
	}
	if identity.Merchant != "Acme Plus" || !identity.IsGrouped {
		t.Errorf("resolved key opened %+v, want the grouped Acme Plus", identity)
	}
}

// Filtering transactions by a grouped merchant's entity id must return every
// descriptor's charges. Filtering by one raw descriptor still works and returns
// only that fragment, which is what the merchant page's descriptor list implies.
func TestTransactionMerchantFilterCoversEveryDescriptor(t *testing.T) {
	ctx, f := setupMerchantFixture(t)

	entityID, _, err := merchants.Merge(ctx, f.q, f.householdID,
		[]string{"acme plus", "acmeplus bill"}, "Acme Plus", nil)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	// An unrelated merchant, so "returns everything" cannot pass by accident.
	f.addTx(t, ctx, "2026-04-11", "TACO STAND", "taco stand")

	list := func(merchantKey, search *string) []dbgen.ListVisibleTransactionsRow {
		t.Helper()
		rows, err := f.q.ListVisibleTransactions(ctx, dbgen.ListVisibleTransactionsParams{
			HouseholdID: f.householdID, UserID: f.userID,
			Date: mustDate(t, "2026-01-01"), Date_2: mustDate(t, "2026-12-31"),
			Limit: 100, MerchantKey: merchantKey, Search: search,
		})
		if err != nil {
			t.Fatalf("ListVisibleTransactions: %v", err)
		}
		return rows
	}

	key := entityID.String()
	if got := list(&key, nil); len(got) != 6 {
		t.Errorf("filtering by the entity id returned %d charges, want all 6", len(got))
	}

	// A raw descriptor of a GROUPED merchant is still a valid filter and returns
	// that fragment. Resolved-only matching would answer "no charges" here, because
	// the descriptor resolves to the entity id and so never equals itself.
	raw := "acmeplus bill"
	if got := list(&raw, nil); len(got) != 3 {
		t.Errorf("filtering by one descriptor returned %d charges, want its 3", len(got))
	}

	// A key nobody has is an empty list, not an error and not everything.
	missing := uuid.New().String()
	if got := list(&missing, nil); len(got) != 0 {
		t.Errorf("filtering by an unknown key returned %d charges, want none", len(got))
	}

	// Search matches the descriptor and the display name, case-insensitively.
	needle := "taco"
	if got := list(nil, &needle); len(got) != 1 {
		t.Errorf("searching %q returned %d charges, want 1", needle, len(got))
	}
	upper := "ACME"
	if got := list(nil, &upper); len(got) != 6 {
		t.Errorf("case-insensitive search returned %d charges, want 6", len(got))
	}
}
