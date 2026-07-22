// Package jobs defines Ledgermancy's background work and the River queue that
// runs it. Job arguments live here so the api (which enqueues) and the worker
// (which executes) share one definition.
package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/riverqueue/river/rivertype"

	"github.com/apex42group/ledgermancy/backend/internal/auth"
	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
	"github.com/apex42group/ledgermancy/backend/internal/networth"
	"github.com/apex42group/ledgermancy/backend/internal/plaid"
)

// QueueDefault is the single queue everything runs on. Volume for a household
// ledger is low enough that separate queues would only add moving parts.
const QueueDefault = "default"

// SyncItemArgs requests a Plaid sync for one item. On an item that has never
// synced this performs the full historical backfill.
type SyncItemArgs struct {
	ItemID uuid.UUID `json:"item_id"`
}

func (SyncItemArgs) Kind() string { return "plaid_sync" }

// InsertOpts makes the job idempotent per item: if a sync for this item is
// already queued or running, a duplicate request is dropped rather than
// running two overlapping syncs against the same cursor.
//
// ByState must include River's four required states (available, pending,
// running, scheduled) or every insert is rejected outright — so this starts
// from the library's default set and adds `retryable`, which keeps a failing
// item from stacking up duplicate syncs while it backs off.
func (a SyncItemArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: QueueDefault,
		UniqueOpts: river.UniqueOpts{
			ByArgs:   true,
			ByState:  append(rivertype.UniqueOptsByStateDefault(), rivertype.JobStateRetryable),
			ByPeriod: time.Minute,
		},
	}
}

// SyncItemWorker executes a Plaid sync.
type SyncItemWorker struct {
	river.WorkerDefaults[SyncItemArgs]
	Syncer *plaid.Syncer
}

func (w *SyncItemWorker) Work(ctx context.Context, job *river.Job[SyncItemArgs]) error {
	start := time.Now()

	result, err := w.Syncer.SyncItem(ctx, job.Args.ItemID)
	if err != nil {
		// Returning the error lets River retry with exponential backoff. An
		// item needing re-authentication has already been marked
		// login_required by the syncer, so retries stop being useful — but
		// they are cheap and self-correct once the user reconnects.
		return fmt.Errorf("sync item %s: %w", job.Args.ItemID, err)
	}

	slog.Info("plaid sync complete",
		"item_id", result.ItemID,
		"pages", result.Pages,
		"added", result.Added,
		"modified", result.Modified,
		"removed", result.Removed,
		"accounts", result.AccountsUpserted,
		"duration_ms", time.Since(start).Milliseconds(),
	)
	return nil
}

// Timeout allows a long initial backfill: two years of history across several
// accounts can take well over the default.
func (w *SyncItemWorker) Timeout(*river.Job[SyncItemArgs]) time.Duration {
	return 10 * time.Minute
}

// SyncAllArgs sweeps every active item that is due for a refresh.
type SyncAllArgs struct{}

func (SyncAllArgs) Kind() string { return "plaid_sync_all" }

// SyncAllWorker enqueues a per-item sync for everything due.
type SyncAllWorker struct {
	river.WorkerDefaults[SyncAllArgs]
	Pool   *pgxpool.Pool
	Client *river.Client[pgx.Tx]
	// StaleAfter is how old an item's last sync must be before it is refreshed.
	StaleAfter time.Duration
}

func (w *SyncAllWorker) Work(ctx context.Context, job *river.Job[SyncAllArgs]) error {
	cutoff := time.Now().Add(-w.StaleAfter)

	rows, err := w.Pool.Query(ctx, `
		SELECT id FROM plaid_items
		WHERE status = 'active'
		  AND (last_synced_at IS NULL OR last_synced_at < $1)`, cutoff)
	if err != nil {
		return fmt.Errorf("list due items: %w", err)
	}
	defer rows.Close()

	var itemIDs []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("scan item id: %w", err)
		}
		itemIDs = append(itemIDs, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, id := range itemIDs {
		if _, err := w.Client.Insert(ctx, SyncItemArgs{ItemID: id}, nil); err != nil {
			slog.Error("enqueue sync", "error", err, "item_id", id)
		}
	}

	slog.Info("scheduled syncs", "items", len(itemIDs))
	return nil
}

// Migrate applies River's own schema. It is separate from the application
// migrations because River owns and versions those tables itself.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return fmt.Errorf("create river migrator: %w", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return fmt.Errorf("run river migrations: %w", err)
	}
	return nil
}

// SnapshotNetWorthArgs records every household's net worth for today.
type SnapshotNetWorthArgs struct{}

func (SnapshotNetWorthArgs) Kind() string { return "net_worth_snapshot" }

// SnapshotNetWorthWorker writes the daily net-worth row.
//
// This runs on a schedule as well as after each sync, because a household
// whose institutions are all quiet still needs a point on the trend line —
// otherwise the chart would show gaps wherever nothing happened to sync.
type SnapshotNetWorthWorker struct {
	river.WorkerDefaults[SnapshotNetWorthArgs]
	Queries *dbgen.Queries
}

func (w *SnapshotNetWorthWorker) Work(ctx context.Context, job *river.Job[SnapshotNetWorthArgs]) error {
	n, err := networth.SnapshotAll(ctx, w.Queries)
	if err != nil {
		return fmt.Errorf("snapshot net worth: %w", err)
	}
	slog.Info("net worth snapshots written", "households", n)
	return nil
}

// --------------------------------------------------------------------------
// Security housekeeping
// --------------------------------------------------------------------------

// SecuritySweepArgs prunes expired auth state.
type SecuritySweepArgs struct{}

func (SecuritySweepArgs) Kind() string { return "security_sweep" }

// SecuritySweepWorker collects rows that are dead but still on disk.
//
// None of this is load-bearing for correctness — every query that reads these
// tables already filters on expiry, so a stale row is never honoured. It is
// housekeeping: without it, sessions, abandoned MFA challenges and audit
// events accumulate forever in a database nobody is watching.
type SecuritySweepWorker struct {
	river.WorkerDefaults[SecuritySweepArgs]
	Queries *dbgen.Queries
	// IdleTTL must match auth.SessionIdleTTL, so the sweep collects exactly
	// the sessions the middleware has already stopped honouring.
	IdleTTL time.Duration
	// AuthEventTTL is how long the audit log is kept. Long enough to
	// investigate something noticed late, short enough that the table stays
	// small on a household-sized deployment.
	AuthEventTTL time.Duration
}

func (w *SecuritySweepWorker) Work(ctx context.Context, job *river.Job[SecuritySweepArgs]) error {
	sessions, err := w.Queries.DeleteExpiredSessions(ctx, auth.Interval(w.IdleTTL))
	if err != nil {
		return fmt.Errorf("delete expired sessions: %w", err)
	}

	challenges, err := w.Queries.DeleteExpiredMFAChallenges(ctx)
	if err != nil {
		return fmt.Errorf("delete expired mfa challenges: %w", err)
	}

	events, err := w.Queries.DeleteOldAuthEvents(ctx, auth.Interval(w.AuthEventTTL))
	if err != nil {
		return fmt.Errorf("delete old auth events: %w", err)
	}

	slog.Info("security sweep",
		"sessions", sessions, "mfa_challenges", challenges, "auth_events", events)
	return nil
}
