package api

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// The in-progress marker (MAD-110). A trailing window always ends on the current
// month, and until this flag existed nothing in the payload said so — so the
// charts divided a whole month's spending by a fraction of a month's income and
// fitted an axis to the result.
//
// These tests pin the two things that can go wrong with the predicate: failing to
// flag a month that IS running, and flagging a window that merely runs up to
// today. The second is the one that would do real damage, because the financial
// summary report asks the same endpoint for a rolling year ending now.

// TestSummaryFlagsTheUnfinishedMonthOnly is the live bug and its blast radius in
// one table. August 2026 viewed on the 12th is month-to-date; the same August
// viewed in September is a finished month; and a trailing year ending on the 12th
// is neither, however much of "now" it contains.
func TestSummaryFlagsTheUnfinishedMonthOnly(t *testing.T) {
	now := time.Date(2026, 8, 12, 9, 30, 0, 0, time.UTC)

	cases := []struct {
		name       string
		from, to   time.Time
		wantFlag   bool
		wantAsOf   string
		wantReason string
	}{
		{
			name:       "the calendar month now falls in",
			from:       time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
			to:         time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC),
			wantFlag:   true,
			wantAsOf:   "2026-08-12",
			wantReason: "this is the month-to-date window the Spending page opens on",
		},
		{
			name:       "a finished month",
			from:       time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
			to:         time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC),
			wantReason: "July is over; nothing about it is provisional",
		},
		{
			name:       "a month that has not started",
			from:       time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
			to:         time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC),
			wantReason: "a future month is empty, not in progress",
		},
		{
			// The regression this predicate exists to avoid. Report.tsx asks for
			// a rolling 365 days ending today; `now` is inside that window, but
			// its one partial month is diluted across eleven whole ones and the
			// trailing-twelve savings rate is a perfectly good figure.
			name:       "a rolling year that ends today",
			from:       time.Date(2025, 8, 12, 0, 0, 0, 0, time.UTC),
			to:         time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC),
			wantReason: "a trailing-twelve window is not a month-to-date window",
		},
		{
			name:       "a quarter containing today",
			from:       time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
			to:         time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC),
			wantReason: "a multi-month window is not one unfinished month",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildSummary(tc.from, tc.to, now, dbgen.GetSpendingSummaryRow{
				Income:   dec("3.81"),
				Spending: dec("2049.87"),
			})

			if got.InProgress != tc.wantFlag {
				t.Errorf("in_progress = %v, want %v — %s", got.InProgress, tc.wantFlag, tc.wantReason)
			}
			if got.AsOf != tc.wantAsOf {
				t.Errorf("as_of = %q, want %q", got.AsOf, tc.wantAsOf)
			}
		})
	}
}

// TestSummaryKeepsTheSavingsRateOnAnUnfinishedMonth pins the deliberate choice
// NOT to null the ratio server-side. The recap, the digest and the chat all read
// savings_rate; redefining it to fix one tile would move all three. The month-to-
// date rate is a real ratio over a real window — InProgress is what tells a
// consumer it is not a month's savings rate.
func TestSummaryKeepsTheSavingsRateOnAnUnfinishedMonth(t *testing.T) {
	now := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	from := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)

	got := buildSummary(from, to, now, dbgen.GetSpendingSummaryRow{
		Income:   dec("3.81"),
		Spending: dec("2049.87"),
	})

	if !got.InProgress {
		t.Fatal("in_progress = false for the current month; the rest of this test is meaningless")
	}
	if got.SavingsRate == nil {
		t.Fatal("savings_rate was nulled server-side; it must keep its meaning and let the client decide not to render it")
	}
	// -2046.06 / 3.81, rounded to four places — the figure behind the -53,702%
	// the Spending page used to print.
	if want := "-537.0236"; got.SavingsRate.String() != want {
		t.Errorf("savings_rate = %s, want %s", got.SavingsRate, want)
	}
	if got.Leftover.String() != "-2046.06" {
		t.Errorf("leftover = %s, want -2046.06 — the dollars are real and must not move", got.Leftover)
	}
}

// TestHeatmapMarksTheRunningColumn covers the other half: the matrix has to name
// which of its columns is a part-month so the client can keep it out of the
// colour ramp's ceiling and label it.
func TestHeatmapMarksTheRunningColumn(t *testing.T) {
	cat := uuid.New()
	rows := []dbgen.GetCategoryMonthMatrixRow{
		matrixRow(cat, "Groceries", "groceries", "#abc", false, time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC), "500.00"),
		matrixRow(cat, "Groceries", "groceries", "#abc", false, time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC), "180.00"),
	}

	t.Run("a window reaching the present names its last column", func(t *testing.T) {
		got := buildHeatmap(
			time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
			time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC),
			time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC),
			rows,
		)
		if got.InProgressMonth != "2026-08" {
			t.Errorf("in_progress_month = %q, want 2026-08", got.InProgressMonth)
		}
		if got.AsOf != "2026-08-12" {
			t.Errorf("as_of = %q, want 2026-08-12", got.AsOf)
		}
	})

	t.Run("a window ending in the past names nothing", func(t *testing.T) {
		got := buildHeatmap(
			time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
			time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC),
			time.Date(2027, 1, 9, 0, 0, 0, 0, time.UTC),
			rows,
		)
		if got.InProgressMonth != "" {
			t.Errorf("in_progress_month = %q for a window that is entirely in the past, want empty", got.InProgressMonth)
		}
		if got.AsOf != "" {
			t.Errorf("as_of = %q, want empty", got.AsOf)
		}
	})

	t.Run("the flag never invents a column outside the axis", func(t *testing.T) {
		// `now` is after the window but the pivot must resolve against the
		// rendered months, not against `to`, or a custom historical range would
		// gain a column the reader cannot see.
		got := buildHeatmap(
			time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC),
			time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC),
			nil,
		)
		if got.InProgressMonth != "" {
			t.Errorf("in_progress_month = %q, want empty — 2026-08 is not on this axis", got.InProgressMonth)
		}
	})
}
