package db

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/reporting"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/testdb"
)

// Doc 27's verification list for the shipped series, against a real Postgres.
//
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' go test ./internal/db/

func cpiQueries(t *testing.T) (context.Context, *pgxpool.Pool, *dbgen.Queries) {
	t.Helper()

	url := testdb.URL(t)

	ctx := context.Background()
	pool, err := Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return ctx, pool, dbgen.New(pool)
}

// The seed is what makes CPI_FETCH_ENABLED safe to default off, so its coverage
// is a shipped promise rather than an implementation detail.
func TestCPISeedCoversTheDocumentedSpan(t *testing.T) {
	ctx, _, q := cpiQueries(t)

	series, err := reporting.LoadCPISeries(ctx, q)
	if err != nil {
		t.Fatalf("load cpi series: %v", err)
	}
	if series.Empty() {
		t.Fatal("the seeded series is empty; migration 00052 did not load")
	}

	earliest, _ := series.Earliest()
	latest, _ := series.Latest()

	// Doc 27 asks for at least fifteen years. The seed starts in January 2010.
	if want := time.Date(2010, time.January, 1, 0, 0, 0, 0, time.UTC); !earliest.Equal(want) {
		t.Errorf("series starts %s, want %s", earliest.Format("2006-01"), want.Format("2006-01"))
	}
	if years := latest.Year() - earliest.Year(); years < 15 {
		t.Errorf("series spans %d years, want at least 15", years)
	}

	// Every index must be a positive price level, and every period the first of
	// its month — the table CHECKs both, so a failure here means the seed was
	// edited past the constraint.
	rows, err := q.ListCPISeries(ctx)
	if err != nil {
		t.Fatalf("list cpi series: %v", err)
	}
	for _, r := range rows {
		if !r.IndexValue.IsPositive() {
			t.Errorf("%s has a non-positive index %s", r.Period.Format("2006-01"), r.IndexValue)
		}
		if r.Period.Day() != 1 {
			t.Errorf("period %s is not a first-of-month date", r.Period.Format(time.DateOnly))
		}
	}
}

// The one gap is real, permanent, and load-bearing: October 2025 was never
// published (the 2025 lapse in appropriations) and BLS will not estimate it.
// If somebody "fixes" the seed by interpolating it, this fails — which is the
// point. Nothing else in the span may be missing.
func TestCPISeedHasExactlyTheOctober2025Hole(t *testing.T) {
	ctx, _, q := cpiQueries(t)

	series, err := reporting.LoadCPISeries(ctx, q)
	if err != nil {
		t.Fatalf("load cpi series: %v", err)
	}

	gaps := series.Gaps()
	if len(gaps) != 1 {
		var names []string
		for _, g := range gaps {
			names = append(names, g.Format("2006-01"))
		}
		t.Fatalf("series has %d gaps (%v), want exactly one (2025-10)", len(gaps), names)
	}
	if got := gaps[0].Format("2006-01"); got != "2025-10" {
		t.Errorf("gap is %s, want 2025-10", got)
	}

	// And the consequence that matters: a figure dated in that month refuses to
	// deflate rather than passing through nominal.
	base, ok := series.BasePeriod(time.Now().UTC())
	if !ok {
		t.Fatal("no base period")
	}
	if _, err := reporting.Real(decimal.NewFromInt(1000), gaps[0], base, series); err == nil {
		t.Error("deflating a figure dated in the gap must fail, not interpolate")
	}
}

// BLS revises. An insert-only job would keep serving the superseded figure
// forever, so the query upserts — and re-running it with the same value is a
// no-op rather than a duplicate.
func TestUpsertCPIPointAppliesARevision(t *testing.T) {
	ctx, pool, q := cpiQueries(t)

	// A period well outside the seeded span, so the test owns it outright.
	period := time.Date(2099, time.March, 1, 0, 0, 0, 0, time.UTC)
	// DELETE, not upsert-back-to-1. Rewriting the row leaves it in the shared
	// test database forever, and TestCPISeedHasExactlyTheOctober2025Hole then
	// scans for gaps all the way out to 2099 and reports 873 of them. The
	// cleanup has to remove what the test added, not overwrite it.
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM cpi_series WHERE period = $1`, period)
	})

	first, err := q.UpsertCPIPoint(ctx, dbgen.UpsertCPIPointParams{
		Period: period, IndexValue: decimal.RequireFromString("400.1234"),
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	revised, err := q.UpsertCPIPoint(ctx, dbgen.UpsertCPIPointParams{
		Period: period, IndexValue: decimal.RequireFromString("400.5678"),
	})
	if err != nil {
		t.Fatalf("revise: %v", err)
	}

	if revised.ID != first.ID {
		t.Error("the revision created a second row; it must update in place")
	}
	if want := decimal.RequireFromString("400.5678"); !revised.IndexValue.Equal(want) {
		t.Errorf("index after revision = %s, want %s", revised.IndexValue, want)
	}
}

// A household whose history predates the series must degrade with an error the
// caller can report, not a wrong number. This is the "history predating the
// seed" case from doc 27's verification list.
func TestDeflatingBeforeTheSeriesBeginsRefuses(t *testing.T) {
	ctx, _, q := cpiQueries(t)

	series, err := reporting.LoadCPISeries(ctx, q)
	if err != nil {
		t.Fatalf("load cpi series: %v", err)
	}
	base, ok := series.BasePeriod(time.Now().UTC())
	if !ok {
		t.Fatal("no base period")
	}

	old := time.Date(2004, time.July, 15, 0, 0, 0, 0, time.UTC)
	got, err := reporting.Real(decimal.NewFromInt(50000), old, base, series)
	if err == nil {
		t.Fatalf("deflating a 2004 figure returned %s; the seed starts in 2010", got)
	}
	if !got.IsZero() {
		t.Errorf("value on error = %s, want zero", got)
	}
}
