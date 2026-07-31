package insights

import (
	"testing"

	"github.com/shopspring/decimal"
)

// These tests take no database. The threshold logic is the one part of anomaly
// detection that lives in Go rather than SQL, and it is the part most worth
// pinning: the choice of a median anchor over a mean one is the whole design,
// and TestP95GateFiresWhereMeanPlus3SigmaWouldNot is what makes that choice
// checkable rather than asserted.

func dec(s string) decimal.Decimal {
	d, err := decimal.NewFromString(s)
	if err != nil {
		panic(err)
	}
	return d
}

func TestOutlierThreshold(t *testing.T) {
	cases := []struct {
		name        string
		median, p95 string
		sensitivity string
		want        string
	}{
		// Ordinary merchant: p95 is close to the median, so it never beats the
		// median multiple and the median anchor decides.
		{"tight spread, balanced", "40", "45", "balanced", "120"},
		{"tight spread, conservative", "40", "45", "conservative", "160"},
		{"tight spread, sensitive", "40", "45", "sensitive", "80"},

		// Legitimately wide merchant (a grocery store): p95 is 3× the median,
		// inside outlierCap, so it RAISES the bar and stops every big shop
		// reading as an outlier. 190 × 1.5 = 285 beats 60 × 3 = 180.
		{"wide but ordinary, balanced", "60", "190", "balanced", "285"},

		// Contaminated sample: p95 is 15× the median, past outlierCap, so it is
		// discarded and the median anchor stands. This is the case that keeps
		// one historical freak charge from disabling the detector.
		{"contaminated sample, balanced", "40", "600", "balanced", "120"},

		// Exactly at the cap: 5× is still admitted (the boundary is inclusive),
		// so 200 × 1.5 = 300 beats 40 × 3 = 120.
		{"p95 exactly at cap", "40", "200", "balanced", "300"},

		// One cent past the cap: discarded.
		{"p95 one cent past cap", "40", "200.01", "balanced", "120"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := outlierThreshold(dec(c.median), dec(c.p95), sensitivityByName(c.sensitivity))
			if !got.Equal(dec(c.want)) {
				t.Fatalf("outlierThreshold(%s, %s, %s) = %s, want %s",
					c.median, c.p95, c.sensitivity, got, c.want)
			}
		})
	}
}

// TestP95GateFiresWhereMeanPlus3SigmaWouldNot is the reason the thresholds are
// shaped the way they are, so the contrast is asserted directly rather than
// left as a comment.
//
// The merchant: eleven $40 charges plus one historical $600 one. A new $300
// charge arrives — six times the typical bill, and exactly what the detector
// exists to surface.
//
// mean+3σ over that history is $571.80, so a mean-anchored gate stays silent.
// It stays silent because of the $600 charge — the single data point most like
// the thing being detected. The median ignores it and fires at $120.
func TestP95GateFiresWhereMeanPlus3SigmaWouldNot(t *testing.T) {
	// Hand-computed over the leave-one-out history: 11 × $40 + 1 × $600.
	const (
		median = "40"
		p95    = "600"
		mean   = "86.67" // 1040 / 12, to cents
		stddev = "161.71"
	)
	candidate := dec("300")

	// What a mean+3σ gate would have said.
	meanPlus3Sigma := dec(mean).Add(dec(stddev).Mul(decimal.NewFromInt(3)))
	if !meanPlus3Sigma.Equal(dec("571.80")) {
		t.Fatalf("fixture drift: mean+3σ = %s, want 571.80", meanPlus3Sigma)
	}
	if candidate.GreaterThanOrEqual(meanPlus3Sigma) {
		t.Fatalf("fixture is not the interesting case: %s already clears mean+3σ of %s",
			candidate, meanPlus3Sigma)
	}

	// What this design says. p95 of 600 is 15× the median, past outlierCap, so
	// it is discarded as contamination and the median anchor decides.
	s := balancedSensitivity()
	got := outlierThreshold(dec(median), dec(p95), s)
	if !got.Equal(dec("120")) {
		t.Fatalf("outlierThreshold = %s, want 120", got)
	}
	if candidate.LessThan(got) {
		t.Fatalf("candidate %s does not clear threshold %s — the detector would stay silent",
			candidate, got)
	}
	if candidate.LessThan(s.outlierFloor) {
		t.Fatalf("candidate %s is below the dollar floor %s", candidate, s.outlierFloor)
	}
}

func TestSensitivityByName(t *testing.T) {
	cases := []struct {
		in         string
		wantName   string
		wantMin    int64
		wantFloor  string
		wantMedian string
	}{
		{"conservative", "conservative", 8, "100", "4"},
		{"balanced", "balanced", 5, "50", "3"},
		{"sensitive", "sensitive", 5, "25", "2"},
		// Unset and unrecognised both fall back rather than disabling detection.
		{"", "balanced", 5, "50", "3"},
		{"aggressive", "balanced", 5, "50", "3"},
		{"BALANCED", "balanced", 5, "50", "3"},
	}

	for _, c := range cases {
		t.Run("in="+c.in, func(t *testing.T) {
			s := sensitivityByName(c.in)
			if s.name != c.wantName {
				t.Fatalf("name = %q, want %q", s.name, c.wantName)
			}
			if s.minSamples != c.wantMin {
				t.Fatalf("minSamples = %d, want %d", s.minSamples, c.wantMin)
			}
			if !s.outlierFloor.Equal(dec(c.wantFloor)) {
				t.Fatalf("outlierFloor = %s, want %s", s.outlierFloor, c.wantFloor)
			}
			if !s.medianMultiple.Equal(dec(c.wantMedian)) {
				t.Fatalf("medianMultiple = %s, want %s", s.medianMultiple, c.wantMedian)
			}
		})
	}
}

// No sensitivity may set minSamples below the value largeTransactionProducer
// yields at, or a merchant would fall between the two producers and neither
// would report it.
func TestNoSensitivityDropsBelowTheHandoffFloor(t *testing.T) {
	for _, name := range []string{"conservative", "balanced", "sensitive", ""} {
		if got := sensitivityByName(name).minSamples; got < outlierMinSamplesFloor {
			t.Fatalf("sensitivity %q has minSamples %d, below the handoff floor %d — "+
				"merchants with %d-%d charges would be reported by neither producer",
				name, got, outlierMinSamplesFloor, got, outlierMinSamplesFloor-1)
		}
	}
}

func TestAnomalyPriorityBands(t *testing.T) {
	cases := []struct {
		amount string
		pushAt decimal.Decimal
		want   int
	}{
		// Outlier bands.
		{"49.99", outlierPushAt, 3},
		{"149.99", outlierPushAt, 3},
		{"150.00", outlierPushAt, 4}, // exact push boundary
		{"499.99", outlierPushAt, 4},
		{"500.00", outlierPushAt, 5},
		{"900.00", outlierPushAt, 5},

		// Duplicate bands push earlier: a duplicate is an error to dispute, not
		// merely a large purchase, and the totals are smaller.
		{"99.99", duplicatePushAt, 3},
		{"100.00", duplicatePushAt, 4}, // exact push boundary
		{"898.00", duplicatePushAt, 5},
	}

	// The feed's push floor. Mirrored rather than imported: jobs imports
	// insights, so insights cannot import jobs.
	const insightPushMinPriorityForTest = 4

	for _, c := range cases {
		got := anomalyPriority(dec(c.amount), c.pushAt)
		if got != c.want {
			t.Fatalf("anomalyPriority(%s, pushAt=%s) = %d, want %d",
				c.amount, c.pushAt, got, c.want)
		}
		pushes := got >= insightPushMinPriorityForTest
		wantPushes := c.want >= insightPushMinPriorityForTest
		if pushes != wantPushes {
			t.Fatalf("amount %s: pushes = %v, want %v", c.amount, pushes, wantPushes)
		}
	}
}
