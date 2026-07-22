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

// NewInsertOnlyClient builds a client that can enqueue jobs but never executes
// them. The api uses this: it should hand work to the worker, not do it.
func NewInsertOnlyClient(pool *pgxpool.Pool) (*river.Client[pgx.Tx], error) {
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	if err != nil {
		return nil, fmt.Errorf("create river insert-only client: %w", err)
	}
	return client, nil
}

// NewWorkerClient builds the client that actually runs jobs.
func NewWorkerClient(pool *pgxpool.Pool, syncer *plaid.Syncer) (*river.Client[pgx.Tx], error) {
	workers := river.NewWorkers()
	queries := dbgen.New(pool)

	// The net-worth snapshot does not depend on Plaid, so it is registered
	// whether or not credentials are configured — manual assets alone are
	// enough to make a net-worth figure worth recording.
	if err := river.AddWorkerSafely(workers, &SnapshotNetWorthWorker{Queries: queries}); err != nil {
		return nil, fmt.Errorf("register net worth worker: %w", err)
	}

	// A sync client is only registered when Plaid is configured. Without
	// credentials the queue still runs; there is simply nothing to sync.
	if syncer != nil {
		if err := river.AddWorkerSafely(workers, &SyncItemWorker{Syncer: syncer}); err != nil {
			return nil, fmt.Errorf("register sync worker: %w", err)
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

	client, err := river.NewClient(riverpgxv5.New(pool), config)
	if err != nil {
		return nil, fmt.Errorf("create river worker client: %w", err)
	}

	// SyncAllWorker needs the client to enqueue per-item jobs, which is only
	// available once the client exists — hence registration after construction.
	if syncer != nil {
		if err := river.AddWorkerSafely(workers, &SyncAllWorker{
			Pool: pool, Client: client, StaleAfter: staleAfter,
		}); err != nil {
			return nil, fmt.Errorf("register sync-all worker: %w", err)
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
