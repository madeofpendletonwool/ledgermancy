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

// SyncAllWorker asks Plaid to refresh what is due, then enqueues a per-item
// sync for everything due.
type SyncAllWorker struct {
	river.WorkerDefaults[SyncAllArgs]
	Pool   *pgxpool.Pool
	Client *river.Client[pgx.Tx]
	// Syncer performs the refresh. Nil when Plaid is not configured, in which
	// case there is nothing to sweep at all.
	Syncer *plaid.Syncer
	// StaleAfter is how old an item's last sync must be before it is refreshed.
	StaleAfter time.Duration
	// RefreshAfter is how long to wait between asking Plaid to pull fresh data
	// for the same item. Much longer than StaleAfter: reading Plaid's cache is
	// cheap, but making Plaid go to the bank is rate limited per item.
	RefreshAfter time.Duration
}

// dueItemsQuery selects the items a sweep should act on, and reports which of
// them additionally need a bank pull.
//
// $1 is the sync cutoff, $2 the refresh cutoff. The two schedules are
// independent, so an item qualifies on *either*. Gating the refresh behind sync
// staleness instead would starve it: a webhook-driven sync bumps
// last_synced_at, so a busy item can stay perpetually sync-fresh and never
// become eligible for the bank pull it is long overdue for — which is precisely
// the stall this job exists to break.
//
// Every row returned is synced, so only the refresh needs its own flag.
// TestDueItemsQuery pins both halves.
const dueItemsQuery = `
	SELECT id, (last_refresh_at IS NULL OR last_refresh_at < $2) AS refresh_due
	FROM plaid_items
	WHERE status = 'active'
	  AND ((last_synced_at  IS NULL OR last_synced_at  < $1)
	    OR (last_refresh_at IS NULL OR last_refresh_at < $2))`

func (w *SyncAllWorker) Work(ctx context.Context, job *river.Job[SyncAllArgs]) error {
	now := time.Now()

	rows, err := w.Pool.Query(ctx, dueItemsQuery,
		now.Add(-w.StaleAfter), now.Add(-w.RefreshAfter))
	if err != nil {
		return fmt.Errorf("list due items: %w", err)
	}
	defer rows.Close()

	type dueItem struct {
		id             uuid.UUID
		refreshOverdue bool
	}
	var items []dueItem
	for rows.Next() {
		var it dueItem
		if err := rows.Scan(&it.id, &it.refreshOverdue); err != nil {
			return fmt.Errorf("scan item id: %w", err)
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	refreshed, synced := 0, 0
	for _, it := range items {
		// Ask Plaid to go to the bank first. It answers immediately and pulls
		// asynchronously, so the sync enqueued below will usually still read
		// the old cache — that is fine and deliberate. The pull lands moments
		// later as a SYNC_UPDATES_AVAILABLE webhook, which enqueues the sync
		// that actually stores the new rows.
		if it.refreshOverdue && w.Syncer != nil {
			if err := w.Syncer.RefreshItem(ctx, it.id); err != nil {
				// A refresh failing must not stop the sync: the cached data is
				// still worth reading, and the item may simply be rate limited.
				slog.Warn("refresh item", "error", err, "item_id", it.id)
			} else {
				refreshed++
			}
		}

		// Synced whenever the item was touched at all, including when only the
		// refresh was due. The webhook is the expected path for picking up what
		// the refresh produces, so this is the fallback for when it never
		// arrives — without it a broken webhook would leave freshly pulled data
		// sitting at Plaid unread. River's per-item uniqueness collapses the
		// duplicate when the webhook does arrive.
		if _, err := w.Client.Insert(ctx, SyncItemArgs{ItemID: it.id}, nil); err != nil {
			slog.Error("enqueue sync", "error", err, "item_id", it.id)
		} else {
			synced++
		}
	}

	slog.Info("scheduled syncs", "items", len(items), "synced", synced, "refreshed", refreshed)
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
