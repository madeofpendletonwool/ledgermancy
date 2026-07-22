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

	"github.com/apex42group/ledgermancy/backend/internal/ai"
	"github.com/apex42group/ledgermancy/backend/internal/auth"
	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
	"github.com/apex42group/ledgermancy/backend/internal/plaid"
)

// syncInterval is how often each item is refreshed when no webhook arrives.
// Plaid updates most institutions a few times a day, so hourly is generous
// without being wasteful.
const syncInterval = time.Hour

// staleAfter is how old an item's last sync must be to be picked up by the
// periodic sweep. Slightly under syncInterval so an item is not skipped
// because the sweep ran a moment early.
const staleAfter = 55 * time.Minute

// snapshotInterval is how often net worth is recorded. Daily granularity is
// what the trend chart shows; running more often simply overwrites the day's
// row, which is harmless but pointless.
const snapshotInterval = 12 * time.Hour

// llmSweepInterval is how often every household is swept for uncategorised
// merchants. Cheap when there is nothing to do — a swept household with no new
// merchants makes zero model calls — so a short interval just keeps latency low.
const llmSweepInterval = 15 * time.Minute

// alertSweepInterval is how often every household's alerts are re-evaluated
// independently of syncs, so a budget crossing the calendar rolls into or a
// quiet household still surfaces. Evaluation is cheap deterministic SQL.
const alertSweepInterval = 30 * time.Minute

// securitySweepInterval is how often expired auth state is collected. Nothing
// depends on it being prompt — every read path already filters on expiry — so
// this is tuned to keep tables tidy, not to enforce anything.
const securitySweepInterval = 6 * time.Hour

// authEventRetention is how long the auth audit log is kept. Long enough to
// investigate something noticed weeks later; short enough that a household
// deployment never accumulates a table worth worrying about.
const authEventRetention = 180 * 24 * time.Hour

// NewInsertOnlyClient builds a client that can enqueue jobs but never executes
// them. The api uses this: it should hand work to the worker, not do it.
func NewInsertOnlyClient(pool *pgxpool.Pool) (*river.Client[pgx.Tx], error) {
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	if err != nil {
		return nil, fmt.Errorf("create river insert-only client: %w", err)
	}
	return client, nil
}

// NewWorkerClient builds the client that actually runs jobs. The AI client is
// always passed; when it is disabled the categorisation jobs are simply not
// registered, so the queue behaves exactly as it did before phase 6.
func NewWorkerClient(pool *pgxpool.Pool, syncer *plaid.Syncer, aiClient *ai.Client) (*river.Client[pgx.Tx], error) {
	workers := river.NewWorkers()
	queries := dbgen.New(pool)
	aiEnabled := aiClient != nil && aiClient.Enabled()

	// The net-worth snapshot does not depend on Plaid, so it is registered
	// whether or not credentials are configured — manual assets alone are
	// enough to make a net-worth figure worth recording.
	if err := river.AddWorkerSafely(workers, &SnapshotNetWorthWorker{Queries: queries}); err != nil {
		return nil, fmt.Errorf("register net worth worker: %w", err)
	}

	// Alert evaluation is deterministic and AI-independent, so it always runs.
	if err := river.AddWorkerSafely(workers, &EvaluateAlertsWorker{Queries: queries}); err != nil {
		return nil, fmt.Errorf("register alerts worker: %w", err)
	}

	// Housekeeping for expired sessions, abandoned MFA challenges and old audit
	// rows. Like the snapshot, it has nothing to do with Plaid, so it runs
	// whether or not credentials are configured.
	if err := river.AddWorkerSafely(workers, &SecuritySweepWorker{
		Queries:      queries,
		IdleTTL:      auth.SessionIdleTTL,
		AuthEventTTL: authEventRetention,
	}); err != nil {
		return nil, fmt.Errorf("register security sweep worker: %w", err)
	}

	// The per-household categorise worker only needs the AI client and queries,
	// so it is registered before construction. The sweep that enqueues it needs
	// the client and is registered afterwards. SyncItemWorker is registered
	// after the client exists too, so it can enqueue follow-up work.
	if aiEnabled {
		if err := river.AddWorkerSafely(workers, &LLMCategoriseWorker{Queries: queries, AI: aiClient}); err != nil {
			return nil, fmt.Errorf("register llm categorise worker: %w", err)
		}
	}

	config := &river.Config{
		Queues: map[string]river.QueueConfig{
			// A household has a handful of institutions; more concurrency
			// would only mean more simultaneous Plaid rate-limit pressure.
			QueueDefault: {MaxWorkers: 4},
		},
		Workers:     workers,
		JobTimeout:  10 * time.Minute,
		MaxAttempts: 5,
		Logger:      slog.Default(),
	}

	config.PeriodicJobs = []*river.PeriodicJob{
		river.NewPeriodicJob(
			river.PeriodicInterval(snapshotInterval),
			func() (river.JobArgs, *river.InsertOpts) {
				return SnapshotNetWorthArgs{}, nil
			},
			&river.PeriodicJobOpts{RunOnStart: true},
		),
		river.NewPeriodicJob(
			river.PeriodicInterval(alertSweepInterval),
			func() (river.JobArgs, *river.InsertOpts) {
				return EvaluateAlertsAllArgs{}, nil
			},
			&river.PeriodicJobOpts{RunOnStart: true},
		),
		river.NewPeriodicJob(
			river.PeriodicInterval(securitySweepInterval),
			func() (river.JobArgs, *river.InsertOpts) {
				return SecuritySweepArgs{}, nil
			},
			&river.PeriodicJobOpts{RunOnStart: true},
		),
	}

	if syncer != nil {
		config.PeriodicJobs = append(config.PeriodicJobs, river.NewPeriodicJob(
			river.PeriodicInterval(syncInterval),
			func() (river.JobArgs, *river.InsertOpts) {
				return SyncAllArgs{}, nil
			},
			&river.PeriodicJobOpts{RunOnStart: true},
		))
	}

	if aiEnabled {
		config.PeriodicJobs = append(config.PeriodicJobs, river.NewPeriodicJob(
			river.PeriodicInterval(llmSweepInterval),
			func() (river.JobArgs, *river.InsertOpts) {
				return LLMCategoriseAllArgs{}, nil
			},
			&river.PeriodicJobOpts{RunOnStart: true},
		))
	}

	client, err := river.NewClient(riverpgxv5.New(pool), config)
	if err != nil {
		return nil, fmt.Errorf("create river worker client: %w", err)
	}

	// Workers that enqueue other jobs need the client, which only exists now —
	// hence registration after construction. River reads the worker set at
	// Start, so adding to it here is safe.
	if syncer != nil {
		// A finished sync always triggers alert evaluation for its household,
		// and AI categorisation too when AI is enabled.
		item := &SyncItemWorker{
			Syncer: syncer, Client: client, Queries: queries, EnqueueLLM: aiEnabled,
		}
		if err := river.AddWorkerSafely(workers, item); err != nil {
			return nil, fmt.Errorf("register sync worker: %w", err)
		}

		if err := river.AddWorkerSafely(workers, &SyncAllWorker{
			Pool: pool, Client: client, StaleAfter: staleAfter,
		}); err != nil {
			return nil, fmt.Errorf("register sync-all worker: %w", err)
		}
	}

	if err := river.AddWorkerSafely(workers, &EvaluateAlertsAllWorker{
		Queries: queries, Client: client,
	}); err != nil {
		return nil, fmt.Errorf("register alerts sweep worker: %w", err)
	}

	if aiEnabled {
		if err := river.AddWorkerSafely(workers, &LLMCategoriseAllWorker{
			Queries: queries, Client: client,
		}); err != nil {
			return nil, fmt.Errorf("register llm sweep worker: %w", err)
		}
	}

	return client, nil
}

// EnqueueSync schedules a sync for one item, tolerating a nil client so the
// caller does not have to branch when Plaid is not configured.
func EnqueueSync(ctx context.Context, client *river.Client[pgx.Tx], itemID uuid.UUID) {
	if client == nil {
		return
	}
	if _, err := client.Insert(ctx, SyncItemArgs{ItemID: itemID}, nil); err != nil {
		slog.Error("enqueue plaid sync", "error", err, "item_id", itemID)
	}
}

// EnqueueAlertEval schedules an alert evaluation for one household, tolerating a
// nil client so the API can call it without knowing whether a queue is wired.
func EnqueueAlertEval(ctx context.Context, client *river.Client[pgx.Tx], householdID uuid.UUID) {
	if client == nil {
		return
	}
	if _, err := client.Insert(ctx, EvaluateAlertsArgs{HouseholdID: householdID}, nil); err != nil {
		slog.Error("enqueue alert evaluation", "error", err, "household_id", householdID)
	}
}
