package reporting

import (
	"testing"
	"time"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/obligations"
)

func TestNetExtraSpend(t *testing.T) {
	cases := []struct {
		name                     string
		medianSpend, knownMonth string
		want                    string
	}{
		{"typical case: some spend is not yet tracked", "3000", "2000", "1000"},
		{"tracked obligations cover everything", "2000", "2000", "0"},
		{"tracked obligations exceed the trailing median: clamp at zero, not negative",
			"1000", "2000", "0"},
		{"nothing tracked yet", "1800", "0", "1800"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := netExtraSpend(dec(tc.medianSpend), dec(tc.knownMonth))
			if !got.Equal(dec(tc.want)) {
				t.Errorf("netExtraSpend(%s, %s) = %s, want %s", tc.medianSpend, tc.knownMonth, got, tc.want)
			}
		})
	}
}

func TestDaysInMonth(t *testing.T) {
	cases := []struct {
		name string
		date time.Time
		want int
	}{
		{"31-day month", time.Date(2026, time.January, 15, 0, 0, 0, 0, time.UTC), 31},
		{"30-day month", time.Date(2026, time.April, 1, 0, 0, 0, 0, time.UTC), 30},
		{"February, non-leap year", time.Date(2026, time.February, 10, 0, 0, 0, 0, time.UTC), 28},
		{"February, leap year", time.Date(2028, time.February, 10, 0, 0, 0, 0, time.UTC), 29},
		{"December rolls into next January correctly", time.Date(2026, time.December, 31, 0, 0, 0, 0, time.UTC), 31},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := daysInMonth(tc.date); got != tc.want {
				t.Errorf("daysInMonth(%s) = %d, want %d", tc.date.Format(time.DateOnly), got, tc.want)
			}
		})
	}
}

// TestEstimateSeriesNoDrift pins the base case: with no income history the
// caller passes a zero drift, and the estimate must equal the known balance
// exactly (Points already reflect Due), not merely track it.
func TestEstimateSeriesNoDrift(t *testing.T) {
	points := []obligations.Point{
		{Date: date(2026, 1, 1), Balance: dec("1000"), Due: dec("0")},
		{Date: date(2026, 1, 2), Balance: dec("900"), Due: dec("100")},
	}
	got := estimateSeries(dec("1000"), points, dec("0"))
	want := []string{"1000", "900"}
	for i, w := range want {
		if !got[i].Equal(dec(w)) {
			t.Errorf("point %d = %s, want %s", i, got[i], w)
		}
	}
}

// TestEstimateSeriesSpreadsDriftPerDay checks the daily amortization: a flat
// $310/month net drift over a 31-day January should add exactly $10/day, and
// still subtract the same Due amounts the known line already used.
func TestEstimateSeriesSpreadsDriftPerDay(t *testing.T) {
	points := []obligations.Point{
		{Date: date(2026, 1, 1), Balance: dec("1000"), Due: dec("0")},
		{Date: date(2026, 1, 2), Balance: dec("1000"), Due: dec("0")},
		{Date: date(2026, 1, 3), Balance: dec("900"), Due: dec("100")}, // a bill clears
	}
	got := estimateSeries(dec("1000"), points, dec("310")) // 310/31 = 10/day
	want := []string{"1010", "1020", "930"}                // day3: 1020 + 10 - 100
	for i, w := range want {
		if !got[i].Equal(dec(w)) {
			t.Errorf("point %d = %s, want %s", i, got[i], w)
		}
	}
}

// TestEstimateSeriesCrossesMonthBoundary checks that the per-day rate
// recomputes when the horizon crosses into a shorter or longer month, rather
// than using one day-count for the whole series.
func TestEstimateSeriesCrossesMonthBoundary(t *testing.T) {
	points := []obligations.Point{
		{Date: date(2026, 1, 31), Balance: dec("1000"), Due: dec("0")}, // Jan: 31 days
		{Date: date(2026, 2, 1), Balance: dec("1000"), Due: dec("0")},  // Feb 2026: 28 days
	}
	got := estimateSeries(dec("1000"), points, dec("310"))
	// Jan 31: +310/31 = +10 -> 1010
	// Feb 1:  +310/28 ≈ +11.0714... rounded to 2dp -> 1021.07
	if !got[0].Equal(dec("1010")) {
		t.Errorf("Jan 31 = %s, want 1010", got[0])
	}
	if !got[1].Equal(dec("1021.07")) {
		t.Errorf("Feb 1 = %s, want 1021.07", got[1])
	}
}

func date(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}
