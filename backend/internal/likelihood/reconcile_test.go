package likelihood

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// Doc 33's "plan tracking drift" verification.

func day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// baseTracking is a plan accepted six months ago: $30k up front and $1,000 a
// month split across two buckets.
func baseTracking(rothActual, brokerageActual string) ReconcileInput {
	return ReconcileInput{
		Since:          day(2026, time.February, 1),
		AsOf:           day(2026, time.August, 1),
		Lump:           dec("30000"),
		RealReturnRate: dec("0.07"),
		HorizonMonths:  17 * 12,
		DriftThreshold: dec("25"),
		Buckets: []BucketActual{
			{AccountID: "roth", Name: "Roth", ExpectedLump: dec("10000"),
				ExpectedMonthly: dec("400"), Actual: dec(rothActual), Tracked: true},
			{AccountID: "brokerage", Name: "Brokerage", ExpectedLump: dec("20000"),
				ExpectedMonthly: dec("600"), Actual: dec(brokerageActual), Tracked: true},
		},
	}
}

// The arithmetic, against a hand-computed expectation.
//
// Six whole months elapsed. Roth expects 10,000 + 6×400 = 12,400; Brokerage
// expects 20,000 + 6×600 = 23,600. Total expected 36,000.
func TestDriftMatchesHandComputedExpectation(t *testing.T) {
	got := Reconcile(baseTracking("12400", "23600"))

	if got.Months != 6 {
		t.Fatalf("months = %d, want 6", got.Months)
	}
	if want := dec("36000"); !got.ExpectedToDate.Equal(want) {
		t.Errorf("expected to date = %s, want %s", got.ExpectedToDate, want)
	}
	if !got.Drift.IsZero() {
		t.Errorf("drift = %s, want 0 — the actuals match the plan exactly", got.Drift)
	}
	if !got.OnTrack {
		t.Error("a plan matching its own contributions must be on track")
	}
	if !got.ProjectedShortfall.IsZero() {
		t.Errorf("projected shortfall = %s, want 0", got.ProjectedShortfall)
	}
}

// Behind: the doc's own example shape — a monthly gap, and what it costs at the
// horizon if nothing changes.
func TestBehindReportsMonthlyGapAndHorizonCost(t *testing.T) {
	// Brokerage received 1,080 less than the 23,600 it should have: $180/mo.
	got := Reconcile(baseTracking("12400", "22520"))

	if want := dec("-1080"); !got.Drift.Equal(want) {
		t.Fatalf("drift = %s, want %s", got.Drift, want)
	}
	if want := dec("-180"); !got.MonthlyDrift.Equal(want) {
		t.Fatalf("monthly drift = %s, want %s", got.MonthlyDrift, want)
	}
	if got.OnTrack {
		t.Error("$180/mo behind must not read as on track")
	}
	if !got.ProjectedShortfall.IsPositive() {
		t.Error("a persistent monthly gap must compound to a horizon shortfall")
	}
	// $180/mo compounded at 7% real over the remaining 198 months is a large
	// five-figure number; the point is that it is far more than the raw gap.
	if !got.ProjectedShortfall.GreaterThan(dec("180").Mul(decimal.NewFromInt(198))) {
		t.Errorf("projected shortfall %s is not more than the un-compounded gap — "+
			"the shortfall must include the growth the missing money would have earned",
			got.ProjectedShortfall)
	}
	if !contains(got.Summary, "180.00") {
		t.Errorf("the summary must name the monthly gap, got: %s", got.Summary)
	}
}

// AHEAD IS ON TRACK. This is a floor, not a band — nobody needs an alert saying
// they saved too much.
func TestAheadIsOnTrack(t *testing.T) {
	got := Reconcile(baseTracking("13000", "24000"))
	if !got.Drift.IsPositive() {
		t.Fatalf("drift = %s, want positive", got.Drift)
	}
	if !got.OnTrack {
		t.Error("a household ahead of its plan must be on track")
	}
	if !got.ProjectedShortfall.IsZero() {
		t.Errorf("projected shortfall = %s, want 0 for a plan that is ahead", got.ProjectedShortfall)
	}
	if !contains(got.Summary, "MORE") {
		t.Errorf("the summary should say the household is ahead, got: %s", got.Summary)
	}
}

// A small gap inside the threshold is on track: a contribution that landed on
// the 2nd rather than the 30th must not flip a plan to "behind" for a month.
func TestSmallGapIsWithinTheThreshold(t *testing.T) {
	// $60 short over six months is $10/mo, inside the $25 band.
	got := Reconcile(baseTracking("12400", "23540"))
	if !got.OnTrack {
		t.Errorf("a $%s/mo gap is inside the threshold and must read as on track", got.MonthlyDrift.Neg())
	}
}

// AN UNTRACKED BUCKET IS NOT A ZERO. "We cannot see what you paid in" and "you
// paid in nothing" are opposite findings, and collapsing them would tell a
// household it had missed contributions it actually made.
func TestUntrackedBucketsAreExcludedNotZeroed(t *testing.T) {
	in := baseTracking("12400", "0")
	in.Buckets[1].Tracked = false

	got := Reconcile(in)

	if want := dec("12400"); !got.ExpectedToDate.Equal(want) {
		t.Errorf("expected to date = %s, want %s — an untracked bucket must not contribute its "+
			"expectation either, or the plan reads as behind by exactly the amount we cannot see",
			got.ExpectedToDate, want)
	}
	if !got.Drift.IsZero() {
		t.Errorf("drift = %s, want 0 — the tracked bucket matches and the untracked one is not judged", got.Drift)
	}
	if len(got.Untracked) != 1 || got.Untracked[0] != "Brokerage" {
		t.Errorf("Untracked = %v, want [Brokerage]", got.Untracked)
	}
	if !contains(got.Summary, "no contribution trail") {
		t.Errorf("the summary must disclose the untracked bucket, got: %s", got.Summary)
	}

	var brokerage BucketDrift
	for _, b := range got.Buckets {
		if b.Name == "Brokerage" {
			brokerage = b
		}
	}
	if brokerage.Tracked || brokerage.Note == "" {
		t.Error("an untracked bucket must be reported as untracked and say why")
	}
	if !brokerage.Drift.IsZero() {
		t.Errorf("an untracked bucket must claim no drift, got %s", brokerage.Drift)
	}
}

// Every bucket untracked means nothing is claimed at all.
func TestAllUntrackedClaimsNothing(t *testing.T) {
	in := baseTracking("0", "0")
	in.Buckets[0].Tracked, in.Buckets[1].Tracked = false, false

	got := Reconcile(in)
	if !got.Drift.IsZero() || !got.ExpectedToDate.IsZero() {
		t.Error("with no trail anywhere, nothing may be claimed")
	}
	if !contains(got.Summary, "nothing to compare") {
		t.Errorf("the summary must say there is nothing to compare, got: %s", got.Summary)
	}
}

// A PLAN LESS THAN A MONTH OLD EXPECTS NO MONTHLY CONTRIBUTION YET. Counting a
// partial month would put every new plan behind on the day it was made.
func TestPartialMonthsExpectNoMonthlyContribution(t *testing.T) {
	in := baseTracking("10000", "20000")
	in.Since = day(2026, time.August, 1)
	in.AsOf = day(2026, time.August, 20)

	got := Reconcile(in)
	if got.Months != 0 {
		t.Fatalf("months = %d, want 0 — nineteen days is not a month", got.Months)
	}
	if want := dec("30000"); !got.ExpectedToDate.Equal(want) {
		t.Errorf("expected = %s, want %s — only the lump is due yet", got.ExpectedToDate, want)
	}
	if !contains(got.Summary, "less than a month old") {
		t.Errorf("the summary should say the plan is too new to compare, got: %s", got.Summary)
	}
}

// wholeMonthsBetween counts COMPLETED months. A plan made on the 3rd is one
// month old on the 3rd of next month and not before.
func TestWholeMonthsBetween(t *testing.T) {
	cases := []struct {
		from, to time.Time
		want     int
	}{
		{day(2026, time.February, 3), day(2026, time.March, 2), 0},
		{day(2026, time.February, 3), day(2026, time.March, 3), 1},
		{day(2026, time.February, 3), day(2026, time.March, 4), 1},
		{day(2026, time.February, 1), day(2026, time.August, 1), 6},
		{day(2026, time.February, 1), day(2027, time.February, 1), 12},
		// A date in the past is not negative months.
		{day(2026, time.August, 1), day(2026, time.February, 1), 0},
	}
	for _, c := range cases {
		if got := wholeMonthsBetween(c.from, c.to); got != c.want {
			t.Errorf("wholeMonthsBetween(%s, %s) = %d, want %d",
				c.from.Format(time.DateOnly), c.to.Format(time.DateOnly), got, c.want)
		}
	}
}

// The horizon shortfall uses the SAME compounding convention as the projection
// engine: a real annual rate divided by twelve, growth before each deposit. A
// closed-form annuity factor would differ by the timing of the first deposit,
// and a shortfall that disagreed with the projection beside it is exactly the
// two-surfaces-disagreeing failure doc 24 names.
func TestCompoundMonthlyMatchesTheProjectionConvention(t *testing.T) {
	// Twelve months of $100 at 12% real: each month grows the running balance by
	// 1% BEFORE the deposit, so the first deposit earns eleven months of growth.
	got := compoundMonthly(dec("100"), dec("0.12"), 12)

	want := decimal.Zero
	rate := dec("0.01")
	for m := 0; m < 12; m++ {
		want = want.Add(want.Mul(rate)).Add(dec("100")).Round(2)
	}
	if !got.Equal(want) {
		t.Errorf("compoundMonthly = %s, want %s", got, want)
	}
	// Sanity: more than the un-compounded total, less than a full year of growth
	// on the whole sum.
	if !got.GreaterThan(dec("1200")) || !got.LessThan(dec("1344")) {
		t.Errorf("compoundMonthly = %s, outside the plausible band for 12 x $100 at 12%%", got)
	}
}

// A zero real return compounds to exactly the sum of the deposits.
func TestCompoundMonthlyWithNoGrowth(t *testing.T) {
	if got := compoundMonthly(dec("100"), decimal.Zero, 12); !got.Equal(dec("1200")) {
		t.Errorf("compoundMonthly at 0%% = %s, want 1200", got)
	}
}

// Past the horizon there is no remaining term to compound over.
func TestNoShortfallPastTheHorizon(t *testing.T) {
	in := baseTracking("0", "0")
	in.HorizonMonths = 3 // already elapsed

	got := Reconcile(in)
	if got.RemainingMonths != 0 {
		t.Errorf("remaining months = %d, want 0", got.RemainingMonths)
	}
	if !got.ProjectedShortfall.IsZero() {
		t.Errorf("projected shortfall = %s, want 0 past the horizon", got.ProjectedShortfall)
	}
}

// Reconcile is pure: the same input twice gives the same answer, and the input
// is not mutated.
func TestReconcileIsPure(t *testing.T) {
	in := baseTracking("12400", "22520")
	first := Reconcile(in)
	second := Reconcile(in)

	if !first.Drift.Equal(second.Drift) || first.Summary != second.Summary {
		t.Error("Reconcile is not deterministic")
	}
	if !in.Buckets[0].Actual.Equal(dec("12400")) {
		t.Error("Reconcile mutated its input")
	}
}
