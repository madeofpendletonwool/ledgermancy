package obligations

import (
	"testing"

	"github.com/shopspring/decimal"
)

// TestCadenceForGapDays pins the detector-gap → cadence mapping. The boundaries
// matter more than the buckets: a monthly bill whose charges average 28 or 32
// days apart must map to "monthly", not to something adjacent, because the
// cadence is what every future due date is generated from.
func TestCadenceForGapDays(t *testing.T) {
	cases := []struct {
		name  string
		gap   string
		want  Cadence
		valid bool
	}{
		{"weekly", "7", Cadence{1, UnitWeek}, true},
		{"weekly jitter high", "9.9", Cadence{1, UnitWeek}, true},
		{"biweekly", "14", Cadence{2, UnitWeek}, true},
		{"biweekly jitter", "15.5", Cadence{2, UnitWeek}, true},
		{"monthly short month", "28", Cadence{1, UnitMonth}, true},
		{"monthly long month", "31", Cadence{1, UnitMonth}, true},
		{"monthly late payer", "34", Cadence{1, UnitMonth}, true},
		{"quarterly", "91", Cadence{3, UnitMonth}, true},
		{"twice a year", "182", Cadence{6, UnitMonth}, true},
		{"annual", "365", Cadence{1, UnitYear}, true},
		// Nothing recognisable: a bill on the calendar that is wrong is worse
		// than one that is missing, so these are not promoted at all.
		{"too frequent", "1", Cadence{}, false},
		{"too sparse", "800", Cadence{}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := CadenceForGapDays(decimal.RequireFromString(tc.gap))
			if ok != tc.valid {
				t.Fatalf("CadenceForGapDays(%s) valid = %v, want %v", tc.gap, ok, tc.valid)
			}
			if ok && got != tc.want {
				t.Errorf("CadenceForGapDays(%s) = %+v, want %+v", tc.gap, got, tc.want)
			}
		})
	}
}

// TestMonthlyEstimate checks the normalisation used to rank and total bills.
// Month and year cadences divide exactly; day and week cadences use the mean
// Gregorian month so a weekly bill and a yearly one stay comparable.
func TestMonthlyEstimate(t *testing.T) {
	cases := []struct {
		name    string
		amount  string
		cadence Cadence
		want    string
	}{
		{"monthly is itself", "1200.00", Cadence{1, UnitMonth}, "1200.00"},
		{"quarterly thirds", "300.00", Cadence{3, UnitMonth}, "100.00"},
		{"annual twelfths", "1200.00", Cadence{1, UnitYear}, "100.00"},
		{"biennial twenty-fourths", "2400.00", Cadence{2, UnitYear}, "100.00"},
		// 100 * 30.4375 / 7
		{"weekly", "100.00", Cadence{1, UnitWeek}, "434.82"},
		// 50 * 30.4375 / 14
		{"biweekly", "50.00", Cadence{2, UnitWeek}, "108.71"},
		{"zero count is zero", "100.00", Cadence{0, UnitMonth}, "0.00"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MonthlyEstimate(decimal.RequireFromString(tc.amount), tc.cadence)
			if !got.Equal(decimal.RequireFromString(tc.want)) {
				t.Errorf("MonthlyEstimate(%s, %+v) = %s, want %s",
					tc.amount, tc.cadence, got, tc.want)
			}
		})
	}
}

// TestCadenceMaxDays guards the bound NextDue relies on. It must never be
// shorter than a real period, or the search window would miss an occurrence and
// the UI would show "no next due" for a live bill.
func TestCadenceMaxDays(t *testing.T) {
	cases := []struct {
		cadence Cadence
		want    int
	}{
		{Cadence{1, UnitDay}, 1},
		{Cadence{2, UnitWeek}, 14},
		{Cadence{1, UnitMonth}, 31},
		{Cadence{3, UnitMonth}, 93},
		{Cadence{2, UnitYear}, 732},
	}
	for _, tc := range cases {
		if got := tc.cadence.MaxDays(); got != tc.want {
			t.Errorf("%+v.MaxDays() = %d, want %d", tc.cadence, got, tc.want)
		}
	}
}
