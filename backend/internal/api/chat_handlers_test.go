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
		"spending_summary":  true,
		"spend_by_category": true,
		"top_merchants":     true,
		"budget_status":     true,
		"net_worth":         true,
		"recurring_charges": true,
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
