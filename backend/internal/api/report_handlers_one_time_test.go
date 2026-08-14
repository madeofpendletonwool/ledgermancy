package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// The "Hide one-time charges" reader toggle (MAD-116). The toggle itself lives
// in the client; what these tests pin is the two things the server owes the
// reader for it — that the opt-in parameter resolves the same way `real` does
// (so the two reader-driven surfaces agree on what "on" means), and that the
// heatmap response carries the flag back so a filtered matrix can never render
// under an unfiltered label.

// TestExcludeOneTimeRequested mirrors TestRealRequested exactly. Anything
// unrecognised resolves to "keep one-time rows", because keeping them is the
// default every report was built on — a typo must not silently drop a payoff
// the household can see in their transactions list.
func TestExcludeOneTimeRequested(t *testing.T) {
	cases := map[string]bool{
		"":      false,
		"1":     true,
		"true":  true,
		"0":     false,
		"false": false,
		"yes":   false,
		"TRUE":  false, // deliberately strict; the clients send "true" or "1"
	}

	for value, want := range cases {
		r := httptest.NewRequest("GET", "/api/reports/heatmap?exclude_one_time="+value, nil)
		if got := excludeOneTimeRequested(r); got != want {
			t.Errorf("excludeOneTimeRequested(exclude_one_time=%q) = %v, want %v", value, got, want)
		}
	}

	// Absent entirely keeps one-time rows.
	if excludeOneTimeRequested(httptest.NewRequest(http.MethodGet, "/api/reports/heatmap", nil)) {
		t.Error("a request with no `exclude_one_time` parameter must keep one-time rows")
	}
}

// TestBuildHeatmapEchoesExcludeOneTime pins the load-bearing contract for the
// toggle: whatever the handler threaded in comes straight back on the response.
// The pivot does not consult the flag — it only echoes it — so the same rows
// feed both states, and the echo is the whole point.
func TestBuildHeatmapEchoesExcludeOneTime(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)
	cat := uuid.New()
	rows := []dbgen.GetCategoryMonthMatrixRow{
		matrixRow(cat, "Groceries", "groceries", "#abc", false, time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC), "100.00"),
	}

	t.Run("default keeps every row and echoes false", func(t *testing.T) {
		got := buildHeatmap(from, to, refNow, false, rows)
		if got.ExcludeOneTime {
			t.Error("exclude_one_time = true on the default response, want false")
		}
	})

	t.Run("the toggle on echoes true without touching the cells", func(t *testing.T) {
		// The pivot is flag-agnostic — the SQL has already filtered by the time
		// these rows arrive — so the same rows produce the same cell, and the
		// only thing that moves is the echo. That is the contract: the client
		// re-asks with the flag and gets different ROWS, not a different pivot.
		got := buildHeatmap(from, to, refNow, true, rows)
		if !got.ExcludeOneTime {
			t.Error("exclude_one_time = false after the toggle was asked for, want true")
		}
		if c := got.Categories[0].Cells["2026-01"]; c != "100" {
			t.Errorf("cell changed with the flag = %q, want 100 — the pivot must not consult it", c)
		}
	})
}
