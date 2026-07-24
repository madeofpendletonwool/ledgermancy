package api

import (
	"testing"
	"time"
)

// monthsInclusive underpins the envelope carryover, so pin its edges: same month
// is 1, and it counts across year boundaries.
func TestMonthsInclusive(t *testing.T) {
	m := func(y int, mo time.Month) time.Time {
		return time.Date(y, mo, 1, 0, 0, 0, 0, time.UTC)
	}
	cases := []struct {
		start, target time.Time
		want          int
	}{
		{m(2026, time.May), m(2026, time.May), 1},           // first month
		{m(2026, time.May), m(2026, time.July), 3},          // May, June, July
		{m(2025, time.November), m(2026, time.February), 4}, // across the year
		{m(2026, time.July), m(2026, time.May), -1},         // target before start
	}
	for _, c := range cases {
		if got := monthsInclusive(c.start, c.target); got != c.want {
			t.Errorf("monthsInclusive(%s, %s) = %d, want %d",
				c.start.Format("2006-01"), c.target.Format("2006-01"), got, c.want)
		}
	}
}
