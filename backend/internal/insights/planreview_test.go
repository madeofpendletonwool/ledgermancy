package insights

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/testdb"
)

// TestPlanStaleProducer pins the plan review nudge (MAD-258): quiet for a
// household with no plan or a fresh one, raised once the newest involvement is
// six months old, and retracted the moment the review stamp lands.
//
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' \
//	    go test ./internal/insights/ -run TestPlanStaleProducer
func TestPlanStaleProducer(t *testing.T) {
	url := testdb.URL(t)

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
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, sql)
		}
	}
	exec(`INSERT INTO households (id, name) VALUES ($1, 'Plan Stale Test')`, householdID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id = $1`, householdID)
	})

	p := planStaleProducer{}
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)

	// No plan at all: quiet. has_content is false, and an empty household must
	// not be nagged about a plan it never wrote.
	if c, err := p.Detect(ctx, q, householdID, now); err != nil || len(c) != 0 {
		t.Fatalf("no-plan household: candidates=%v err=%v", c, err)
	}

	// A section written NOW: fresh involvement, still quiet.
	sealed := []byte("body") // the producer never opens bodies; bytes are opaque here
	section, err := q.UpsertPlanSection(ctx, dbgen.UpsertPlanSectionParams{
		HouseholdID: householdID, Kind: "strategy", Body: sealed,
	})
	if err != nil {
		t.Fatalf("seed section: %v", err)
	}
	if c, err := p.Detect(ctx, q, householdID, now); err != nil || len(c) != 0 {
		t.Fatalf("fresh plan: candidates=%v err=%v", c, err)
	}

	// Same section, seven months later: raised, exactly once.
	later := now.AddDate(0, 7, 0)
	c, err := p.Detect(ctx, q, householdID, later)
	if err != nil {
		t.Fatalf("detect stale: %v", err)
	}
	if len(c) != 1 || c[0].DedupeKey != "plan_stale" {
		t.Fatalf("expected one plan_stale candidate, got %+v", c)
	}

	// LiveKeys agrees with Detect while stale…
	keys, err := p.LiveKeys(ctx, q, householdID, later)
	if err != nil || len(keys) != 1 || keys[0] != "plan_stale" {
		t.Fatalf("stale live keys: %v %v", keys, err)
	}

	// …and retracts once the household stamps a review. The stamp is written
	// with the database clock, which does not know this test's synthetic
	// `later` — so the retraction assertion sets plan_reviewed_at directly at
	// the synthetic time, exactly what StampPlanReview's now() would write in
	// production. (That the endpoint lands a real stamp is asserted in the API
	// tests, TestPlanReviewStampAndBriefingDigest.)
	stamped := later.AddDate(0, 0, 1)
	exec(`UPDATE households SET plan_reviewed_at = $2 WHERE id = $1`, householdID, stamped)
	keys, err = p.LiveKeys(ctx, q, householdID, stamped)
	if err != nil || len(keys) != 0 {
		t.Fatalf("post-stamp live keys should be empty: %v %v", keys, err)
	}
	if c, err := p.Detect(ctx, q, householdID, stamped); err != nil || len(c) != 0 {
		t.Fatalf("post-stamp detect should be quiet: %v %v", c, err)
	}

	_ = section
}
