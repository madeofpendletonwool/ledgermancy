package jobs

import (
	"testing"
	"time"
)

func TestDigestDue(t *testing.T) {
	// 2026-07 calendar: Jul 1 is a Wednesday; Jul 6 and Jul 13 are Mondays.
	cases := []struct {
		name       string
		cadence    string
		now        time.Time
		wantDue    bool
		wantPeriod string
	}{
		{"weekly on Monday is due", "weekly", date(2026, 7, 13, 9), true, "2026-W29"},
		{"weekly mid-week is not due", "weekly", date(2026, 7, 15, 9), false, "2026-W29"},
		{"weekly on Sunday is not due", "weekly", date(2026, 7, 12, 9), false, "2026-W28"},
		{"monthly on the 1st is due, covers prior month", "monthly", date(2026, 7, 1, 9), true, "2026-06"},
		{"monthly on the 2nd is due", "monthly", date(2026, 7, 2, 9), true, "2026-06"},
		{"monthly on the 3rd is not due", "monthly", date(2026, 7, 3, 9), false, "2026-06"},
		{"unknown cadence falls back to weekly gating", "", date(2026, 7, 13, 9), true, "2026-W29"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			due, period := digestDue(c.cadence, c.now)
			if due != c.wantDue {
				t.Errorf("due = %v, want %v", due, c.wantDue)
			}
			if period != c.wantPeriod {
				t.Errorf("period = %q, want %q", period, c.wantPeriod)
			}
		})
	}
}

func TestDigestWindow(t *testing.T) {
	now := date(2026, 7, 13, 9)

	t.Run("monthly covers the completed prior month and is cacheable", func(t *testing.T) {
		monthDate, from, to, label, cacheable := digestWindow("monthly", now)
		if !cacheable {
			t.Error("monthly window should be cacheable")
		}
		if got := monthDate.Format("2006-01-02"); got != "2026-06-01" {
			t.Errorf("monthDate = %s, want 2026-06-01", got)
		}
		if got := from.Format("2006-01-02"); got != "2026-06-01" {
			t.Errorf("from = %s, want 2026-06-01", got)
		}
		if got := to.Format("2006-01-02"); got != "2026-06-30" {
			t.Errorf("to = %s, want 2026-06-30", got)
		}
		if label != "June 2026" {
			t.Errorf("label = %q, want June 2026", label)
		}
	})

	t.Run("weekly covers current month-to-date and is NOT cacheable", func(t *testing.T) {
		monthDate, from, to, label, cacheable := digestWindow("weekly", now)
		if cacheable {
			t.Error("weekly window must not be cacheable — a partial month can't overwrite the full-month cache")
		}
		if got := monthDate.Format("2006-01-02"); got != "2026-07-01" {
			t.Errorf("monthDate = %s, want 2026-07-01", got)
		}
		if got := from.Format("2006-01-02"); got != "2026-07-01" {
			t.Errorf("from = %s, want 2026-07-01", got)
		}
		if !to.Equal(now.UTC()) {
			t.Errorf("to = %s, want now (month-to-date)", to)
		}
		if label != "July 2026" {
			t.Errorf("label = %q, want July 2026", label)
		}
	})
}

func date(y int, m time.Month, d, h int) time.Time {
	return time.Date(y, m, d, h, 0, 0, 0, time.UTC)
}
