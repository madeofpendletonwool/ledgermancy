package api

import (
	"encoding/json"
	"testing"
	"time"
)

// Every tool's input schema must be valid JSON, and the set must match what the
// executor dispatches on — a name drift would surface only as a live failure.
func TestChatToolDefs(t *testing.T) {
	defs := chatToolDefs()
	want := map[string]bool{
		"spending_summary":   true,
		"spend_by_category":  true,
		"top_merchants":      true,
		"budget_status":      true,
		"net_worth":          true,
		"recurring_charges":  true,
		"list_transactions":  true,
		"monthly_trend":      true,
		"category_averages":  true,
		"spending_by_day":    true,
		"query_transactions": true,
		"breakdown":          true,
	}
	if len(defs) != len(want) {
		t.Fatalf("got %d tools, want %d", len(defs), len(want))
	}
	for _, d := range defs {
		if !want[d.Name] {
			t.Errorf("unexpected tool %q", d.Name)
		}
		if d.Description == "" {
			t.Errorf("tool %q has no description", d.Name)
		}
		var schema map[string]any
		if err := json.Unmarshal(d.InputSchema, &schema); err != nil {
			t.Errorf("tool %q has invalid input schema: %v", d.Name, err)
		}
	}
}

func TestToolMonth(t *testing.T) {
	// An explicit month resolves to that month's day range.
	from, to, err := monthRange("2026-06")
	if err != nil {
		t.Fatalf("monthRange: %v", err)
	}
	if from.Format(time.DateOnly) != "2026-06-01" || to.Format(time.DateOnly) != "2026-06-30" {
		t.Errorf("range = %s..%s", from.Format(time.DateOnly), to.Format(time.DateOnly))
	}

	// Empty defaults to the current month rather than erroring.
	if _, _, err := monthRange(""); err != nil {
		t.Errorf("empty month should default, got %v", err)
	}

	// A malformed month is rejected.
	if _, _, err := monthRange("June"); err == nil {
		t.Error("expected error for malformed month")
	}
}

// toolMonth tolerates absent/empty input JSON, defaulting to the current month.
func TestToolMonthFromInput(t *testing.T) {
	if _, _, err := toolMonth(json.RawMessage(`{}`)); err != nil {
		t.Errorf("empty object: %v", err)
	}
	if _, _, err := toolMonth(json.RawMessage(`{"month":"2025-01"}`)); err != nil {
		t.Errorf("explicit month: %v", err)
	}
}

// toolMonths clamps to 1-24 and defaults to 12, so a missing or silly value
// never produces an empty or absurd trailing window.
func TestToolMonths(t *testing.T) {
	cases := map[string]int{
		`{}`:             12,
		`{"months":0}`:   12,
		`{"months":25}`:  12,
		`{"months":-3}`:  12,
		`{"months":1}`:   1,
		`{"months":6}`:   6,
		`{"months":24}`:  24,
		`{"months":"x"}`: 12, // malformed value ignored
	}
	for in, want := range cases {
		if got := toolMonths(json.RawMessage(in)); got != want {
			t.Errorf("toolMonths(%s) = %d, want %d", in, got, want)
		}
	}
}

// normalizeFlow defaults to spending, accepts the four known flows, and rejects
// anything else so a bad value fails loudly rather than silently querying all.
func TestNormalizeFlow(t *testing.T) {
	cases := map[string]string{"": "spending", "income": "income", "TRANSFERS": "transfers", " all ": "all", "spending": "spending"}
	for in, want := range cases {
		got, err := normalizeFlow(in)
		if err != nil {
			t.Errorf("normalizeFlow(%q): %v", in, err)
		}
		if got != want {
			t.Errorf("normalizeFlow(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := normalizeFlow("savings"); err == nil {
		t.Error("expected error for unknown flow")
	}
}

// normalizeGroupBy accepts the known dimensions (case-insensitively) and rejects
// empty (it is required) and unknown values.
func TestNormalizeGroupBy(t *testing.T) {
	for _, in := range []string{"category", "MERCHANT", "account", "month", "day", "pfc"} {
		if _, err := normalizeGroupBy(in); err != nil {
			t.Errorf("normalizeGroupBy(%q): %v", in, err)
		}
	}
	if _, err := normalizeGroupBy(""); err == nil {
		t.Error("expected error for empty group_by")
	}
	if _, err := normalizeGroupBy("payee"); err == nil {
		t.Error("expected error for unknown group_by")
	}
}

// toolDateRange resolves its window by precedence: explicit range, then month,
// then trailing months, then the current month.
func TestToolDateRange(t *testing.T) {
	// Explicit start/end wins and is returned verbatim.
	from, to, err := toolDateRange("2026-05", "2026-05-10", "2026-06-20", 6)
	if err != nil {
		t.Fatalf("toolDateRange: %v", err)
	}
	if from.Format(time.DateOnly) != "2026-05-10" || to.Format(time.DateOnly) != "2026-06-20" {
		t.Errorf("range = %s..%s", from.Format(time.DateOnly), to.Format(time.DateOnly))
	}

	// A reversed range is normalised.
	from, to, err = toolDateRange("", "2026-06-20", "2026-05-10", 0)
	if err != nil || to.Before(from) {
		t.Errorf("reversed range not normalised: %s..%s (%v)", from.Format(time.DateOnly), to.Format(time.DateOnly), err)
	}

	// One-sided range is an error.
	if _, _, err := toolDateRange("", "2026-06-20", "", 0); err == nil {
		t.Error("expected error for one-sided range")
	}

	// Month is used when no explicit range is given.
	from, to, err = toolDateRange("2026-06", "", "", 0)
	if err != nil || from.Format(time.DateOnly) != "2026-06-01" || to.Format(time.DateOnly) != "2026-06-30" {
		t.Errorf("month range = %s..%s (%v)", from.Format(time.DateOnly), to.Format(time.DateOnly), err)
	}

	// Malformed explicit date is rejected.
	if _, _, err := toolDateRange("", "May 10", "2026-06-20", 0); err == nil {
		t.Error("expected error for malformed start date")
	}

	// Trailing months and the empty default both land on month boundaries.
	if from, _, err := toolDateRange("", "", "", 3); err != nil || from.Day() != 1 {
		t.Errorf("trailing months should start on the 1st, got %s (%v)", from.Format(time.DateOnly), err)
	}
	if _, _, err := toolDateRange("", "", "", 0); err != nil {
		t.Errorf("empty inputs should default to current month, got %v", err)
	}
}

// trailingMonthsRange spans the current month plus the n-1 before it, always
// landing on the first and last calendar days.
func TestTrailingMonthsRange(t *testing.T) {
	from, to := trailingMonthsRange(12)
	if from.Day() != 1 {
		t.Errorf("from should be the 1st, got %s", from.Format(time.DateOnly))
	}
	// 12 months inclusive means the start is 11 months before the end's month.
	months := int(to.Year()-from.Year())*12 + int(to.Month()-from.Month())
	if months != 11 {
		t.Errorf("expected 11 months between start and end month, got %d", months)
	}
	if to.Before(from) {
		t.Errorf("to %s is before from %s", to.Format(time.DateOnly), from.Format(time.DateOnly))
	}
}
