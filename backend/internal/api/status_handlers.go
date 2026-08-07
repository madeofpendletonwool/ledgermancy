package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/continuity"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/jobs"
)

// The operational status surface: what this instance is actually doing.
//
// Owner-only for the same reason continuity is (see continuity_handlers.go):
// this describes the deployment rather than the household. It is grouped under
// auth.RequireOwner in server.go rather than checked per handler.
//
// Everything here is a count, a state word or a timestamp. No balances, no
// amounts, no tokens, no job arguments — a job's args routinely carry account
// and item identifiers, and there is no reason an operational panel needs them
// to answer "is the queue draining".

// systemStatusResponse is the whole panel in one payload, polled every few
// seconds by the browser. Three sections, in the order they fail: the queue
// stops first, the syncs go stale next, and the backup is the thing you find
// out about last and care about most.
type systemStatusResponse struct {
	Jobs   jobsPanel   `json:"jobs"`
	Sync   syncPanel   `json:"sync"`
	Backup backupPanel `json:"backup"`
}

// jobsPanel is the queue at a glance.
type jobsPanel struct {
	Health   string `json:"health"`
	Headline string `json:"headline"`

	// WorkerAlive is whether a worker process has a live claim on the leader
	// row. False is the single most consequential fact on this page: the API
	// keeps accepting work and the queue keeps growing, and nothing else about
	// the deployment looks wrong.
	WorkerAlive bool `json:"worker_alive"`

	// Counts is every River state with a non-zero count, plus the states the
	// panel always shows so a row does not vanish when it reaches zero.
	Counts map[string]int `json:"counts"`

	// WaitingSince is the oldest queued job's scheduled time — the honest
	// measure of whether the queue is draining. A big Available count with a
	// recent WaitingSince is a burst; a small one that keeps aging is a stall.
	WaitingSince *time.Time `json:"waiting_since,omitempty"`
	WaitingAge   string     `json:"waiting_age,omitempty"`

	Running  []runningJob   `json:"running"`
	Failures []failureGroup `json:"failures"`
}

// runningJob is one job being worked right now.
type runningJob struct {
	Kind        string     `json:"kind"`
	Attempt     int        `json:"attempt"`
	MaxAttempts int        `json:"max_attempts"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	Age         string     `json:"age,omitempty"`
}

// failureGroup is one kind of job that is failing, and why.
//
// LastError is the stringified error River recorded, not a stack trace, and it
// is truncated. It is included because a failure panel that will not say what
// failed sends the operator to the logs, which is the thing this page exists to
// avoid. The audience is the owner of a single-household instance who already
// has shell access to the host.
type failureGroup struct {
	Kind      string     `json:"kind"`
	State     string     `json:"state"`
	Count     int        `json:"count"`
	LastError string     `json:"last_error,omitempty"`
	LastAt    *time.Time `json:"last_at,omitempty"`
	Age       string     `json:"age,omitempty"`
}

// syncPanel is one line per linked institution.
type syncPanel struct {
	Health     string     `json:"health"`
	Headline   string     `json:"headline"`
	Configured bool       `json:"configured"`
	Items      []syncItem `json:"items"`
}

// syncItem is an institution's freshness, and nothing else about it.
type syncItem struct {
	ID           uuid.UUID  `json:"id"`
	Institution  string     `json:"institution"`
	Health       string     `json:"health"`
	Status       string     `json:"status"`
	ErrorCode    string     `json:"error_code,omitempty"`
	LastSyncedAt *time.Time `json:"last_synced_at,omitempty"`
	Age          string     `json:"age,omitempty"`
	// BackfillComplete is surfaced because a still-backfilling item legitimately
	// looks half-populated, and an operator who does not know that is being
	// invited to debug a working system.
	BackfillComplete bool `json:"backfill_complete"`
}

// backupPanel is a one-line summary that defers to the Continuity tab.
//
// Deliberately not a second copy of that panel: two pages rendering the same
// backup state drift, and the one with less room wins the operator's attention
// for no good reason. This says whether the last backup worked and points at
// the page that explains it.
type backupPanel struct {
	Health   string     `json:"health"`
	Headline string     `json:"headline"`
	At       *time.Time `json:"at,omitempty"`
	Age      string     `json:"age,omitempty"`
}

// queueStallAfter is how long the oldest queued job may wait before the panel
// calls the queue stalled.
//
// The worker polls on a seconds-long cycle and every periodic job in this app
// finishes in well under a minute, so a job that has been waiting a quarter of
// an hour is not queued behind work — something is wrong with the thing meant
// to pick it up.
const queueStallAfter = 15 * time.Minute

// jobErrorMaxLen truncates a recorded job error to something a table cell can
// hold. The full text is in the logs; this is a pointer to which log to read.
const jobErrorMaxLen = 300

// panelJobStates is the states the queue panel always renders, in lifecycle
// order, so a row reads as "0" rather than disappearing when it empties.
//
// `completed` and `cancelled` are omitted on purpose. River's cleaner reaps
// them on its own schedule, so their counts describe the cleaner's cadence
// rather than the app's health, and an operator reading a big `completed`
// number as significant is being misled.
var panelJobStates = []string{"available", "running", "retryable", "scheduled", "pending", "discarded"}

func (s *Server) handleSystemStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now()

	jobsSection, err := s.jobsPanel(ctx, now)
	if err != nil {
		s.internalError(w, "job queue status", err)
		return
	}
	syncSection, err := s.syncPanel(ctx, now)
	if err != nil {
		s.internalError(w, "sync status", err)
		return
	}
	backupSection, err := s.backupPanel(ctx, now)
	if err != nil {
		s.internalError(w, "backup status", err)
		return
	}

	writeJSON(w, http.StatusOK, systemStatusResponse{
		Jobs:   jobsSection,
		Sync:   syncSection,
		Backup: backupSection,
	})
}

// ---------------------------------------------------------------------------
// Jobs
// ---------------------------------------------------------------------------

// jobsPanel assembles the queue section.
//
// The four queries below are hand-written against s.Pool rather than generated,
// because river_job and river_leader are River's tables: it creates and
// migrates them at runtime (jobs.Migrate), so they are not in the schema
// directory sqlc reads and sqlc cannot type them. Reading them is a documented,
// stable contract — the column set has been unchanged across River's v0 line —
// but it is still someone else's schema, so nothing here writes to it.
func (s *Server) jobsPanel(ctx context.Context, now time.Time) (jobsPanel, error) {
	panel := jobsPanel{Counts: map[string]int{}}
	for _, state := range panelJobStates {
		panel.Counts[state] = 0
	}

	rows, err := s.Pool.Query(ctx, `
		SELECT state::text, count(*), min(scheduled_at)
		FROM river_job
		GROUP BY state`)
	if err != nil {
		return panel, fmt.Errorf("count jobs by state: %w", err)
	}
	for rows.Next() {
		var state string
		var count int
		var oldest *time.Time
		if err := rows.Scan(&state, &count, &oldest); err != nil {
			rows.Close()
			return panel, err
		}
		panel.Counts[state] = count
		if state == "available" {
			panel.WaitingSince = oldest
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return panel, err
	}
	if panel.WaitingSince != nil {
		panel.WaitingAge = continuity.Age(now.Sub(*panel.WaitingSince))
	}

	// A live claim on the leader row is the only evidence the API process has
	// that a worker exists at all. The API's own River client is insert-only
	// and never started (cmd/api/main.go), so it never elects — anything in
	// this row was put there by the worker, and the elector resigns the row on
	// a clean shutdown rather than leaving it to expire.
	var leaderExpires *time.Time
	if err := s.Pool.QueryRow(ctx,
		`SELECT max(expires_at) FROM river_leader`).Scan(&leaderExpires); err != nil {
		return panel, fmt.Errorf("read worker heartbeat: %w", err)
	}
	panel.WorkerAlive = leaderExpires != nil && leaderExpires.After(now)

	if panel.Running, err = s.runningJobs(ctx, now); err != nil {
		return panel, err
	}
	if panel.Failures, err = s.jobFailures(ctx, now); err != nil {
		return panel, err
	}

	panel.Health, panel.Headline = classifyQueue(panel, now)
	return panel, nil
}

func (s *Server) runningJobs(ctx context.Context, now time.Time) ([]runningJob, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT kind, attempt, max_attempts, attempted_at
		FROM river_job
		WHERE state = 'running'
		ORDER BY attempted_at
		LIMIT 20`)
	if err != nil {
		return nil, fmt.Errorf("list running jobs: %w", err)
	}
	defer rows.Close()

	running := []runningJob{}
	for rows.Next() {
		var j runningJob
		if err := rows.Scan(&j.Kind, &j.Attempt, &j.MaxAttempts, &j.StartedAt); err != nil {
			return nil, err
		}
		if j.StartedAt != nil {
			j.Age = continuity.Age(now.Sub(*j.StartedAt))
		}
		running = append(running, j)
	}
	return running, rows.Err()
}

// jobFailures groups everything currently failing by kind and state.
//
// Grouped rather than listed because 40 rows of the same broken job is not 40
// pieces of information. `retryable` and `discarded` are kept apart because
// they mean different things to the reader: one is the system still trying, the
// other is the system having given up and waiting on a human.
func (s *Server) jobFailures(ctx context.Context, now time.Time) ([]failureGroup, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT kind,
		       state::text,
		       count(*),
		       max(coalesce(finalized_at, attempted_at)) AS last_at,
		       (array_agg(errors[array_length(errors, 1)] ->> 'error'
		                  ORDER BY coalesce(finalized_at, attempted_at) DESC NULLS LAST))[1]
		FROM river_job
		WHERE state IN ('retryable', 'discarded')
		GROUP BY kind, state
		ORDER BY count(*) DESC, kind
		LIMIT 12`)
	if err != nil {
		return nil, fmt.Errorf("group job failures: %w", err)
	}
	defer rows.Close()

	failures := []failureGroup{}
	for rows.Next() {
		var f failureGroup
		var lastError *string
		if err := rows.Scan(&f.Kind, &f.State, &f.Count, &f.LastAt, &lastError); err != nil {
			return nil, err
		}
		if lastError != nil {
			f.LastError = truncate(*lastError, jobErrorMaxLen)
		}
		if f.LastAt != nil {
			f.Age = continuity.Age(now.Sub(*f.LastAt))
		}
		failures = append(failures, f)
	}
	return failures, rows.Err()
}

// classifyQueue turns the queue's numbers into a colour and a sentence.
//
// Ordered by consequence, not by severity of the word: a dead worker outranks
// discarded jobs because it explains them, and because every other number on
// the panel is meaningless while nothing is being worked.
func classifyQueue(p jobsPanel, now time.Time) (string, string) {
	queued := p.Counts["available"] + p.Counts["scheduled"] + p.Counts["retryable"]

	if !p.WorkerAlive {
		return string(continuity.HealthBad), fmt.Sprintf(
			"No worker is running. Nothing in the queue is being worked, and %d %s waiting. "+
				"Scheduled syncs, backups and alerts are all stopped until the worker container is back.",
			queued, pluralWord(queued, "job is", "jobs are"))
	}

	if discarded := p.Counts["discarded"]; discarded > 0 {
		return string(continuity.HealthBad), fmt.Sprintf(
			"%d %s given up after using every retry. They will not run again on their own — "+
				"read the failures below, fix the cause, and the next scheduled run will pick the work up.",
			discarded, pluralWord(discarded, "job has", "jobs have"))
	}

	if p.WaitingSince != nil && now.Sub(*p.WaitingSince) > queueStallAfter {
		return string(continuity.HealthWarn), fmt.Sprintf(
			"The oldest queued job has been waiting %s. The worker is alive but the queue is not "+
				"draining, so something is either stuck mid-job or arriving faster than it finishes.",
			continuity.Age(now.Sub(*p.WaitingSince)))
	}

	if retryable := p.Counts["retryable"]; retryable > 0 {
		return string(continuity.HealthWarn), fmt.Sprintf(
			"%d %s failed and will be retried automatically. Worth a look if the same kind keeps "+
				"appearing, but nothing is lost yet.",
			retryable, pluralWord(retryable, "job has", "jobs have"))
	}

	if running := p.Counts["running"]; running > 0 {
		return string(continuity.HealthGood), fmt.Sprintf(
			"Working %d %s now, nothing failing.", running, pluralWord(running, "job", "jobs"))
	}
	return string(continuity.HealthGood), "The queue is empty and the worker is waiting for something to do."
}

// ---------------------------------------------------------------------------
// Sync
// ---------------------------------------------------------------------------

// syncStaleAfter and syncBadAfter are multiples of the real sweep cadence
// rather than hand-picked hours, so changing jobs.SyncInterval moves these with
// it instead of leaving the panel quietly asserting a schedule the app no
// longer keeps.
//
// Missing a couple of cycles is unremarkable — Plaid rate-limits, institutions
// go down, laptops sleep. A whole day without a successful sync is not.
var (
	syncStaleAfter = 6 * jobs.SyncInterval
	syncBadAfter   = 24 * jobs.SyncInterval
)

// syncPanel reports each linked institution's freshness.
//
// Scoped through ListVisiblePlaidItems with the caller's own identity, exactly
// like every other read of items, so the owner sees their own items and the
// household's shared ones and no more. The operator role does NOT widen this:
// sharing is a privacy boundary between two adults in a household, and "I run
// the server" is not consent to cross it. An unshared item that is failing to
// sync is its owner's to see, on their own accounts page.
func (s *Server) syncPanel(ctx context.Context, now time.Time) (syncPanel, error) {
	identity := auth.MustFromContext(ctx)

	panel := syncPanel{
		Configured: s.Plaid != nil,
		Items:      []syncItem{},
	}

	items, err := s.Queries.ListVisiblePlaidItems(ctx, dbgen.ListVisiblePlaidItemsParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
	})
	if err != nil {
		return panel, fmt.Errorf("list items: %w", err)
	}

	worst := continuity.HealthGood
	stale, broken := 0, 0
	for _, item := range items {
		line := syncItem{
			ID:               item.ID,
			Institution:      "Unnamed institution",
			Status:           item.Status,
			LastSyncedAt:     item.LastSyncedAt,
			BackfillComplete: item.BackfillComplete,
		}
		if item.InstitutionName != nil && *item.InstitutionName != "" {
			line.Institution = *item.InstitutionName
		}
		if item.ErrorCode != nil {
			line.ErrorCode = *item.ErrorCode
		}

		// Phrased before the health switch, not inside it. A broken item that
		// last synced three days ago still has a last-sync time worth showing,
		// and an earlier version that only set this on the healthy path made the
		// panel report "never synced" for an item that had synced for months —
		// the one reading a reader would act on most decisively, and wrong.
		if item.LastSyncedAt != nil {
			line.Age = continuity.Age(now.Sub(*item.LastSyncedAt))
		}

		switch {
		case item.Status != "active":
			// A revoked or login_required item is broken in a way only the user
			// can fix, so it is red regardless of when it last synced.
			line.Health = string(continuity.HealthBad)
			broken++
		case item.LastSyncedAt == nil:
			line.Health = string(continuity.HealthNeverRun)
			broken++
		default:
			age := now.Sub(*item.LastSyncedAt)
			switch {
			case age > syncBadAfter:
				line.Health = string(continuity.HealthBad)
				broken++
			case age > syncStaleAfter:
				line.Health = string(continuity.HealthWarn)
				stale++
			default:
				line.Health = string(continuity.HealthGood)
			}
		}
		worst = worseHealth(worst, continuity.Health(line.Health))
		panel.Items = append(panel.Items, line)
	}

	panel.Health = string(worst)
	switch {
	case !panel.Configured:
		panel.Health = string(continuity.HealthOff)
		panel.Headline = "Plaid is not configured, so nothing is being synced automatically. " +
			"Manually entered accounts are unaffected."
	case len(panel.Items) == 0:
		panel.Health = string(continuity.HealthOff)
		panel.Headline = "No institutions are linked yet."
	case broken > 0:
		panel.Headline = fmt.Sprintf(
			"%d of %d %s not syncing. Anything spent through %s is missing from your ledger "+
				"until it reconnects.",
			broken, len(panel.Items), pluralWord(broken, "connection is", "connections are"),
			pluralWord(broken, "it", "them"))
	case stale > 0:
		panel.Headline = fmt.Sprintf(
			"%d %s not updated for a while. Usually the institution being slow rather than a fault.",
			stale, pluralWord(stale, "connection has", "connections have"))
	default:
		panel.Headline = fmt.Sprintf("All %d %s up to date.",
			len(panel.Items), pluralWord(len(panel.Items), "connection is", "connections are"))
	}
	return panel, nil
}

// ---------------------------------------------------------------------------
// Backup
// ---------------------------------------------------------------------------

// backupPanel is the database dump's health only, classified by the same
// function the Continuity tab uses so the two pages can never disagree.
func (s *Server) backupPanel(ctx context.Context, now time.Time) (backupPanel, error) {
	panel := backupPanel{}

	if !s.Config.Backup.Enabled {
		panel.Health = string(continuity.HealthOff)
		panel.Headline = "Backups are switched off. A disk failure would be final."
		return panel, nil
	}

	latest, err := s.Queries.LatestBackupRunPerKind(ctx)
	if err != nil {
		return panel, fmt.Errorf("latest backup runs: %w", err)
	}
	for _, run := range latest {
		if run.Kind != continuity.KindDBDump {
			continue
		}
		at := run.StartedAt
		panel.At = &at
		panel.Age = continuity.Age(now.Sub(at))
		panel.Health = string(continuity.ClassifyRun(run.Kind, run.Status, at, now))
		panel.Headline = panelHeadline(continuity.KindDBDump, continuity.Health(panel.Health), &at, now)
		return panel, nil
	}

	panel.Health = string(continuity.HealthNeverRun)
	panel.Headline = panelHeadline(continuity.KindDBDump, continuity.HealthNeverRun, nil, now)
	return panel, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// healthRank orders the five health words by how much they should worry the
// reader, so a section can take the worst of its rows. `off` sorts below `good`
// because a thing switched off deliberately is a decision, not a fault.
var healthRank = map[continuity.Health]int{
	continuity.HealthOff:      0,
	continuity.HealthGood:     1,
	continuity.HealthWarn:     2,
	continuity.HealthNeverRun: 3,
	continuity.HealthBad:      4,
}

func worseHealth(a, b continuity.Health) continuity.Health {
	if healthRank[b] > healthRank[a] {
		return b
	}
	return a
}

func pluralWord(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
