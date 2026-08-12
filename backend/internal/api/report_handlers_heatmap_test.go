package api

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// buildHeatmap pivots the per-(category, month) rows the SQL returns into the
// matrix the heatmap and small-multiples charts consume. The pivot itself is
// the part worth pinning without a database: the month axis, the ranking by
// whole-range total, and the per-month cell keys.
//
// All money here is hand-set decimal; no floats anywhere in the path.

func strPtr(s string) *string { return &s }

// refNow is the clock the pivot tests below run against. It sits AFTER every
// window they use, so none of them has an in-progress column and the pivot
// assertions stay about the pivot alone. The marker gets its own test.
var refNow = time.Date(2027, 6, 15, 0, 0, 0, 0, time.UTC)

// TestBuildHeatmapEmpty verifies the empty state: no rows means an empty
// category list but a STILL-COMPLETE month axis, so the chart renders the
// columns it asked for rather than collapsing to nothing.
func TestBuildHeatmapEmpty(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC)

	got := buildHeatmap(from, to, refNow, false, nil)

	if len(got.Categories) != 0 {
		t.Errorf("empty rows = %d categories, want 0", len(got.Categories))
	}
	wantMonths := []string{"2026-01", "2026-02", "2026-03"}
	if len(got.Months) != len(wantMonths) {
		t.Fatalf("months = %v, want %v", got.Months, wantMonths)
	}
	for i, m := range got.Months {
		if m != wantMonths[i] {
			t.Errorf("months[%d] = %s, want %s", i, m, wantMonths[i])
		}
	}
	if got.From != "2026-01-01" || got.To != "2026-03-31" {
		t.Errorf("from/to = %s/%s, want 2026-01-01/2026-03-31", got.From, got.To)
	}
}

// TestBuildHeatmapPivotsAndRanks builds a small three-category matrix across
// three months and checks: the month axis spans the range, categories appear
// once each, totals are the whole-range sum, cells are keyed by "YYYY-MM", and
// the ranking is by total descending.
func TestBuildHeatmapPivotsAndRanks(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC)

	groceries := uuid.New()
	dining := uuid.New()
	rent := uuid.New()

	rows := []dbgen.GetCategoryMonthMatrixRow{
		// Groceries: 100 + 0 + 200 = 300.
		matrixRow(groceries, "Groceries", "groceries", "#abc", false, time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC), "100.00"),
		matrixRow(groceries, "Groceries", "groceries", "#abc", false, time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC), "200.00"),
		// Dining: 50 + 50 + 50 = 150.
		matrixRow(dining, "Dining", "dining", "#def", false, time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC), "50.00"),
		matrixRow(dining, "Dining", "dining", "#def", false, time.Date(2026, 2, 18, 0, 0, 0, 0, time.UTC), "50.00"),
		matrixRow(dining, "Dining", "dining", "#def", false, time.Date(2026, 3, 22, 0, 0, 0, 0, time.UTC), "50.00"),
		// Rent: fixed, 1000 + 1000 = 2000 — should rank first.
		matrixRow(rent, "Rent", "rent", "#f00", true, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), "1000.00"),
		matrixRow(rent, "Rent", "rent", "#f00", true, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), "1000.00"),
	}

	got := buildHeatmap(from, to, refNow, false, rows)

	if len(got.Months) != 3 || got.Months[0] != "2026-01" || got.Months[2] != "2026-03" {
		t.Fatalf("months = %v, want 2026-01..2026-03", got.Months)
	}

	if len(got.Categories) != 3 {
		t.Fatalf("categories = %d, want 3", len(got.Categories))
	}

	// Ranking: rent (2000) > groceries (300) > dining (150).
	wantOrder := []string{"Rent", "Groceries", "Dining"}
	wantTotals := []string{"2000", "300", "150"}
	for i, c := range got.Categories {
		if c.Name != wantOrder[i] {
			t.Errorf("categories[%d].name = %s, want %s", i, c.Name, wantOrder[i])
		}
		// decimal.String() of "2000.00" is "2000", and of "1000.00"+"1000.00"
		// is also "2000" — decimal canonicalises. The handler returns
		// .String() (canonical), not the input form.
		if c.Total != wantTotals[i] {
			t.Errorf("categories[%d].total = %s, want %s", i, c.Total, wantTotals[i])
		}
	}

	// Groceries is index 1 after ranking. Its cells should be Jan and Mar only —
	// the empty Feb is omitted, not zero-filled (the client fills the gaps).
	groceriesRow := got.Categories[1]
	if groceriesRow.Cells["2026-01"] != "100" {
		t.Errorf("groceries Jan = %q, want 100", groceriesRow.Cells["2026-01"])
	}
	if groceriesRow.Cells["2026-03"] != "200" {
		t.Errorf("groceries Mar = %q, want 200", groceriesRow.Cells["2026-03"])
	}
	if _, ok := groceriesRow.Cells["2026-02"]; ok {
		t.Error("groceries Feb should be absent (no spend that month), not zero-filled server-side")
	}
	if groceriesRow.IsFixed {
		t.Error("groceries should not be fixed")
	}

	// Rent should carry the fixed flag through.
	rentRow := got.Categories[0]
	if !rentRow.IsFixed {
		t.Error("rent should be fixed")
	}
	if rentRow.Cells["2026-02"] != "1000" {
		t.Errorf("rent Feb = %q, want 1000", rentRow.Cells["2026-02"])
	}
}

// TestBuildHeatmapAccumulatesDuplicateRows pins the contract that two rows for
// the same (category, month) are summed, not overwritten. date_trunc('month')
// already groups in SQL, but the pivot must not silently drop a row if a future
// caller feeds it pre-grouped data.
func TestBuildHeatmapAccumulatesDuplicateRows(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)

	cat := uuid.New()
	rows := []dbgen.GetCategoryMonthMatrixRow{
		matrixRow(cat, "Groceries", "groceries", "#abc", false, time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC), "30.00"),
		matrixRow(cat, "Groceries", "groceries", "#abc", false, time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC), "70.00"),
	}

	got := buildHeatmap(from, to, refNow, false, rows)

	if len(got.Categories) != 1 {
		t.Fatalf("categories = %d, want 1", len(got.Categories))
	}
	c := got.Categories[0]
	if c.Total != "100" {
		t.Errorf("total = %s, want 100 (30 + 70 accumulated)", c.Total)
	}
	if c.Cells["2026-01"] != "100" {
		t.Errorf("Jan cell = %s, want 100", c.Cells["2026-01"])
	}
}

// TestBuildHeatmapReversedRange returns an empty month axis when from > to,
// rather than spinning — a corrupt range must fail closed.
func TestBuildHeatmapReversedRange(t *testing.T) {
	from := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	got := buildHeatmap(from, to, refNow, false, nil)
	if len(got.Months) != 0 {
		t.Errorf("reversed range months = %v, want empty", got.Months)
	}
}

func matrixRow(id uuid.UUID, name, slug, color string, fixed bool, month time.Time, total string) dbgen.GetCategoryMonthMatrixRow {
	return dbgen.GetCategoryMonthMatrixRow{
		CategoryID:    id,
		CategoryName:  name,
		CategorySlug:  slug,
		CategoryColor: strPtr(color),
		IsFixed:       fixed,
		Month:         month,
		Total:         decimal.RequireFromString(total),
	}
}
