package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// Every tool's input schema must be valid JSON, and the set must match what the
// executor dispatches on — a name drift would surface only as a live failure.
func TestChatToolDefs(t *testing.T) {
	defs := chatBaseToolDefs()
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

// toolMonths clamps an out-of-range window to 1-24 and defaults a missing one
// to 12, so neither produces an empty or absurd trailing window. A window it
// cannot READ is a different case and must surface as an error: defaulting it
// to 12 would answer a question about three months with a year of figures and
// leave no trace that it had done so.
func TestToolMonths(t *testing.T) {
	cases := map[string]int{
		`{}`:            12,
		`{"months":0}`:  12,
		`{"months":25}`: 12,
		`{"months":-3}`: 12,
		`{"months":1}`:  1,
		`{"months":6}`:  6,
		`{"months":24}`: 24,
	}
	for in, want := range cases {
		got, err := toolMonths(json.RawMessage(in))
		if err != nil {
			t.Errorf("toolMonths(%s) unexpected error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("toolMonths(%s) = %d, want %d", in, got, want)
		}
	}

	for _, in := range []string{`{"months":"x"}`, `not json`, `{"months":[3]}`} {
		if _, err := toolMonths(json.RawMessage(in)); err == nil {
			t.Errorf("toolMonths(%s) = nil error, want a retryable decode error", in)
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

// trendWindow is the resolver behind the monthly_trend tool. It is the fix for
// the "redo the average without July" question the advisor could not answer:
// the window can now be a custom range, and named months can be excluded from
// both the series and the average. These cases pin the precedence (explicit
// range over trailing months), the whole-month snap, the reversal handling, and
// the exclude parsing — including the errors that have to surface so a bad
// argument does not quietly answer a different question.
func TestTrendWindow(t *testing.T) {
	// Default: trailing twelve months, landing on whole-month boundaries, no
	// exclusions. This is the behaviour every existing caller relied on.
	from, to, exclude, err := trendWindow(json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("empty input: %v", err)
	}
	if from.Day() != 1 || exclude != nil {
		t.Errorf("default window should start on the 1st with nil exclude: %s, %v", from, exclude)
	}
	if _, exp := trailingMonthsRange(12); to != exp {
		t.Errorf("default window should match trailing twelve months' end, got %s", to.Format(time.DateOnly))
	}

	// An explicit months count overrides 12 but is still a trailing window.
	from, _, _, err = trendWindow(json.RawMessage(`{"months":3}`))
	if err != nil {
		t.Fatalf("months input: %v", err)
	}
	if exp, _ := trailingMonthsRange(3); from != exp {
		t.Errorf("months=3 should match trailing three months' start, got %s", from.Format(time.DateOnly))
	}

	// An explicit start+end wins over a months count and is snapped to whole
	// months — the case the advisor needs to answer "the last year excluding
	// the current partial month".
	from, to, exclude, err = trendWindow(json.RawMessage(`{"months":3,"start":"2025-09-01","end":"2026-06-30"}`))
	if err != nil {
		t.Fatalf("custom range: %v", err)
	}
	if from.Format(time.DateOnly) != "2025-09-01" || to.Format(time.DateOnly) != "2026-06-30" {
		t.Errorf("custom range = %s..%s", from.Format(time.DateOnly), to.Format(time.DateOnly))
	}
	if exclude != nil {
		t.Errorf("custom range should have nil exclude, got %v", exclude)
	}

	// Mid-month boundaries are snapped to the first and last of their months,
	// because the tool reports whole months.
	from, to, _, err = trendWindow(json.RawMessage(`{"start":"2025-09-15","end":"2026-06-10"}`))
	if err != nil || from.Format(time.DateOnly) != "2025-09-01" || to.Format(time.DateOnly) != "2026-06-30" {
		t.Errorf("mid-month snap = %s..%s (%v)", from.Format(time.DateOnly), to.Format(time.DateOnly), err)
	}

	// A reversed range is normalised to whole months AFTER the swap, so "from"
	// stays a first-of-month rather than the last day of the later month.
	from, to, _, err = trendWindow(json.RawMessage(`{"start":"2026-06-30","end":"2025-09-01"}`))
	if err != nil || from.Format(time.DateOnly) != "2025-09-01" || to.Format(time.DateOnly) != "2026-06-30" {
		t.Errorf("reversed range = %s..%s (%v)", from.Format(time.DateOnly), to.Format(time.DateOnly), err)
	}

	// exclude parses YYYY-MM entries into a set, over the default window. This
	// is the exact shape of the "without this month and last month" follow-up.
	_, _, exclude, err = trendWindow(json.RawMessage(`{"exclude":["2026-07","2026-08"]}`))
	if err != nil {
		t.Fatalf("exclude input: %v", err)
	}
	if !exclude["2026-07"] || !exclude["2026-08"] || len(exclude) != 2 {
		t.Errorf("exclude set = %v", exclude)
	}

	// Duplicates collapse and empty entries are ignored, leaving the set the
	// caller named rather than one padded with blanks.
	_, _, exclude, err = trendWindow(json.RawMessage(`{"exclude":["2026-07", " 2026-07 ", ""]}`))
	if err != nil {
		t.Fatalf("exclude dedupe input: %v", err)
	}
	if len(exclude) != 1 || !exclude["2026-07"] {
		t.Errorf("exclude should dedupe and drop blanks, got %v", exclude)
	}

	// Errors that must surface rather than be defaulted to twelve months.
	for _, in := range []string{
		`{"start":"2026-06-30"}`,              // one-sided range
		`{"start":"June","end":"2026-06-30"}`, // malformed date
		`{"exclude":["July"]}`,                // malformed exclude month
		`{"months":"x"}`,                      // unreadable months
		`not json`,                            // not json
		`{"exclude":"2026-07"}`,               // exclude as string, not array
	} {
		if _, _, _, err := trendWindow(json.RawMessage(in)); err == nil {
			t.Errorf("trendWindow(%s) = nil error, want a retryable error", in)
		}
	}
}

// monthlyTrendSeries computes avg_leftover in exact decimal over EXACTLY the
// months it returns — the property that lets the advisor quote the average
// verbatim. Excluding a month has to flow through the same average or the
// reference line and the months it spans disagree, so this is the case that
// matters most: the dropped month leaves the total and the count together.
func TestMonthlyTrendSeries(t *testing.T) {
	// Three months with leftover 100 / 200 / 300.
	rows := []dbgen.GetMonthlyTrendRow{
		{Month: time.Date(2025, 9, 1, 0, 0, 0, 0, time.UTC), Income: decimal.NewFromInt(1000), Spending: decimal.NewFromInt(900)},
		{Month: time.Date(2025, 10, 1, 0, 0, 0, 0, time.UTC), Income: decimal.NewFromInt(1000), Spending: decimal.NewFromInt(800)},
		{Month: time.Date(2025, 11, 1, 0, 0, 0, 0, time.UTC), Income: decimal.NewFromInt(1000), Spending: decimal.NewFromInt(700)},
	}

	// No exclusions: average is the mean of 100, 200, 300.
	months, avg, hasAvg := monthlyTrendSeries(rows, nil)
	if !hasAvg || avg != "200.00" {
		t.Errorf("avg = %q (hasAvg=%v), want 200.00", avg, hasAvg)
	}
	if len(months) != 3 || months[0]["leftover"] != "100.00" {
		t.Errorf("months = %+v", months)
	}

	// Exclude the 300 month: average is now the mean of 100 and 200.
	months, avg, hasAvg = monthlyTrendSeries(rows, map[string]bool{"2025-11": true})
	if !hasAvg || avg != "150.00" {
		t.Errorf("avg after exclude = %q, want 150.00", avg)
	}
	if len(months) != 2 {
		t.Errorf("expected 2 months after exclude, got %d", len(months))
	}
	for _, m := range months {
		if m["month"] == "2025-11" {
			t.Errorf("excluded month 2025-11 should not appear in the series")
		}
	}

	// An empty row set returns no average — the model has nothing to quote, so
	// it says so plainly rather than narrating a zero.
	months, avg, hasAvg = monthlyTrendSeries(nil, nil)
	if hasAvg || avg != "" || len(months) != 0 {
		t.Errorf("empty series = %+v avg=%q hasAvg=%v, want no average", months, avg, hasAvg)
	}

	// An exclude set naming a month that is not in the series is a no-op.
	months, avg, hasAvg = monthlyTrendSeries(rows, map[string]bool{"1999-01": true})
	if !hasAvg || avg != "200.00" || len(months) != 3 {
		t.Errorf("non-matching exclude should be a no-op: avg=%q months=%d", avg, len(months))
	}

	// Excluding EVERY month leaves no average, because dividing by zero months
	// is not a figure this app can quote.
	months, avg, hasAvg = monthlyTrendSeries(rows, map[string]bool{
		"2025-09": true, "2025-10": true, "2025-11": true,
	})
	if hasAvg || avg != "" || len(months) != 0 {
		t.Errorf("all-excluded series should yield no average: avg=%q months=%d", avg, len(months))
	}
}
