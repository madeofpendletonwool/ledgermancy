package search

import (
	"testing"
	"time"
)

// A fixed reference so "today" is a known day. 2026-03-15 is a Sunday, chosen
// deliberately: Go's Weekday puts Sunday at 0, so a week that starts on Monday is
// exactly where naive arithmetic goes wrong.
var ref = time.Date(2026, time.March, 15, 13, 45, 0, 0, time.UTC)

func TestParseDate(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		// Absolute.
		{"2026-01-01", "2026-01-01"},
		{"2019-12-31", "2019-12-31"},

		// Named days.
		{"today", "2026-03-15"},
		{"now", "2026-03-15"},
		{"yesterday", "2026-03-14"},
		{"tomorrow", "2026-03-16"},
		{"  TODAY  ", "2026-03-15"},

		// Relative. No sign means the past, which is what a search box means by
		// "30d" every time.
		{"-30d", "2026-02-13"},
		{"30d", "2026-02-13"},
		{"+30d", "2026-04-14"},
		{"-2w", "2026-03-01"},
		{"-6m", "2025-09-15"},
		{"-1y", "2025-03-15"},
		{"+1y", "2027-03-15"},

		// Period edges. All three separator spellings are the same keyword, so a
		// user can avoid quoting.
		{"start of this month", "2026-03-01"},
		{"start-of-this-month", "2026-03-01"},
		{"start_of_this_month", "2026-03-01"},
		{"end of this month", "2026-03-31"},
		{"start of last month", "2026-02-01"},
		// February, which is the month day-28 arithmetic gets wrong.
		{"end of last month", "2026-02-28"},
		{"start of next month", "2026-04-01"},
		{"end of next month", "2026-04-30"},
		{"start of this year", "2026-01-01"},
		{"end of this year", "2026-12-31"},
		{"start of last year", "2025-01-01"},
		{"end of last year", "2025-12-31"},
		// The reference day is a Sunday, so its Monday-based week began the 9th.
		{"start of this week", "2026-03-09"},
		{"end of this week", "2026-03-15"},
		{"start of last week", "2026-03-02"},
		{"end of last week", "2026-03-08"},
	} {
		got, err := ParseDate(tc.in, ref)
		if err != nil {
			t.Errorf("ParseDate(%q): %v", tc.in, err)
			continue
		}
		if s := got.Format(time.DateOnly); s != tc.want {
			t.Errorf("ParseDate(%q) = %s, want %s", tc.in, s, tc.want)
		}
	}
}

// A Monday reference, so the week edges are checked from the other end of the
// off-by-one they invite.
func TestParseDateWeekFromMonday(t *testing.T) {
	monday := time.Date(2026, time.March, 16, 8, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ in, want string }{
		{"start of this week", "2026-03-16"},
		{"end of this week", "2026-03-22"},
		{"start of last week", "2026-03-09"},
	} {
		got, err := ParseDate(tc.in, monday)
		if err != nil {
			t.Fatalf("ParseDate(%q): %v", tc.in, err)
		}
		if s := got.Format(time.DateOnly); s != tc.want {
			t.Errorf("ParseDate(%q) from a Monday = %s, want %s", tc.in, s, tc.want)
		}
	}
}

func TestParseDateRejects(t *testing.T) {
	for _, in := range []string{
		"",
		"banana",
		"2026-13-45",
		"middle of this month",
		"start of this fortnight",
		"end of every month",
		"-30",  // no unit
		"-30q", // no such unit
		"01/02/2026",
	} {
		if got, err := ParseDate(in, ref); err == nil {
			t.Errorf("ParseDate(%q) = %s, want an error", in, got.Format(time.DateOnly))
		}
	}
}

// The clock is stripped: a date term is about a calendar day, and t.date is a
// DATE. Anything else would make `on:today` depend on the time of day.
func TestParseDateStripsTheClock(t *testing.T) {
	got, err := ParseDate("today", ref)
	if err != nil {
		t.Fatalf("ParseDate: %v", err)
	}
	if h, m, s := got.Clock(); h != 0 || m != 0 || s != 0 {
		t.Errorf("ParseDate(today) = %s, want midnight", got)
	}
}
