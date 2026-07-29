package jobs

import (
	"testing"
	"time"
)

func day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 12, 0, 0, 0, time.UTC)
}

// PeriodStart is the idempotency key for allowance posting, so the property
// that matters is STABILITY: every day inside a period must map to the same
// boundary. If it does not, a job running on Tuesday pays again for a week it
// already paid on Monday.
func TestPeriodStartIsStableWithinAPeriod(t *testing.T) {
	t.Run("weekly anchors on Monday", func(t *testing.T) {
		// 2026-07-27 is a Monday.
		want := day(2026, time.July, 27).Truncate(24 * time.Hour)
		for d := 27; d <= 31; d++ { // Mon–Fri
			got := PeriodStart("weekly", day(2026, time.July, d))
			if !got.Equal(want) {
				t.Errorf("weekly start for Jul %d = %s, want %s",
					d, got.Format(time.DateOnly), want.Format(time.DateOnly))
			}
		}
		// The following Monday starts a new period, or nobody is ever paid twice.
		next := PeriodStart("weekly", day(2026, time.August, 3))
		if next.Equal(want) {
			t.Error("a new week did not start a new period")
		}
	})

	t.Run("monthly anchors on the first", func(t *testing.T) {
		want := time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC)
		for _, d := range []int{1, 15, 31} {
			if got := PeriodStart("monthly", day(2026, time.July, d)); !got.Equal(want) {
				t.Errorf("monthly start for Jul %d = %s, want %s",
					d, got.Format(time.DateOnly), want.Format(time.DateOnly))
			}
		}
	})

	t.Run("biweekly is stable across its fortnight", func(t *testing.T) {
		start := PeriodStart("biweekly", day(2026, time.July, 27))
		// Every day in the following 14 must map to the same boundary.
		for i := range 14 {
			got := PeriodStart("biweekly", day(2026, time.July, 27).AddDate(0, 0, i))
			if !got.Equal(start) {
				t.Fatalf("biweekly drifted on day %d: %s vs %s",
					i, got.Format(time.DateOnly), start.Format(time.DateOnly))
			}
		}
		// And day 15 must not.
		if PeriodStart("biweekly", day(2026, time.July, 27).AddDate(0, 0, 14)).Equal(start) {
			t.Error("biweekly period did not advance after 14 days")
		}
	})

	t.Run("weekly boundary lands on Monday for a Sunday", func(t *testing.T) {
		// Go's Weekday puts Sunday at 0, so a naive offset sends Sunday
		// forward into the next week instead of back to its own Monday.
		sunday := day(2026, time.August, 2)
		got := PeriodStart("weekly", sunday)
		want := time.Date(2026, time.July, 27, 0, 0, 0, 0, time.UTC)
		if !got.Equal(want) {
			t.Errorf("Sunday mapped to %s, want the preceding Monday %s",
				got.Format(time.DateOnly), want.Format(time.DateOnly))
		}
	})
}

// TestPeriodStartIsDeterministic: the same input must always give the same
// answer, since a wobbling boundary would silently double-pay.
func TestPeriodStartIsDeterministic(t *testing.T) {
	for _, cadence := range []string{"weekly", "biweekly", "monthly"} {
		when := day(2026, time.July, 29)
		first := PeriodStart(cadence, when)
		for range 10 {
			if got := PeriodStart(cadence, when); !got.Equal(first) {
				t.Fatalf("%s: %s != %s", cadence,
					got.Format(time.DateOnly), first.Format(time.DateOnly))
			}
		}
	}
}
