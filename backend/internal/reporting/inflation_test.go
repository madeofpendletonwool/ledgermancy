package reporting

import (
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// Doc 27's verification list for the deflation helper, all of it against
// hand-computed fixtures and none of it against a database.
//
// The published CPI-U values used below are real (BLS CUUR0000SA0) and match
// the seed in migration 00052, so a fixture drifting from the shipped series is
// a test failure rather than a quiet disagreement.

func month(y int, m time.Month) time.Time {
	return time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
}

// `dec` lives in returns_test.go; the two files share this package.

// testSeries mirrors a slice of the shipped seed, including the October 2025
// hole. Every case below leans on that hole being genuinely absent.
func testSeries() *CPISeries {
	return NewCPISeries([]CPIPoint{
		{Period: month(2015, time.June), Index: dec("238.638")},
		{Period: month(2020, time.June), Index: dec("257.797")},
		{Period: month(2025, time.September), Index: dec("324.800")},
		// October 2025 deliberately absent: BLS never published it.
		{Period: month(2025, time.November), Index: dec("324.122")},
		{Period: month(2025, time.December), Index: dec("324.054")},
		{Period: month(2026, time.June), Index: dec("333.952")},
	})
}

func TestRealKnownPair(t *testing.T) {
	s := testSeries()

	// $1,000 in June 2020 dollars, expressed in June 2026 dollars:
	//   1000 × 333.952 / 257.797 = 1295.406..., to the cent 1295.41
	got, err := Real(dec("1000"), month(2020, time.June), month(2026, time.June), s)
	if err != nil {
		t.Fatalf("Real: %v", err)
	}
	want := dec("1295.41")
	if !got.Equal(want) {
		t.Errorf("Real(1000, 2020-06 → 2026-06) = %s, want %s", got, want)
	}

	// And the other direction: today's $1,000 buys what $771.96 bought in 2020.
	back, err := Real(dec("1000"), month(2026, time.June), month(2020, time.June), s)
	if err != nil {
		t.Fatalf("Real reverse: %v", err)
	}
	if want := dec("771.96"); !back.Equal(want) {
		t.Errorf("Real(1000, 2026-06 → 2020-06) = %s, want %s", back, want)
	}
}

// A same-period conversion must be the identity EXACTLY, including for a value
// no rounding could preserve. The ratio is 1, so nothing may touch the number.
func TestRealSamePeriodIsIdentity(t *testing.T) {
	s := testSeries()

	for _, in := range []string{"1000", "0", "-4321.99", "0.005", "123456789.123456789"} {
		got, err := Real(dec(in), month(2026, time.June), month(2026, time.June), s)
		if err != nil {
			t.Fatalf("Real(%s): %v", in, err)
		}
		if !got.Equal(dec(in)) {
			t.Errorf("same-period Real(%s) = %s, want the input unchanged", in, got)
		}
	}

	// The day within the month must not matter either — periods normalise.
	got, err := Real(dec("500"), time.Date(2026, time.June, 30, 23, 0, 0, 0, time.UTC),
		month(2026, time.June), s)
	if err != nil {
		t.Fatalf("Real mid-month: %v", err)
	}
	if !got.Equal(dec("500")) {
		t.Errorf("mid-month same-period Real = %s, want 500", got)
	}
}

// The rule the whole feature rests on: a missing index ERRORS. It must never
// return the nominal figure, because a real number that is quietly nominal
// cannot be detected by anything downstream.
func TestRealMissingIndexErrorsRatherThanPassingThrough(t *testing.T) {
	s := testSeries()

	cases := []struct {
		name       string
		from, base time.Time
	}{
		{"the October 2025 hole", month(2025, time.October), month(2026, time.June)},
		{"before the series begins", month(1999, time.March), month(2026, time.June)},
		{"after the series ends", month(2030, time.January), month(2026, time.June)},
		{"base month missing", month(2020, time.June), month(2025, time.October)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Real(dec("1000"), tc.from, tc.base, s)
			if err == nil {
				t.Fatalf("Real returned %s and no error; a missing index must fail", got)
			}
			var missing ErrNoIndex
			if !errors.As(err, &missing) {
				t.Fatalf("error is %T (%v), want ErrNoIndex", err, err)
			}
			if !got.IsZero() {
				t.Errorf("value on error = %s, want zero — never the nominal figure", got)
			}
		})
	}
}

// An empty series is a real deployment state (a seed that did not load) and
// must degrade rather than panic.
func TestRealOnEmptySeries(t *testing.T) {
	s := NewCPISeries(nil)
	if !s.Empty() {
		t.Fatal("NewCPISeries(nil) should be empty")
	}
	if _, err := Real(dec("1000"), month(2026, time.June), month(2026, time.June), s); err == nil {
		t.Error("Real on an empty series must error")
	}
	if _, ok := s.BasePeriod(month(2026, time.June)); ok {
		t.Error("an empty series has no base period")
	}
}

// The base period falls back to the newest PUBLISHED month, because the current
// month never has an index — BLS publishes mid-following-month.
func TestBasePeriodFallsBackToLatestPublished(t *testing.T) {
	s := testSeries()

	// August 2026: nothing published for it, so June 2026 is the base.
	base, ok := s.BasePeriod(month(2026, time.August))
	if !ok {
		t.Fatal("BasePeriod not ok")
	}
	if !base.Equal(month(2026, time.June)) {
		t.Errorf("base = %s, want 2026-06", base.Format("2006-01"))
	}

	// The exact month, when it exists, is used as-is.
	base, _ = s.BasePeriod(month(2025, time.December))
	if !base.Equal(month(2025, time.December)) {
		t.Errorf("base = %s, want 2025-12", base.Format("2006-01"))
	}

	// A clock predating the whole series has no honest base.
	if _, ok := s.BasePeriod(month(2001, time.January)); ok {
		t.Error("a date before the series begins must not resolve a base period")
	}
}

// Gaps names the interior holes, so a chart can explain a missing point rather
// than drawing through it.
func TestGapsFindsInteriorHolesOnly(t *testing.T) {
	gaps := testSeries().Gaps()

	found := map[string]bool{}
	for _, g := range gaps {
		found[g.Format("2006-01")] = true
	}
	if !found["2025-10"] {
		t.Error("October 2025 should be reported as a gap")
	}
	// Everything between the fixture's sparse points is a gap too, but nothing
	// outside its span may be.
	for _, g := range gaps {
		if g.Before(month(2015, time.June)) || g.After(month(2026, time.June)) {
			t.Errorf("gap %s lies outside the series span", g.Format("2006-01"))
		}
	}
}

// Staleness fires at two months, not one: one month behind is the NORMAL state
// of a healthy series, and flagging it would train users to ignore the flag.
func TestStaleAllowsTheNormalOneMonthLag(t *testing.T) {
	s := testSeries() // latest published: 2026-06

	for _, tc := range []struct {
		now       time.Time
		wantStale bool
	}{
		{month(2026, time.June), false},
		{month(2026, time.July), false},   // normal publication lag
		{month(2026, time.August), false}, // exactly at the threshold
		{month(2026, time.September), true},
		{month(2027, time.January), true},
	} {
		if got := s.Stale(tc.now); got != tc.wantStale {
			t.Errorf("Stale(%s) = %v, want %v", tc.now.Format("2006-01"), got, tc.wantStale)
		}
	}
}

// RealRate must divide, not subtract. Subtraction is the approximation everyone
// reaches for and it is always wrong in the flattering direction.
func TestRealRateDividesRatherThanSubtracting(t *testing.T) {
	// 20% nominal, 6% inflation. Subtraction says 14%; the truth is 13.2075%.
	got := RealRate(dec("0.20"), dec("0.06"))
	if want := dec("0.1320755"); got.Round(7).Cmp(want) != 0 {
		t.Errorf("RealRate(0.20, 0.06) = %s, want ~%s", got.Round(7), want)
	}
	if got.Equal(dec("0.14")) {
		t.Error("RealRate subtracted; it must divide")
	}

	// Zero inflation leaves the nominal rate untouched.
	if got := RealRate(dec("0.07"), decimal.Zero); !got.Equal(dec("0.07")) {
		t.Errorf("RealRate(0.07, 0) = %s, want 0.07", got)
	}

	// A nominal gain smaller than inflation is a real LOSS, which is the whole
	// point of the feature.
	if got := RealRate(dec("0.02"), dec("0.06")); !got.IsNegative() {
		t.Errorf("RealRate(0.02, 0.06) = %s, want a negative real return", got)
	}
}

func TestChangeAndAnnualisedChange(t *testing.T) {
	s := testSeries()

	// 2015-06 → 2020-06: 257.797 / 238.638 − 1 = 0.08029...
	change, err := s.Change(month(2015, time.June), month(2020, time.June))
	if err != nil {
		t.Fatalf("Change: %v", err)
	}
	if want := dec("0.0802848"); change.Round(7).Cmp(want) != 0 {
		t.Errorf("Change = %s, want ~%s", change.Round(7), want)
	}

	// Compounded over five years, not divided by five: (1.0803)^(1/5) − 1.
	annual, ok := s.AnnualisedChange(month(2015, time.June), month(2020, time.June))
	if !ok {
		t.Fatal("AnnualisedChange not ok over five years")
	}
	if want := dec("0.0155"); annual.Round(4).Cmp(want) != 0 {
		t.Errorf("AnnualisedChange = %s, want ~%s", annual.Round(4), want)
	}

	// Under a year is refused rather than annualised.
	if _, ok := s.AnnualisedChange(month(2025, time.November), month(2025, time.December)); ok {
		t.Error("AnnualisedChange must refuse a span under a year")
	}
	// A missing month is refused, not interpolated around.
	if _, err := s.Change(month(2025, time.October), month(2026, time.June)); err == nil {
		t.Error("Change across the October 2025 hole must fail")
	}
}

func TestYearToDateAnchorsOnDecember(t *testing.T) {
	s := testSeries()

	// Base June 2026, anchored on December 2025: 333.952 / 324.054 − 1.
	rate, from, to, ok := s.YearToDate(month(2026, time.July))
	if !ok {
		t.Fatal("YearToDate not ok")
	}
	if !from.Equal(month(2025, time.December)) {
		t.Errorf("from = %s, want 2025-12", from.Format("2006-01"))
	}
	if !to.Equal(month(2026, time.June)) {
		t.Errorf("to = %s, want 2026-06", to.Format("2006-01"))
	}
	if want := dec("0.0305"); rate.Round(4).Cmp(want) != 0 {
		t.Errorf("YTD rate = %s, want ~%s", rate.Round(4), want)
	}
}

// A duplicate period keeps the LAST value, matching the table's upsert: a BLS
// revision supersedes what it revises.
func TestNewCPISeriesRevisionWins(t *testing.T) {
	s := NewCPISeries([]CPIPoint{
		{Period: month(2026, time.June), Index: dec("333.000")},
		{Period: month(2026, time.June), Index: dec("333.952")},
	})
	got, ok := s.Index(month(2026, time.June))
	if !ok {
		t.Fatal("index missing")
	}
	if !got.Equal(dec("333.952")) {
		t.Errorf("index = %s, want the revised 333.952", got)
	}
}

// A non-positive index is not a price level. Dropping it here is what stops
// Real() dividing by zero further down.
func TestNewCPISeriesRejectsNonPositiveIndex(t *testing.T) {
	s := NewCPISeries([]CPIPoint{
		{Period: month(2026, time.May), Index: decimal.Zero},
		{Period: month(2026, time.June), Index: dec("-1")},
	})
	if !s.Empty() {
		t.Error("a series of non-positive indices must be empty")
	}
}

func TestDeflatePerformance(t *testing.T) {
	s := testSeries()

	twr := dec("0.40")
	mwr := dec("0.08")
	annualised := dec("0.0696")
	perf := Performance{
		Start:      month(2015, time.June),
		End:        month(2020, time.June),
		Days:       1827,
		TWR:        &twr,
		Annualised: &annualised,
		MWR:        &mwr,
	}

	real, ok := s.Deflate(perf)
	if !ok {
		t.Fatal("Deflate not ok")
	}

	// The real total return is (1.40 / 1.0802891) − 1 = 0.29596...
	if want := dec("0.2960"); real.TWR.Round(4).Cmp(want) != 0 {
		t.Errorf("real TWR = %s, want ~%s", real.TWR.Round(4), want)
	}
	if real.TWR.Cmp(twr) >= 0 {
		t.Error("real TWR must be below nominal when prices rose")
	}

	// MWR is already annualised, so it is deflated by the ANNUALISED price
	// change (~1.55%), not by the five-year total (~8%). Deflating by the total
	// would give ~0.0%; the right answer is ~6.35%.
	if real.MWR == nil {
		t.Fatal("real MWR missing over a five-year span")
	}
	if want := dec("0.0635"); real.MWR.Round(4).Cmp(want) != 0 {
		t.Errorf("real MWR = %s, want ~%s (annualised deflator, not the total)",
			real.MWR.Round(4), want)
	}

	// A span reaching outside the series is refused outright.
	perf.Start = month(1999, time.January)
	if _, ok := s.Deflate(perf); ok {
		t.Error("Deflate must refuse a span whose endpoint has no index")
	}

	// Nothing to deflate when there was no nominal return.
	perf = Performance{Start: month(2015, time.June), End: month(2020, time.June)}
	if _, ok := s.Deflate(perf); ok {
		t.Error("Deflate must refuse a Performance with no TWR")
	}
}
