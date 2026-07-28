package reporting

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// These tests are the point of the file they cover. A wrong spending total is
// visibly wrong; a wrong return figure looks exactly like a right one, so every
// case below is against a hand-computed answer rather than a golden value taken
// from the implementation.

func day(offset int) time.Time {
	return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, offset)
}

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// approx asserts a decimal is within tolerance of the expected value. Returns
// are irrational in general (a chained product, a solved root), so an exact
// comparison would be testing the solver's last digit rather than the answer.
func approx(t *testing.T, label string, got decimal.Decimal, want string, tolerance string) {
	t.Helper()
	diff := got.Sub(dec(want)).Abs()
	if diff.GreaterThan(dec(tolerance)) {
		t.Errorf("%s = %s, want ~%s (diff %s > tolerance %s)",
			label, got.String(), want, diff.String(), tolerance)
	}
}

// TestTWRIgnoresDeposit is the single most important case here.
//
// A portfolio worth 100 grows 10% to 110, then receives a 100 deposit and ends
// at 210. It has returned 10%, not 110%. Any implementation that forgets to
// remove the deposit reports the latter — and reports it as investment skill.
func TestTWRIgnoresDeposit(t *testing.T) {
	in := PerformanceInput{
		Valuations: []Valuation{
			{Date: day(0), Value: dec("100")},
			{Date: day(1), Value: dec("110")},
			{Date: day(2), Value: dec("210")},
		},
		Flows: []Flow{{Date: day(2), Amount: dec("100")}},
	}

	perf, ok := ComputePerformance(in)
	if !ok {
		t.Fatal("ComputePerformance returned not-ok for a three-point series")
	}
	if perf.TWR == nil {
		t.Fatal("TWR is nil")
	}

	// (110/100) × ((210−100)/110) − 1 = 1.10 × 1.00 − 1 = 0.10
	approx(t, "TWR", *perf.TWR, "0.10", "0.0000001")

	// Gain must also strip the deposit: 210 − 100 − 100 = 10.
	if !perf.Gain.Equal(dec("10")) {
		t.Errorf("Gain = %s, want 10", perf.Gain)
	}
	if !perf.NetFlows.Equal(dec("100")) {
		t.Errorf("NetFlows = %s, want 100", perf.NetFlows)
	}
}

// TestMWRRespondsToDepositTiming pins the complementary property: where TWR is
// deliberately blind to when money arrived, IRR must not be. Same portfolio
// path, deposit early vs. late, must produce different money-weighted returns.
func TestMWRRespondsToDepositTiming(t *testing.T) {
	vals := []Valuation{
		{Date: day(0), Value: dec("1000")},
		{Date: day(180), Value: dec("1100")},
		{Date: day(365), Value: dec("2310")},
	}

	early, ok := ComputePerformance(PerformanceInput{
		Valuations: vals,
		Flows:      []Flow{{Date: day(180), Amount: dec("1000")}},
	})
	if !ok {
		t.Fatal("early: not computable")
	}
	late, ok := ComputePerformance(PerformanceInput{
		Valuations: []Valuation{
			{Date: day(0), Value: dec("1000")},
			{Date: day(180), Value: dec("1100")},
			{Date: day(365), Value: dec("2310")},
		},
		Flows: []Flow{{Date: day(364), Amount: dec("1000")}},
	})
	if !ok {
		t.Fatal("late: not computable")
	}

	if early.MWR == nil {
		t.Fatalf("early MWR is nil: %s", early.MWRNote)
	}
	if late.MWR == nil {
		t.Fatalf("late MWR is nil: %s", late.MWRNote)
	}
	if early.MWR.Equal(*late.MWR) {
		t.Errorf("MWR is identical (%s) for an early and a late deposit — "+
			"it is behaving like a time-weighted return", early.MWR)
	}

	// The same market path with money in it for less time must show a HIGHER
	// money-weighted return here: the late deposit rides the whole second-half
	// gain, so the investor's dollars earned more per dollar-year.
	if !late.MWR.GreaterThan(*early.MWR) {
		t.Errorf("late deposit MWR %s should exceed early deposit MWR %s",
			late.MWR, early.MWR)
	}
}

// TestMWRKnownAnswer checks the solver against a case anyone can verify by
// hand: 1000 in, 1100 out, exactly one year later. XIRR = 10%.
func TestMWRKnownAnswer(t *testing.T) {
	perf, ok := ComputePerformance(PerformanceInput{
		Valuations: []Valuation{
			{Date: day(0), Value: dec("1000")},
			{Date: day(365), Value: dec("1100")},
		},
	})
	if !ok {
		t.Fatal("not computable")
	}
	if perf.MWR == nil {
		t.Fatalf("MWR is nil: %s", perf.MWRNote)
	}
	approx(t, "MWR", *perf.MWR, "0.10", "0.00001")
}

// TestFullWithdrawal covers a portfolio emptied mid-period. The withdrawal is
// not a loss, and the return must not read as −100%.
func TestFullWithdrawal(t *testing.T) {
	perf, ok := ComputePerformance(PerformanceInput{
		Valuations: []Valuation{
			{Date: day(0), Value: dec("1000")},
			{Date: day(10), Value: dec("1200")},
			{Date: day(11), Value: dec("0")},
		},
		Flows: []Flow{{Date: day(11), Amount: dec("-1200")}},
	})
	if !ok {
		t.Fatal("not computable")
	}
	if perf.TWR == nil {
		t.Fatal("TWR is nil")
	}

	// (1200/1000) × ((0 − (−1200))/1200) − 1 = 1.2 × 1.0 − 1 = 0.20.
	approx(t, "TWR", *perf.TWR, "0.20", "0.0000001")

	// The market made 200; the other 1200 the user took out.
	if !perf.Gain.Equal(dec("200")) {
		t.Errorf("Gain = %s, want 200", perf.Gain)
	}
}

// TestMWRNotComputable pins the honest-refusal path. A series whose cash flows
// never change sign has no internal rate of return, and the function must say
// so rather than produce a number from the bracket edge.
func TestMWRNotComputable(t *testing.T) {
	// Opening value zero and closing value zero, with a single deposit: every
	// non-zero flow points the same way, so there is no root.
	perf, ok := ComputePerformance(PerformanceInput{
		Valuations: []Valuation{
			{Date: day(0), Value: dec("0")},
			{Date: day(30), Value: dec("0")},
		},
		Flows: []Flow{{Date: day(10), Amount: dec("500")}},
	})
	if !ok {
		t.Fatal("not computable at all — expected the performance shell to build")
	}
	if perf.MWR != nil {
		t.Errorf("MWR = %s, want nil for a series with no sign change", perf.MWR)
	}
	if perf.MWRNote == "" {
		t.Error("MWRNote is empty; a nil rate must always carry a reason")
	}
}

// TestTWRSkipsZeroBaseSubPeriod covers the account funded partway through the
// window. The zero-base day cannot express a return; including it would divide
// by zero, and treating the first deposit as growth would report an infinite
// gain on day one.
func TestTWRSkipsZeroBaseSubPeriod(t *testing.T) {
	perf, ok := ComputePerformance(PerformanceInput{
		Valuations: []Valuation{
			{Date: day(0), Value: dec("0")},
			{Date: day(1), Value: dec("1000")},
			{Date: day(2), Value: dec("1100")},
		},
		Flows: []Flow{{Date: day(1), Amount: dec("1000")}},
	})
	if !ok {
		t.Fatal("not computable")
	}
	if perf.TWR == nil {
		t.Fatal("TWR is nil")
	}
	// Only the second sub-period is linkable: 1100/1000 − 1 = 0.10.
	approx(t, "TWR", *perf.TWR, "0.10", "0.0000001")
}

// TestTooFewValuations: one point is a position, not a performance.
func TestTooFewValuations(t *testing.T) {
	if _, ok := ComputePerformance(PerformanceInput{
		Valuations: []Valuation{{Date: day(0), Value: dec("100")}},
	}); ok {
		t.Error("ComputePerformance accepted a single valuation")
	}
	if _, ok := ComputePerformance(PerformanceInput{}); ok {
		t.Error("ComputePerformance accepted an empty series")
	}
}

// TestAnnualisationRefusesShortSpans is a policy test, and the policy is the
// honest one: a 5% gain over three weeks is not an 87% annual return in any
// sense a reader would understand.
func TestAnnualisationRefusesShortSpans(t *testing.T) {
	short, ok := ComputePerformance(PerformanceInput{
		Valuations: []Valuation{
			{Date: day(0), Value: dec("1000")},
			{Date: day(21), Value: dec("1050")},
		},
	})
	if !ok {
		t.Fatal("not computable")
	}
	if short.Annualised != nil {
		t.Errorf("Annualised = %s for a 21-day span; it must be nil", short.Annualised)
	}

	// Two years at a total 21% is ~10% a year. (1.21)^(365/730) − 1 ≈ 0.1.
	long, ok := ComputePerformance(PerformanceInput{
		Valuations: []Valuation{
			{Date: day(0), Value: dec("1000")},
			{Date: day(730), Value: dec("1210")},
		},
	})
	if !ok {
		t.Fatal("not computable")
	}
	if long.Annualised == nil {
		t.Fatal("Annualised is nil for a two-year span")
	}
	approx(t, "Annualised", *long.Annualised, "0.10", "0.001")
}

// TestIsExternalFlow guards the classification the whole file depends on. A buy
// counted as an external flow would strip real market movement out of the
// return; a contribution missed would credit the market with the user's savings.
func TestIsExternalFlow(t *testing.T) {
	external := []struct{ txnType, subtype string }{
		{"cash", "deposit"},
		{"cash", "withdrawal"},
		{"cash", "contribution"},
		{"transfer", "transfer"},
		{"transfer", "distribution"},
	}
	for _, c := range external {
		if !IsExternalFlow(c.txnType, c.subtype) {
			t.Errorf("IsExternalFlow(%q, %q) = false, want true", c.txnType, c.subtype)
		}
	}

	internal := []struct{ txnType, subtype string }{
		{"buy", "buy"},
		{"sell", "sell"},
		{"cash", "dividend"},
		{"cash", "interest"},
		{"fee", "management fee"},
		// A "transfer" subtype under a buy is a reallocation inside the account,
		// not new money crossing the boundary.
		{"buy", "transfer"},
		{"cash", "qualified dividend"},
	}
	for _, c := range internal {
		if IsExternalFlow(c.txnType, c.subtype) {
			t.Errorf("IsExternalFlow(%q, %q) = true, want false", c.txnType, c.subtype)
		}
	}
}

// TestFlowsOutsideWindowIgnored: a flow on the opening valuation date is already
// inside the opening value, and counting it again would double it.
func TestFlowsOutsideWindowIgnored(t *testing.T) {
	perf, ok := ComputePerformance(PerformanceInput{
		Valuations: []Valuation{
			{Date: day(10), Value: dec("1000")},
			{Date: day(20), Value: dec("1100")},
		},
		Flows: []Flow{
			{Date: day(5), Amount: dec("999")},  // before the window
			{Date: day(10), Amount: dec("888")}, // on the opening valuation
			{Date: day(25), Amount: dec("777")}, // after the window
		},
	})
	if !ok {
		t.Fatal("not computable")
	}
	if !perf.NetFlows.IsZero() {
		t.Errorf("NetFlows = %s, want 0 — no flow falls inside the measured span", perf.NetFlows)
	}
	approx(t, "TWR", *perf.TWR, "0.10", "0.0000001")
}

// TestPortfolioGrowthSeriesStripsFlows: the benchmark chart must show market
// performance, not deposit size. A portfolio that doubled purely by deposit must
// plot flat against an index.
func TestPortfolioGrowthSeries(t *testing.T) {
	points := PortfolioGrowthSeries(
		[]Valuation{
			{Date: day(0), Value: dec("1000")},
			{Date: day(1), Value: dec("2000")},
			{Date: day(2), Value: dec("2200")},
		},
		[]Flow{{Date: day(1), Amount: dec("1000")}},
	)

	if len(points) != 3 {
		t.Fatalf("got %d points, want 3", len(points))
	}
	approx(t, "index[0]", points[0].Value, "100", "0.0001")
	// The deposit day is flat: (2000−1000)/1000 = 1.0.
	approx(t, "index[1]", points[1].Value, "100", "0.0001")
	// Then a real 10% gain.
	approx(t, "index[2]", points[2].Value, "110", "0.0001")
}
