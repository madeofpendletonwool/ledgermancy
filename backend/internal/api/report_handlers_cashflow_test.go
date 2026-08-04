package api

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// buildCashFlow assembles the cash-flow Sankey payload and derives the one
// figure the SQL does not hand it directly: the uncategorised spending gap.
// The parts worth pinning without a database are the reconciliation invariants
// the chart depends on — income sources sum to the income total, spending
// categories plus the gap sum to the spending total, and leftover is income
// minus spending — plus the gap maths itself, including the defensive clamp.
//
// All money here is hand-set decimal; no floats anywhere in the path.

func mustDec(s string) decimal.Decimal {
	d, err := decimal.NewFromString(s)
	if err != nil {
		panic(err)
	}
	return d
}

func sumSources(tts []cashFlowSource) decimal.Decimal {
	sum := decimal.Zero
	for _, c := range tts {
		sum = sum.Add(c.Total)
	}
	return sum
}

func sumCats(cats []categorySpendResponse) decimal.Decimal {
	sum := decimal.Zero
	for _, c := range cats {
		sum = sum.Add(c.Total)
	}
	return sum
}

// TestBuildCashFlowReconciles is the load-bearing assertion for the Sankey:
// every band the chart sizes itself from ties out to a headline the rest of the
// Spending page already shows. If any of these drift, the Sankey stops
// balancing and either draws a gap or quietly disagrees with the tiles beside
// it.
func TestBuildCashFlowReconciles(t *testing.T) {
	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)

	summary := dbgen.GetSpendingSummaryRow{
		Income:   mustDec("5000.00"),
		Spending: mustDec("3200.00"),
	}
	spending := []dbgen.GetSpendingByCategoryRow{
		{CategoryID: uuid.New(), CategoryName: "Rent", CategorySlug: "rent", Total: mustDec("2000.00")},
		{CategoryID: uuid.New(), CategoryName: "Food", CategorySlug: "food", Total: mustDec("800.00")},
	}
	income := []dbgen.GetIncomeByCategoryRow{
		{CategoryID: uuid.New(), CategoryName: "Paycheck", CategorySlug: "paycheck", Total: mustDec("4500.00")},
		{CategoryID: uuid.New(), CategoryName: "Interest", CategorySlug: "interest", Total: mustDec("500.00")},
	}

	got := buildCashFlow(from, to, summary, spending, income)

	if !got.IncomeTotal.Equal(mustDec("5000.00")) {
		t.Errorf("income_total = %s, want 5000.00", got.IncomeTotal)
	}
	if !got.SpendingTotal.Equal(mustDec("3200.00")) {
		t.Errorf("spending_total = %s, want 3200.00", got.SpendingTotal)
	}
	if !got.Leftover.Equal(mustDec("1800.00")) {
		t.Errorf("leftover = %s, want 1800.00", got.Leftover)
	}

	// Income sources must sum to the income band: income requires an is_income
	// category, so there is no uncategorised slice the way spending has one.
	if incomeSum := sumSources(got.IncomeSources); !incomeSum.Equal(got.IncomeTotal) {
		t.Errorf("income sources sum to %s, want %s (income_total)", incomeSum, got.IncomeTotal)
	}

	// Spending categories plus the uncategorised gap must sum to the spending
	// band — this is the whole reason the gap is carried on the response.
	spendingSum := sumCats(got.SpendingCategories).Add(got.UncategorizedSpending)
	if !spendingSum.Equal(got.SpendingTotal) {
		t.Errorf("spending categories (%s) + uncategorised (%s) = %s, want %s (spending_total)",
			sumCats(got.SpendingCategories), got.UncategorizedSpending, spendingSum, got.SpendingTotal)
	}

	if got.From != "2026-07-01" || got.To != "2026-07-31" {
		t.Errorf("from/to = %s/%s, want 2026-07-01/2026-07-31", got.From, got.To)
	}
}

// TestBuildCashFlowUncategorisedGap pins the gap maths: when the spending
// categories sum to less than the headline (charges with no category), the
// shortfall surfaces as the uncategorised figure so the Sankey still balances.
func TestBuildCashFlowUncategorisedGap(t *testing.T) {
	summary := dbgen.GetSpendingSummaryRow{
		Income:   mustDec("1000.00"),
		Spending: mustDec("1000.00"),
	}
	// 700 categorised against 1000 of spending → 300 uncategorised.
	spending := []dbgen.GetSpendingByCategoryRow{
		{CategoryID: uuid.New(), CategoryName: "Food", CategorySlug: "food", Total: mustDec("700.00")},
	}

	got := buildCashFlow(time.Time{}, time.Time{}, summary, spending, nil)

	if !got.UncategorizedSpending.Equal(mustDec("300.00")) {
		t.Errorf("uncategorised = %s, want 300.00", got.UncategorizedSpending)
	}
}

// TestBuildCashFlowNoGap asserts a zero gap when every dollar of spending is
// categorised, so the Sankey does not invent a spurious "Uncategorised" flow.
func TestBuildCashFlowNoGap(t *testing.T) {
	summary := dbgen.GetSpendingSummaryRow{
		Income:   mustDec("1000.00"),
		Spending: mustDec("1000.00"),
	}
	spending := []dbgen.GetSpendingByCategoryRow{
		{CategoryID: uuid.New(), CategoryName: "Food", CategorySlug: "food", Total: mustDec("1000.00")},
	}

	got := buildCashFlow(time.Time{}, time.Time{}, summary, spending, nil)

	if !got.UncategorizedSpending.IsZero() {
		t.Errorf("uncategorised = %s, want 0 when categories cover spending", got.UncategorizedSpending)
	}
}

// TestBuildCashFlowClampsNegativeGap guards the defensive clamp: a negative gap
// is not a reachable state (categories are a subset of summary spending by
// construction), but a future query change must never be able to push a
// negative flow through to the chart.
func TestBuildCashFlowClampsNegativeGap(t *testing.T) {
	summary := dbgen.GetSpendingSummaryRow{
		Income:   mustDec("1000.00"),
		Spending: mustDec("1000.00"),
	}
	// Categories overspilling the headline by 100 — impossible today, clamped
	// to zero rather than rendered as a negative flow.
	spending := []dbgen.GetSpendingByCategoryRow{
		{CategoryID: uuid.New(), CategoryName: "Food", CategorySlug: "food", Total: mustDec("1100.00")},
	}

	got := buildCashFlow(time.Time{}, time.Time{}, summary, spending, nil)

	if !got.UncategorizedSpending.IsZero() {
		t.Errorf("uncategorised = %s, want 0 (negative gap clamped)", got.UncategorizedSpending)
	}
}

// TestBuildCashFlowEmpty covers the degrade states: no income and no spending
// in the period must produce a complete, zeroed response rather than nil
// slices or NaN-derived totals, so the chart can render its empty state.
func TestBuildCashFlowEmpty(t *testing.T) {
	summary := dbgen.GetSpendingSummaryRow{} // zero values: 0 income, 0 spending

	got := buildCashFlow(time.Time{}, time.Time{}, summary, nil, nil)

	if !got.IncomeTotal.IsZero() || !got.SpendingTotal.IsZero() || !got.Leftover.IsZero() {
		t.Errorf("empty period = income %s / spending %s / leftover %s, want all 0",
			got.IncomeTotal, got.SpendingTotal, got.Leftover)
	}
	if !got.UncategorizedSpending.IsZero() {
		t.Errorf("uncategorised = %s, want 0 on an empty period", got.UncategorizedSpending)
	}
	if got.IncomeSources == nil || got.SpendingCategories == nil {
		t.Errorf("nil slice on empty period; chart expects an empty (non-nil) array")
	}
}

// TestBuildCashFlowDeficit pins the deficit case (spending exceeds income):
// leftover goes negative, which is the signal the chart uses to draw a deficit
// inflow rather than a savings outflow.
func TestBuildCashFlowDeficit(t *testing.T) {
	summary := dbgen.GetSpendingSummaryRow{
		Income:   mustDec("1000.00"),
		Spending: mustDec("1500.00"),
	}
	spending := []dbgen.GetSpendingByCategoryRow{
		{CategoryID: uuid.New(), CategoryName: "Food", CategorySlug: "food", Total: mustDec("1500.00")},
	}

	got := buildCashFlow(time.Time{}, time.Time{}, summary, spending, nil)

	if !got.Leftover.Equal(mustDec("-500.00")) {
		t.Errorf("leftover = %s, want -500.00 in a deficit period", got.Leftover)
	}
}
