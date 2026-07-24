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
	"github.com/apex42group/ledgermancy/backend/internal/notify"
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

// refreshInterval is how often each item is pushed to pull fresh data from the
// institution, as opposed to re-reading what Plaid already cached.
//
// These are different operations with very different costs. /transactions/sync
// is cheap and returns Plaid's cache; without a refresh it can return the same
// rows indefinitely, because Plaid only goes to the bank on its own schedule —
// which has been observed stalling for many hours on some institutions.
// /transactions/refresh forces that pull but is rate limited per item, so four
// hours (six calls a day) keeps data fresh while staying well inside the quota.
const refreshInterval = 4 * time.Hour

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

// insightInterval is how often every household's proactive feed is regenerated
// independently of syncs, so a quiet household still surfaces a budget crossing
// the calendar rolls into. Detection is cheap deterministic SQL; phrasing (when
// AI is configured) only runs on the candidates a household actually has.
const insightInterval = time.Hour

// digestSweepInterval is how often the digest sweep runs. It runs hourly but the
// cadence gating inside the sweep decides who is actually due (weekly users on
// Monday, monthly users on the 1st/2nd), so it is cheap when nobody is — the
// same pattern as SyncAllWorker. The period_key dedupe makes exact timing safe.
const digestSweepInterval = time.Hour

// securitySweepInterval is how often expired auth state is collected. Nothing
// depends on it being prompt — every read path already filters on expiry — so
// this is tuned to keep tables tidy, not to enforce anything.
const securitySweepInterval = 6 * time.Hour

// summaryRefreshInterval is how often the monthly-recap auto-generation sweep
// runs. Daily: the worker throttles the actual model calls (weekly-ish per
// household, once per completed month), so the frequent tick only exists to
// catch month rollover promptly rather than to regenerate anything daily.
const summaryRefreshInterval = 24 * time.Hour

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
// registered, so the queue behaves exactly as it did before phase 6. The
// notifier is likewise always passed and always registered — it is not
// AI-gated; delivery is gated per-user inside the notifier. appURL is the
// frontend origin used to build notification deep links.
func NewWorkerClient(pool *pgxpool.Pool, syncer *plaid.Syncer, aiClient *ai.Client, notifier notify.Notifier, appURL string) (*river.Client[pgx.Tx], error) {
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
	// Its worker enqueues notify jobs, so it needs the client and is registered
	// after construction (below), not here.

	// Push delivery is not AI-gated: the notifier is always constructed and the
	// worker always registered, with per-user gating inside Send.
	if err := river.AddWorkerSafely(workers, &NotifyWorker{Notifier: notifier}); err != nil {
		return nil, fmt.Errorf("register notify worker: %w", err)
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

	// Insight generation is deterministic and useful without a key — only the
	// phrasing inside insights.Generate is AI-gated — so the per-household
	// worker is registered unconditionally (unlike the LLM workers below). This
	// is a deliberate deviation from the shared-contract's "AI-gated periodic
	// jobs" list: literal AI-gating would leave the feed empty without a key,
	// defeating the point of a deterministic feed. The AI client is still passed
	// through; the engine no-ops the phrasing when it is disabled. The sweep
	// that enqueues this worker needs the client and is registered afterwards.
	if err := river.AddWorkerSafely(workers, &GenerateInsightsWorker{Queries: queries, AI: aiClient}); err != nil {
		return nil, fmt.Errorf("register insights worker: %w", err)
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
		// The insight sweep is deterministic (phrasing is separately AI-gated),
		// so it runs whether or not a key is configured.
		river.NewPeriodicJob(
			river.PeriodicInterval(insightInterval),
			func() (river.JobArgs, *river.InsertOpts) {
				return GenerateInsightsAllArgs{}, nil
			},
			&river.PeriodicJobOpts{RunOnStart: true},
		),
		// The digest sweep runs unconditionally; cadence gating inside decides who
		// is due, and the summary call self-gates on AI being enabled. Not
		// RunOnStart — a digest is a scheduled outbound push, not something to fire
		// on every worker restart.
		river.NewPeriodicJob(
			river.PeriodicInterval(digestSweepInterval),
			func() (river.JobArgs, *river.InsertOpts) {
				return DigestSweepArgs{}, nil
			},
			nil,
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

		// Recap auto-generation sweep. Runs daily, but the worker itself keeps the
		// current month's recap on a weekly-ish refresh and finalises a completed
		// month exactly once — so a daily tick is cheap and reliably catches month
		// rollover regardless of when the interval happens to fall. RunOnStart so a
		// fresh deploy warms recaps without waiting a day.
		config.PeriodicJobs = append(config.PeriodicJobs, river.NewPeriodicJob(
			river.PeriodicInterval(summaryRefreshInterval),
			func() (river.JobArgs, *river.InsertOpts) {
				return SummaryRefreshSweepArgs{}, nil
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
			Pool: pool, Client: client, Syncer: syncer,
			StaleAfter: staleAfter, RefreshAfter: refreshInterval,
		}); err != nil {
			return nil, fmt.Errorf("register sync-all worker: %w", err)
		}
	}

	// The per-household evaluation worker enqueues notify jobs on the client, so
	// it is registered after construction like the sweeps.
	if err := river.AddWorkerSafely(workers, &EvaluateAlertsWorker{
		Queries: queries, Client: client, AppURL: appURL,
	}); err != nil {
		return nil, fmt.Errorf("register alerts worker: %w", err)
	}

	if err := river.AddWorkerSafely(workers, &EvaluateAlertsAllWorker{
		Queries: queries, Client: client,
	}); err != nil {
		return nil, fmt.Errorf("register alerts sweep worker: %w", err)
	}

	// The insight sweep enqueues per-household jobs, so it needs the client and
	// is registered after construction. Always on, like the alert sweep.
	if err := river.AddWorkerSafely(workers, &GenerateInsightsAllWorker{
		Queries: queries, Client: client,
	}); err != nil {
		return nil, fmt.Errorf("register insights sweep worker: %w", err)
	}

	// Digest sweep + per-user worker. Both enqueue other jobs (the sweep enqueues
	// DigestArgs; the worker enqueues NotifyArgs), so they need the client and are
	// registered after construction. Registered unconditionally — the deterministic
	// parts (top insights) send without AI, and the summary call self-gates on
	// Enabled() inside the worker.
	if err := river.AddWorkerSafely(workers, &DigestWorker{
		Queries: queries, AI: aiClient, Client: client, AppURL: appURL,
	}); err != nil {
		return nil, fmt.Errorf("register digest worker: %w", err)
	}
	if err := river.AddWorkerSafely(workers, &DigestSweepWorker{
		Queries: queries, Client: client,
	}); err != nil {
		return nil, fmt.Errorf("register digest sweep worker: %w", err)
	}

	if aiEnabled {
		if err := river.AddWorkerSafely(workers, &LLMCategoriseAllWorker{
			Queries: queries, Client: client,
		}); err != nil {
			return nil, fmt.Errorf("register llm sweep worker: %w", err)
		}

		// Monthly recap auto-generation. The sweep fans out per household (needs the
		// client); the worker generates and caches (needs AI). Both are AI-only, so
		// they are registered here alongside the periodic sweep below.
		if err := river.AddWorkerSafely(workers, &SummaryRefreshSweepWorker{
			Queries: queries, Client: client,
		}); err != nil {
			return nil, fmt.Errorf("register summary refresh sweep worker: %w", err)
		}
		if err := river.AddWorkerSafely(workers, &SummaryRefreshWorker{
			Queries: queries, AI: aiClient,
		}); err != nil {
			return nil, fmt.Errorf("register summary refresh worker: %w", err)
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

// EnqueueLLMCategorise kicks the AI categorisation pass for a household now,
// rather than waiting for the periodic sweep — used right after a CSV import so
// freshly-imported merchants get placed promptly. Fire-and-forget: a failure to
// enqueue just means the periodic sweep picks them up on its next run.
func EnqueueLLMCategorise(ctx context.Context, client *river.Client[pgx.Tx], householdID uuid.UUID) {
	if client == nil {
		return
	}
	if _, err := client.Insert(ctx, LLMCategoriseArgs{HouseholdID: householdID}, nil); err != nil {
		slog.Error("enqueue llm categorise", "error", err, "household_id", householdID)
	}
}

// EnqueueDigestNow queues a one-off "send now" digest for a single user,
// bypassing cadence and the per-period dedupe (see DigestArgs.Force). Unlike the
// fire-and-forget enqueues above it returns the error, so a Settings "send one
// now" action can tell the user when their request did not take.
func EnqueueDigestNow(ctx context.Context, client *river.Client[pgx.Tx], userID, householdID uuid.UUID) error {
	if client == nil {
		return fmt.Errorf("background jobs are not available")
	}
	if _, err := client.Insert(ctx, DigestArgs{
		UserID: userID, HouseholdID: householdID, Force: true,
	}, nil); err != nil {
		return fmt.Errorf("enqueue digest: %w", err)
	}
	return nil
}
