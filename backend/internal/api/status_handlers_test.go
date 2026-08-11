package api

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/continuity"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/jobs"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/testdb"
)

// The status panel's judgement, tested without a database.
//
// The SQL that feeds these functions is exercised by the role-enforcement test
// reaching the route and by running the app; what is worth pinning down here is
// the ordering of the verdicts, because it is the part a later change is most
// likely to get subtly wrong. A queue panel that reports "3 jobs are retrying"
// while no worker exists to retry them is not a cosmetic bug — it is the page
// actively misleading the person trying to fix the outage.

func TestClassifyQueueOrdersVerdictsByConsequence(t *testing.T) {
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	longAgo := now.Add(-2 * time.Hour)
	recent := now.Add(-time.Minute)

	tests := []struct {
		name       string
		panel      jobsPanel
		wantHealth continuity.Health
		wantSaid   string
	}{
		{
			// The whole point of the ordering. Every other number on the panel
			// is describing a queue nothing is reading.
			name: "a dead worker outranks everything else",
			panel: jobsPanel{
				WorkerAlive:  false,
				Counts:       map[string]int{"available": 4, "discarded": 9, "retryable": 2},
				WaitingSince: &longAgo,
			},
			wantHealth: continuity.HealthBad,
			wantSaid:   "No worker is running",
		},
		{
			name: "discarded jobs outrank a stalled queue",
			panel: jobsPanel{
				WorkerAlive:  true,
				Counts:       map[string]int{"discarded": 3, "retryable": 1},
				WaitingSince: &longAgo,
			},
			wantHealth: continuity.HealthBad,
			wantSaid:   "given up",
		},
		{
			name: "a queue that is not draining is a warning",
			panel: jobsPanel{
				WorkerAlive:  true,
				Counts:       map[string]int{"available": 2},
				WaitingSince: &longAgo,
			},
			wantHealth: continuity.HealthWarn,
			wantSaid:   "not draining",
		},
		{
			// A burst is not a stall: same queue depth, recent arrival.
			name: "a recent backlog is not a stall",
			panel: jobsPanel{
				WorkerAlive:  true,
				Counts:       map[string]int{"available": 2, "running": 1},
				WaitingSince: &recent,
			},
			wantHealth: continuity.HealthGood,
			wantSaid:   "Working",
		},
		{
			name: "retrying jobs warn without alarming",
			panel: jobsPanel{
				WorkerAlive: true,
				Counts:      map[string]int{"retryable": 2},
			},
			wantHealth: continuity.HealthWarn,
			wantSaid:   "retried automatically",
		},
		{
			name: "an idle queue with a live worker is healthy",
			panel: jobsPanel{
				WorkerAlive: true,
				Counts:      map[string]int{},
			},
			wantHealth: continuity.HealthGood,
			wantSaid:   "waiting for something to do",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			health, headline := classifyQueue(tt.panel, now)

			if health != string(tt.wantHealth) {
				t.Errorf("health = %q, want %q", health, tt.wantHealth)
			}
			if !strings.Contains(headline, tt.wantSaid) {
				t.Errorf("headline %q does not mention %q", headline, tt.wantSaid)
			}
		})
	}
}

// The stall threshold is the one number on the panel a reader cannot see, so it
// is worth asserting rather than trusting the constant to stay put.
func TestClassifyQueueStallThreshold(t *testing.T) {
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

	justInside := now.Add(-queueStallAfter + time.Minute)
	justOutside := now.Add(-queueStallAfter - time.Minute)

	if health, _ := classifyQueue(jobsPanel{
		WorkerAlive:  true,
		Counts:       map[string]int{"available": 1},
		WaitingSince: &justInside,
	}, now); health != string(continuity.HealthGood) {
		t.Errorf("a job waiting under the threshold got %q, want good", health)
	}

	if health, _ := classifyQueue(jobsPanel{
		WorkerAlive:  true,
		Counts:       map[string]int{"available": 1},
		WaitingSince: &justOutside,
	}, now); health != string(continuity.HealthWarn) {
		t.Errorf("a job waiting past the threshold got %q, want warn", health)
	}
}

// worseHealth decides a section's colour from its rows, so getting `off` wrong
// would paint every deliberately-disabled deployment red.
func TestWorseHealthRanksOffBelowGood(t *testing.T) {
	tests := []struct {
		a, b, want continuity.Health
	}{
		{continuity.HealthGood, continuity.HealthOff, continuity.HealthGood},
		{continuity.HealthOff, continuity.HealthGood, continuity.HealthGood},
		{continuity.HealthGood, continuity.HealthWarn, continuity.HealthWarn},
		{continuity.HealthWarn, continuity.HealthBad, continuity.HealthBad},
		{continuity.HealthNeverRun, continuity.HealthWarn, continuity.HealthNeverRun},
		{continuity.HealthBad, continuity.HealthNeverRun, continuity.HealthBad},
	}

	for _, tt := range tests {
		if got := worseHealth(tt.a, tt.b); got != tt.want {
			t.Errorf("worseHealth(%q, %q) = %q, want %q", tt.a, tt.b, got, tt.want)
		}
	}
}

// A job error is attacker-influenced in the sense that it can be arbitrarily
// long, and it renders in a table cell.
func TestTruncateCapsJobErrors(t *testing.T) {
	if got := truncate("short", jobErrorMaxLen); got != "short" {
		t.Errorf("a short error was altered: %q", got)
	}

	long := strings.Repeat("x", jobErrorMaxLen+50)
	got := truncate(long, jobErrorMaxLen)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("a truncated error should say so, got %q", got[len(got)-10:])
	}
	if len([]rune(got)) != jobErrorMaxLen+1 {
		t.Errorf("truncated to %d runes, want %d", len([]rune(got)), jobErrorMaxLen+1)
	}
}

// The queue panel against a real River schema.
//
// The pure tests above cover the verdict ordering; this one covers the part
// they cannot — that the SQL actually runs against the tables River creates,
// and that the failure grouping picks the LATEST error from the LATEST failing
// job rather than an arbitrary one. That expression (a subscript into a jsonb[]
// inside an ordered aggregate) is the single most fragile line in the file.
//
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' go test ./internal/api/
func TestJobsPanelReadsRiverSchema(t *testing.T) {
	url := testdb.URL(t)

	ctx := context.Background()
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := jobs.Migrate(ctx, pool); err != nil {
		t.Fatalf("river migrate: %v", err)
	}

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, sql)
		}
	}

	// No other test in this package reads or writes River's tables, so the
	// panel can be tested against a known-empty queue.
	reset := func() {
		exec(`DELETE FROM river_job`)
		exec(`DELETE FROM river_leader`)
	}
	reset()
	// A defer rather than t.Cleanup: cleanups run after the deferred
	// pool.Close() above, which would leave this reset talking to a shut pool.
	defer reset()

	exec(`INSERT INTO river_job
	        (state, attempt, max_attempts, kind, queue, args, scheduled_at, attempted_at, finalized_at, errors)
	      VALUES
	        ('available', 0, 3, 'notify', 'default', '{}', now() - interval '40 minutes', NULL, NULL, NULL),
	        ('running',   1, 3, 'digest', 'default', '{}', now() - interval '1 minute',
	                                                      now() - interval '30 seconds', NULL, NULL),
	        ('retryable', 1, 3, 'llm_categorise', 'default', '{}', now() + interval '5 minutes',
	                                                      now() - interval '10 minutes', NULL,
	          ARRAY['{"at":"2026-01-01T00:00:00Z","attempt":1,"error":"overloaded","trace":""}'::jsonb]),
	        -- Two discards of the same kind. The older one is inserted last so a
	        -- query that happened to pick "whichever row came back first" would
	        -- pass by luck; only ordering by time gets the newer error.
	        ('discarded', 3, 3, 'plaid_sync', 'default', '{}', now() - interval '2 hours',
	                                                      now() - interval '90 minutes',
	                                                      now() - interval '80 minutes',
	          ARRAY['{"at":"2026-01-01T00:00:00Z","attempt":1,"error":"first attempt failed","trace":""}'::jsonb,
	                '{"at":"2026-01-01T00:05:00Z","attempt":3,"error":"NEWEST: reconnect needed","trace":""}'::jsonb]),
	        ('discarded', 3, 3, 'plaid_sync', 'default', '{}', now() - interval '3 hours',
	                                                      now() - interval '3 hours',
	                                                      now() - interval '170 minutes',
	          ARRAY['{"at":"2026-01-01T00:00:00Z","attempt":3,"error":"older discard","trace":""}'::jsonb])`)

	srv := &Server{Pool: pool, Queries: dbgen.New(pool)}
	now := time.Now()

	panel, err := srv.jobsPanel(ctx, now)
	if err != nil {
		t.Fatalf("jobsPanel: %v", err)
	}

	for state, want := range map[string]int{
		"available": 1, "running": 1, "retryable": 1, "discarded": 2, "scheduled": 0,
	} {
		if got := panel.Counts[state]; got != want {
			t.Errorf("count[%s] = %d, want %d", state, got, want)
		}
	}

	// The oldest AVAILABLE job, not the oldest job of any state — a scheduled
	// job due next week is not evidence the queue is stuck.
	if panel.WaitingSince == nil {
		t.Fatal("WaitingSince is nil, want the queued job's scheduled_at")
	}
	if waited := now.Sub(*panel.WaitingSince); waited < 35*time.Minute || waited > 45*time.Minute {
		t.Errorf("oldest queued job waited %s, want ~40 minutes", waited)
	}

	if len(panel.Running) != 1 || panel.Running[0].Kind != "digest" {
		t.Errorf("Running = %+v, want one digest job", panel.Running)
	}

	// No leader row was seeded, so nothing is working the queue.
	if panel.WorkerAlive {
		t.Error("WorkerAlive is true with no leader row")
	}

	byKind := map[string]failureGroup{}
	for _, f := range panel.Failures {
		byKind[f.Kind+"/"+f.State] = f
	}
	discarded, ok := byKind["plaid_sync/discarded"]
	if !ok {
		t.Fatalf("no discarded plaid_sync group in %+v", panel.Failures)
	}
	if discarded.Count != 2 {
		t.Errorf("discarded count = %d, want 2", discarded.Count)
	}
	if !strings.Contains(discarded.LastError, "NEWEST") {
		t.Errorf("LastError = %q, want the newest failing job's newest error", discarded.LastError)
	}
	if _, ok := byKind["llm_categorise/retryable"]; !ok {
		t.Errorf("retryable group missing from %+v", panel.Failures)
	}

	// A live leader claim is the only thing that flips the worker verdict.
	exec(`INSERT INTO river_leader (name, leader_id, elected_at, expires_at)
	      VALUES ('default', 'test-worker', now(), now() + interval '30 seconds')`)
	if panel, err = srv.jobsPanel(ctx, time.Now()); err != nil {
		t.Fatalf("jobsPanel after election: %v", err)
	}
	if !panel.WorkerAlive {
		t.Error("WorkerAlive is false with an unexpired leader row")
	}

	// An expired claim is a dead worker, not a live one.
	exec(`UPDATE river_leader SET expires_at = now() - interval '1 minute'`)
	if panel, err = srv.jobsPanel(ctx, time.Now()); err != nil {
		t.Fatalf("jobsPanel after expiry: %v", err)
	}
	if panel.WorkerAlive {
		t.Error("WorkerAlive is true with an expired leader row")
	}
}

// A broken connection still reports when it last worked.
//
// Regression: the age was originally phrased only on the healthy branch of the
// health switch, so an item that had synced for months and then hit
// ITEM_LOGIN_REQUIRED came back with no age at all — and the panel renders a
// missing age as "never synced". That is the most misleading thing this page
// could say about a connection: it sends the reader to re-link an account whose
// history is fine, instead of to the reconnect prompt.
func TestSyncPanelPhrasesAgeForBrokenConnections(t *testing.T) {
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

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, sql)
		}
	}

	household, user := uuid.New(), uuid.New()
	exec(`INSERT INTO households (id, name) VALUES ($1, 'Sync Panel')`, household)
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
	      VALUES ($1, $2, $3, 'x', 'Owner')`, user, household, user.String()+"@example.test")
	defer func() {
		exec(`DELETE FROM plaid_items WHERE user_id = $1`, user)
		exec(`DELETE FROM users WHERE id = $1`, user)
		exec(`DELETE FROM households WHERE id = $1`, household)
	}()

	broken, never := uuid.New(), uuid.New()
	exec(`INSERT INTO plaid_items
	        (id, user_id, plaid_item_id, access_token_encrypted, institution_name,
	         status, error_code, last_synced_at, backfill_complete)
	      VALUES ($1, $2, $3, '\x00', 'Chase', 'login_required', 'ITEM_LOGIN_REQUIRED',
	              now() - interval '3 days', true)`, broken, user, broken.String())
	exec(`INSERT INTO plaid_items
	        (id, user_id, plaid_item_id, access_token_encrypted, institution_name,
	         status, last_synced_at, backfill_complete)
	      VALUES ($1, $2, $3, '\x00', 'Never Synced', 'active', NULL, false)`,
		never, user, never.String())

	srv := &Server{Pool: pool, Queries: dbgen.New(pool)}
	ctx = auth.ContextWithIdentity(ctx, auth.Identity{
		UserID: user, HouseholdID: household, Role: auth.RoleOwner,
	})

	panel, err := srv.syncPanel(ctx, time.Now())
	if err != nil {
		t.Fatalf("syncPanel: %v", err)
	}

	byName := map[string]syncItem{}
	for _, item := range panel.Items {
		byName[item.Institution] = item
	}

	chase, ok := byName["Chase"]
	if !ok {
		t.Fatalf("Chase missing from %+v", panel.Items)
	}
	if chase.Health != string(continuity.HealthBad) {
		t.Errorf("a login_required item is %q, want bad", chase.Health)
	}
	if chase.Age == "" {
		t.Error("a broken item that has synced before reports no age, so the panel will call it never synced")
	}
	if chase.ErrorCode != "ITEM_LOGIN_REQUIRED" {
		t.Errorf("ErrorCode = %q, want the code the operator will search for", chase.ErrorCode)
	}

	// The genuinely-never-synced item is the one that may report no age.
	fresh, ok := byName["Never Synced"]
	if !ok {
		t.Fatalf("Never Synced missing from %+v", panel.Items)
	}
	if fresh.Age != "" {
		t.Errorf("an item that has never synced reports age %q", fresh.Age)
	}
	if fresh.Health != string(continuity.HealthNeverRun) {
		t.Errorf("a never-synced item is %q, want never", fresh.Health)
	}

	// The access token must not survive the mapping into the response type.
	for _, item := range panel.Items {
		if strings.Contains(fmt.Sprintf("%+v", item), "\x00") {
			t.Errorf("item %+v carries raw token bytes", item)
		}
	}
}
