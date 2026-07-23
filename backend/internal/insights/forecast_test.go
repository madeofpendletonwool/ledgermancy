package insights

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/apex42group/ledgermancy/backend/internal/db"
	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
	"github.com/apex42group/ledgermancy/backend/internal/networth"
)

// TestForecastProducer confirms the producer raises exactly one forecast row and
// that its milestone figures equal networth.Project's output byte-for-byte — the
// producer reads the projection, it never recomputes it.
//
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' go test ./internal/insights/
func TestForecastProducer(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	q := dbgen.New(pool)

	householdID := uuid.New()
	userID := uuid.New()

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, sql)
		}
	}
	exec(`INSERT INTO households (id, name) VALUES ($1, 'Forecast Test')`, householdID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id = $1`, householdID)
	})
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
	      VALUES ($1, $2, $3, 'x', 'Tester')`, userID, householdID, userID.String()+"@example.test")
	// A manual asset gives Compute a non-zero starting position without needing
	// Plaid items; is_liability FALSE → an asset.
	exec(`INSERT INTO manual_assets (household_id, name, kind, value, is_liability)
	      VALUES ($1, 'Savings', 'cash', '10000.00', FALSE)`, householdID)

	now := time.Now()

	cands, err := forecastProducer{}.Detect(ctx, q, householdID, now)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want exactly 1 forecast", len(cands))
	}
	c := cands[0]
	if c.Kind != "forecast" {
		t.Errorf("kind = %q, want forecast", c.Kind)
	}
	if c.DedupeKey != "forecast:"+monthStart(now).Format("2006-01") {
		t.Errorf("dedupe_key = %q", c.DedupeKey)
	}

	// Recompute the projection independently and confirm the stored milestones
	// match exactly — no drift between the feed and the engine.
	current, err := networth.Compute(ctx, q, householdID)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	surplus, err := networth.DefaultMonthlySurplus(ctx, q, householdID, sharedUser, now)
	if err != nil {
		t.Fatalf("surplus: %v", err)
	}
	points := networth.Project(current, networth.Assumptions{
		MonthlySurplus:    surplus,
		AnnualReturnRate:  decimal.RequireFromString("0.06"),
		AnnualDebtPaydown: decimal.Zero,
		Months:            120,
	})

	milestones, ok := c.Data["milestones"].([]map[string]any)
	if !ok {
		t.Fatalf("milestones type = %T, want []map[string]any", c.Data["milestones"])
	}
	if len(milestones) != len(networth.ForecastMilestones) {
		t.Fatalf("got %d milestones, want %d", len(milestones), len(networth.ForecastMilestones))
	}
	for n, idx := range networth.ForecastMilestones {
		want := points[idx].NetWorth.StringFixed(2)
		if got := milestones[n]["net_worth"]; got != want {
			t.Errorf("milestone %d net_worth = %v, want %v (points[%d])", n, got, want, idx)
		}
		if got := milestones[n]["month"]; got != points[idx].Month {
			t.Errorf("milestone %d month = %v, want %v", n, got, points[idx].Month)
		}
	}
}
