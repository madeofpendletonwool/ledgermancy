package insights

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/apex42group/ledgermancy/backend/internal/ai"
	"github.com/apex42group/ledgermancy/backend/internal/config"
	"github.com/apex42group/ledgermancy/backend/internal/db"
	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
)

// TestGoalProducerCoachesBehind proves the coaching producer raises a kind=goal
// insight for a household goal that is behind schedule, with the shortfall in
// its data and template (AI-off) text — and that only household-scoped goals are
// coached, never a personal one.
//
//	TEST_DATABASE_URL='postgres://…' go test ./internal/insights/ -run TestGoalProducer
func TestGoalProducerCoachesBehind(t *testing.T) {
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

	if _, err := pool.Exec(ctx, `INSERT INTO households (id, name) VALUES ($1, 'Goal Test')`, householdID); err != nil {
		t.Fatalf("seed household: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id = $1`, householdID)
	})
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, household_id, email, password_hash, display_name)
		VALUES ($1, $2, $3, 'x', 'Tester')`, userID, householdID, userID.String()+"@example.test"); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	now := time.Now().UTC()
	target := now.AddDate(1, 0, 0) // a year out

	// A household goal with no linked account and no transactions → zero surplus,
	// zero progress, positive required → behind.
	shared, err := q.CreateGoal(ctx, dbgen.CreateGoalParams{
		HouseholdID:  householdID,
		Scope:        "household",
		Kind:         "savings",
		Name:         "New roof",
		TargetAmount: decimal.RequireFromString("12000"),
		TargetDate:   &target,
	})
	if err != nil {
		t.Fatalf("create household goal: %v", err)
	}

	// A personal goal that is also behind — it must NOT surface in the shared feed.
	if _, err := q.CreateGoal(ctx, dbgen.CreateGoalParams{
		HouseholdID:  householdID,
		Scope:        "user",
		UserID:       &userID,
		Kind:         "savings",
		Name:         "Secret splurge",
		TargetAmount: decimal.RequireFromString("5000"),
		TargetDate:   &target,
	}); err != nil {
		t.Fatalf("create personal goal: %v", err)
	}

	aiOff := ai.New(config.AIConfig{})
	if _, err := Generate(ctx, q, aiOff, householdID, now); err != nil {
		t.Fatalf("generate: %v", err)
	}

	rows, err := q.ListInsights(ctx, dbgen.ListInsightsParams{HouseholdID: householdID, IncludeDismissed: true})
	if err != nil {
		t.Fatalf("list insights: %v", err)
	}

	var goalRows []dbgen.Insight
	for _, r := range rows {
		if r.Kind == "goal" {
			goalRows = append(goalRows, r)
		}
	}
	if len(goalRows) != 1 {
		t.Fatalf("expected exactly 1 goal insight (household only), got %d", len(goalRows))
	}

	got := goalRows[0]
	var data map[string]any
	if err := json.Unmarshal(got.Data, &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if data["goal_id"] != shared.ID.String() {
		t.Errorf("goal insight is for %v, want the household goal %v", data["goal_id"], shared.ID)
	}
	if data["status"] != "behind" {
		t.Errorf("status = %v, want behind", data["status"])
	}
	// required = 12000 / 12 = 1000.00; surplus 0 → shortfall 1000.00.
	if data["shortfall"] != "1000.00" {
		t.Errorf("shortfall = %v, want 1000.00", data["shortfall"])
	}
	if got.Title == "" || got.Body == "" {
		t.Errorf("expected template title/body with AI off, got title=%q body=%q", got.Title, got.Body)
	}

	// The behind goal is not yet achieved, so achieved_at must remain unset.
	fresh, err := q.GetGoal(ctx, dbgen.GetGoalParams{ID: shared.ID, HouseholdID: householdID, UserID: &userID})
	if err != nil {
		t.Fatalf("get goal: %v", err)
	}
	if fresh.AchievedAt != nil {
		t.Errorf("achieved_at should be nil for a behind goal, got %v", fresh.AchievedAt)
	}
}
