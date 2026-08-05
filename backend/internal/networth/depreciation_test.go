package networth

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// The published curve, checked at the points the source actually states: about
// 20% gone at one year, about 40% of value left at five.
func TestDepreciationCurveMatchesItsSource(t *testing.T) {
	for _, tc := range []struct {
		years string
		want  string
	}{
		{"0", "1.0000"},
		{"0.5", "0.9000"}, // linear through the steep first year
		{"1", "0.8000"},   // ~20% lost in year one
		{"2", "0.6800"},   // 15% of the remainder
		{"3", "0.5780"},
		{"5", "0.4176"}, // "about 40% of value after five years"
	} {
		got := retainedFraction(decimal.RequireFromString(tc.years))
		if got.StringFixed(4) != tc.want {
			t.Errorf("retained at %s years = %s, want %s", tc.years, got.StringFixed(4), tc.want)
		}
	}
}

// A three-year-old car at average mileage. The figure is exact, because the
// curve is exact — the estimate is in the curve's applicability, not in the
// arithmetic.
func TestVehicleSuggestionAtAverageMileage(t *testing.T) {
	s := SuggestVehicleValue(VehicleValuation{
		LastValue:     decimal.NewFromInt(20000),
		LastValueAsOf: month(2025, time.January), // 2 years old
		ModelYear:     2023,
		Mileage:       40500, // exactly 3 × 13,500 at the reading date
		MileageAsOf:   month(2026, time.January),
		AnnualMileage: 13500,
	}, month(2026, time.January)) // 3 years old

	if !s.OK {
		t.Fatalf("refused: %s", s.Reason)
	}
	// 20000 × retained(3)/retained(2) = 20000 × 0.5780/0.6800 = 17000.00,
	// with no mileage tilt because the car is driven exactly to expectation.
	if got := s.Value.StringFixed(2); got != "17000.00" {
		t.Errorf("value = %s, want 17000.00", got)
	}
	if got := s.Change.StringFixed(2); got != "-3000.00" {
		t.Errorf("change = %s, want -3000.00", got)
	}
	if s.Basis == "" {
		t.Error("a suggestion must carry its basis")
	}
}

// A high-mileage case is worth less than the curve alone says, and the tilt is
// bounded — a car driven double the average is worth less, not half as much.
func TestHighMileageIsWorthLessButNotAbsurdlyLess(t *testing.T) {
	base := VehicleValuation{
		LastValue:     decimal.NewFromInt(20000),
		LastValueAsOf: month(2025, time.January),
		ModelYear:     2023,
		MileageAsOf:   month(2026, time.January),
		AnnualMileage: 13500,
	}

	average := base
	average.Mileage = 40500
	heavy := base
	heavy.Mileage = 81000 // double
	light := base
	light.Mileage = 20250 // half

	avg := SuggestVehicleValue(average, month(2026, time.January))
	hi := SuggestVehicleValue(heavy, month(2026, time.January))
	lo := SuggestVehicleValue(light, month(2026, time.January))

	for _, s := range []Suggestion{avg, hi, lo} {
		if !s.OK {
			t.Fatalf("refused: %s", s.Reason)
		}
	}
	if !hi.Value.LessThan(avg.Value) {
		t.Errorf("high mileage (%s) should be worth less than average (%s)", hi.Value, avg.Value)
	}
	if !lo.Value.GreaterThan(avg.Value) {
		t.Errorf("low mileage (%s) should be worth more than average (%s)", lo.Value, avg.Value)
	}

	// The tilt is capped at 20% either way.
	if got := hi.Value.StringFixed(2); got != "13600.00" { // 17000 × 0.80
		t.Errorf("double-mileage value = %s, want 13600.00 (a 20%% cap)", got)
	}
	if got := lo.Value.StringFixed(2); got != "18700.00" { // 17000 × 1.10
		t.Errorf("half-mileage value = %s, want 18700.00", got)
	}
}

// An odometer reading older than the value it would adjust is already baked
// into that value; applying it again would double-count it.
func TestStaleOdometerReadingIsNotApplied(t *testing.T) {
	s := SuggestVehicleValue(VehicleValuation{
		LastValue:     decimal.NewFromInt(20000),
		LastValueAsOf: month(2025, time.January),
		ModelYear:     2023,
		Mileage:       81000,
		MileageAsOf:   month(2024, time.January), // predates the value
		AnnualMileage: 13500,
	}, month(2026, time.January))

	if !s.OK {
		t.Fatalf("refused: %s", s.Reason)
	}
	if got := s.Value.StringFixed(2); got != "17000.00" {
		t.Errorf("value = %s, want the curve-only 17000.00", got)
	}
	if !contains(s.Basis, "already reflected") {
		t.Errorf("basis should explain why mileage was skipped; got %q", s.Basis)
	}
}

func TestVehicleSuggestionRefusals(t *testing.T) {
	now := month(2026, time.January)

	for _, tc := range []struct {
		name string
		in   VehicleValuation
	}{
		{"no value", VehicleValuation{ModelYear: 2020, LastValueAsOf: month(2025, time.January)}},
		{"no model year", VehicleValuation{
			LastValue: decimal.NewFromInt(20000), LastValueAsOf: month(2025, time.January),
		}},
		{"already current", VehicleValuation{
			LastValue: decimal.NewFromInt(20000), LastValueAsOf: now, ModelYear: 2023,
		}},
	} {
		s := SuggestVehicleValue(tc.in, now)
		if s.OK {
			t.Errorf("%s: expected a refusal", tc.name)
		}
		if s.Reason == "" {
			t.Errorf("%s: refusal carries no reason", tc.name)
		}
	}
}

// The suggestion is a proposal and its basis has to say so, because the user is
// being asked to accept or override it rather than told what their car is worth.
func TestSuggestionSaysItIsAnEstimate(t *testing.T) {
	s := SuggestVehicleValue(VehicleValuation{
		LastValue:     decimal.NewFromInt(20000),
		LastValueAsOf: month(2025, time.January),
		ModelYear:     2023,
	}, month(2026, time.January))

	if !s.OK {
		t.Fatalf("refused: %s", s.Reason)
	}
	for _, want := range []string{"estimate", "enter your own", "CARFAX"} {
		if !contains(s.Basis, want) {
			t.Errorf("basis should mention %q; got %q", want, s.Basis)
		}
	}
}

// Determinism, so a suggestion shown twice does not move on its own.
func TestSuggestionIsDeterministic(t *testing.T) {
	in := VehicleValuation{
		LastValue:     decimal.RequireFromString("18345.67"),
		LastValueAsOf: month(2024, time.March),
		ModelYear:     2019,
		Mileage:       97431,
		MileageAsOf:   month(2026, time.February),
		AnnualMileage: 11000,
	}
	first := SuggestVehicleValue(in, month(2026, time.August))
	for i := 0; i < 20; i++ {
		if again := SuggestVehicleValue(in, month(2026, time.August)); again.Value.String() != first.Value.String() {
			t.Fatalf("run %d differed: %s vs %s", i, again.Value, first.Value)
		}
	}
	// Any day within the month gives the same answer.
	mid := time.Date(2026, time.August, 19, 4, 30, 0, 0, time.UTC)
	if v := SuggestVehicleValue(in, mid); v.Value.String() != first.Value.String() {
		t.Errorf("value moved within the month: %s vs %s", v.Value, first.Value)
	}
}

// --------------------------------------------------------------------------
// Equity
// --------------------------------------------------------------------------

func TestEquity(t *testing.T) {
	for _, tc := range []struct {
		name           string
		value, balance string
		owned, paid    string
		underwater     bool
	}{
		{"owned outright", "32000", "0", "32000", "1", false},
		{"part paid", "32000", "8000", "24000", "0.75", false},
		{"underwater", "18000", "23000", "-5000", "0", true},
		{"worthless", "0", "0", "0", "0", false},
	} {
		e := ComputeEquity(
			decimal.RequireFromString(tc.value),
			decimal.RequireFromString(tc.balance))

		if e.Owned.String() != decimal.RequireFromString(tc.owned).String() {
			t.Errorf("%s: owned = %s, want %s", tc.name, e.Owned, tc.owned)
		}
		if !e.PaidFraction.Equal(decimal.RequireFromString(tc.paid)) {
			t.Errorf("%s: paid fraction = %s, want %s", tc.name, e.PaidFraction, tc.paid)
		}
		if e.Underwater != tc.underwater {
			t.Errorf("%s: underwater = %v, want %v", tc.name, e.Underwater, tc.underwater)
		}
	}
}

// Equity is a derived display figure. The arithmetic that proves it cannot
// double-count is that it is exactly the difference of two numbers net worth
// already holds — so adding it would count the asset twice.
func TestEquityIsAlreadyInNetWorth(t *testing.T) {
	value := decimal.RequireFromString("425000")
	loan := decimal.RequireFromString("310000")

	e := ComputeEquity(value, loan)

	// Net worth counts the asset on one side and the loan on the other.
	netWorthContribution := value.Sub(loan)
	if !e.Owned.Equal(netWorthContribution) {
		t.Fatalf("equity %s is not the net-worth contribution %s", e.Owned, netWorthContribution)
	}

	// So a net worth that also added equity would be wrong by exactly the
	// asset's value.
	doubleCounted := netWorthContribution.Add(e.Owned)
	if doubleCounted.Sub(netWorthContribution).Equal(decimal.Zero) {
		t.Fatal("expected adding equity to change the total; the test is not exercising anything")
	}
}
