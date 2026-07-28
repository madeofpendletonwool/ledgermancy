package reporting

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

func strptr(s string) *string { return &s }

func holding(value string, securityType *string, taxTreatment *string, cash bool) dbgen.ListVisibleHoldingsDetailedRow {
	return dbgen.ListVisibleHoldingsDetailedRow{
		InstitutionValue: decimal.NullDecimal{Decimal: dec(value), Valid: true},
		SecurityType:     securityType,
		TaxTreatment:     taxTreatment,
		IsCashEquivalent: cash,
	}
}

// TestAllocationKeepsUnknownVisible is the honesty test for this chart. Plaid's
// asset-class coverage is partial, and the tempting fix — spreading the
// unclassified value across the known bands so the pie adds to 100% — makes
// every slice wrong while making the chart look complete.
func TestAllocationKeepsUnknownVisible(t *testing.T) {
	slices := Allocation([]dbgen.ListVisibleHoldingsDetailedRow{
		holding("600", strptr("etf"), nil, false),
		holding("300", strptr("equity"), nil, false),
		holding("100", nil, nil, false), // no type reported
	})

	byLabel := map[string]AllocationSlice{}
	for _, s := range slices {
		byLabel[s.Label] = s
	}

	unknown, ok := byLabel["Unknown"]
	if !ok {
		t.Fatal("no Unknown slice: unclassified value was folded into the known bands")
	}
	if !unknown.Value.Equal(dec("100")) {
		t.Errorf("Unknown value = %s, want 100", unknown.Value)
	}
	if !unknown.Percent.Equal(dec("10")) {
		t.Errorf("Unknown percent = %s, want 10", unknown.Percent)
	}
	if !byLabel["ETF"].Percent.Equal(dec("60")) {
		t.Errorf("ETF percent = %s, want 60 (not renormalised over the known slices)",
			byLabel["ETF"].Percent)
	}

	// Unknown sorts last regardless of size, so the gap does not lead the chart.
	if slices[len(slices)-1].Label != "Unknown" {
		t.Errorf("last slice is %q, want Unknown pinned last", slices[len(slices)-1].Label)
	}
}

// TestAllocationByTaxTreatmentSurfacesUntagged: an untagged account is a prompt
// to classify, so it has to be visible rather than silently defaulted.
func TestAllocationByTaxTreatmentSurfacesUntagged(t *testing.T) {
	slices := AllocationByTaxTreatment([]dbgen.ListVisibleHoldingsDetailedRow{
		holding("500", strptr("etf"), strptr("roth_ira"), false),
		holding("500", strptr("etf"), nil, false),
	})

	byLabel := map[string]AllocationSlice{}
	for _, s := range slices {
		byLabel[s.Label] = s
	}
	if _, ok := byLabel["Untagged"]; !ok {
		t.Fatal("untagged holdings did not produce an Untagged slice")
	}
	if _, ok := byLabel["Roth IRA"]; !ok {
		t.Fatalf("expected a Roth IRA slice, got %v", slices)
	}
}

// TestFeeDragDisclosesExclusions: with no expense-ratio source (today's real
// state) every holding must be reported as excluded and the cost must be zero —
// never a partial total presented as the whole.
func TestFeeDragDisclosesExclusions(t *testing.T) {
	rows := []dbgen.ListVisibleHoldingsDetailedRow{
		{
			InstitutionValue: decimal.NullDecimal{Decimal: dec("10000"), Valid: true},
			Ticker:           strptr("VTI"),
		},
		{
			InstitutionValue: decimal.NullDecimal{Decimal: dec("5000"), Valid: true},
			Ticker:           strptr("XYZ"),
		},
	}

	none := ComputeFeeDrag(rows, nil)
	if !none.AnnualCost.IsZero() {
		t.Errorf("AnnualCost = %s with no ratio source, want 0", none.AnnualCost)
	}
	if none.ExcludedHoldings != 2 {
		t.Errorf("ExcludedHoldings = %d, want 2", none.ExcludedHoldings)
	}
	if !none.UncoveredValue.Equal(dec("15000")) {
		t.Errorf("UncoveredValue = %s, want 15000", none.UncoveredValue)
	}

	// With partial coverage the cost is computed over the covered part only, and
	// the excluded count is what lets the UI say so.
	partial := ComputeFeeDrag(rows, map[string]decimal.Decimal{"VTI": dec("0.03")})
	// 10000 × 0.03% = 3.00
	if !partial.AnnualCost.Equal(dec("3")) {
		t.Errorf("AnnualCost = %s, want 3", partial.AnnualCost)
	}
	if partial.CoveredHoldings != 1 || partial.ExcludedHoldings != 1 {
		t.Errorf("coverage = %d covered / %d excluded, want 1 / 1",
			partial.CoveredHoldings, partial.ExcludedHoldings)
	}
	if !partial.UncoveredValue.Equal(dec("5000")) {
		t.Errorf("UncoveredValue = %s, want 5000", partial.UncoveredValue)
	}
}

// TestSuggestTaxTreatmentRefusesAmbiguity is the test that protects every
// retirement number downstream. Plaid reports "401k" for both a traditional and
// a Roth plan; guessing one would be wrong for half of all users, silently.
func TestSuggestTaxTreatmentRefusesAmbiguity(t *testing.T) {
	ambiguous := []string{"401k", "401a", "403b", "457b", "pension", "", "something-new"}
	for _, subtype := range ambiguous {
		if got := SuggestTaxTreatment(subtype); got != "" {
			t.Errorf("SuggestTaxTreatment(%q) = %q, want \"\" — it cannot be known from the subtype",
				subtype, got)
		}
	}

	clear := map[string]string{
		"brokerage": "taxable",
		"ira":       "trad_ira",
		"roth":      "roth_ira",
		"roth ira":  "roth_ira",
		"529":       "529",
		"hsa":       "hsa",
	}
	for subtype, want := range clear {
		if got := SuggestTaxTreatment(subtype); got != want {
			t.Errorf("SuggestTaxTreatment(%q) = %q, want %q", subtype, got, want)
		}
	}
}

// TestRebaseRefusesNonPositiveBase: an index rebased on zero would be a chart
// that looks authoritative and means nothing.
func TestRebaseRefusesNonPositiveBase(t *testing.T) {
	if _, ok := Rebase("X", []RebasedPoint{{Date: day(0), Value: decimal.Zero}}); ok {
		t.Error("Rebase accepted a zero base")
	}
	if _, ok := Rebase("X", nil); ok {
		t.Error("Rebase accepted an empty series")
	}

	series, ok := Rebase("SPY", []RebasedPoint{
		{Date: day(0), Value: dec("400")},
		{Date: day(1), Value: dec("440")},
	})
	if !ok {
		t.Fatal("Rebase refused a valid series")
	}
	if !series.Points[0].Value.Equal(dec("100")) {
		t.Errorf("first point = %s, want 100", series.Points[0].Value)
	}
	if !series.Points[1].Value.Equal(dec("110")) {
		t.Errorf("second point = %s, want 110", series.Points[1].Value)
	}
}

// TestPeriodStart pins the window boundaries the UI selects between.
func TestPeriodStart(t *testing.T) {
	now := time.Date(2026, 7, 28, 15, 30, 0, 0, time.UTC)

	if got := PeriodStart(PeriodYTD, now); got.Format(time.DateOnly) != "2026-01-01" {
		t.Errorf("YTD start = %s, want 2026-01-01", got.Format(time.DateOnly))
	}
	if got := PeriodStart(Period1Y, now); got.Format(time.DateOnly) != "2025-07-28" {
		t.Errorf("1y start = %s, want 2025-07-28", got.Format(time.DateOnly))
	}
	if got := PeriodStart(Period5Y, now); got.Format(time.DateOnly) != "2021-07-28" {
		t.Errorf("5y start = %s, want 2021-07-28", got.Format(time.DateOnly))
	}
	// Inception is clamped by the data, so the floor only has to be earlier than
	// any snapshot could be.
	if !PeriodStart(PeriodSinceInception, now).Before(now.AddDate(-50, 0, 0)) {
		t.Error("inception start is not far enough back to be clamped by real data")
	}
}

func TestValidPeriod(t *testing.T) {
	for _, ok := range []string{"ytd", "1y", "3y", "5y", "inception"} {
		if !ValidPeriod(ok) {
			t.Errorf("ValidPeriod(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", "10y", "YTD", "all"} {
		if ValidPeriod(bad) {
			t.Errorf("ValidPeriod(%q) = true", bad)
		}
	}
}

// TestHistoryCaveatAlwaysPresentWhenThin: the caveat is not decoration. A young
// install must never render a return figure without it.
func TestHistoryCaveatAlwaysPresentWhenThin(t *testing.T) {
	thin := Performance{Days: 12, Start: day(0)}
	if historyCaveat(Period1Y, day(-365), thin) == "" {
		t.Error("no caveat for a 12-day history")
	}

	// A short window fully covered needs no caveat.
	full := Performance{Days: 400, Start: day(0)}
	if got := historyCaveat(Period1Y, day(0), full); got != "" {
		t.Errorf("unexpected caveat for a fully-covered period: %q", got)
	}

	// A long request only partly covered says how much it actually has.
	partial := Performance{Days: 200, Start: day(100)}
	if historyCaveat(Period5Y, day(0), partial) == "" {
		t.Error("no caveat for a 5-year request backed by 200 days")
	}
}
