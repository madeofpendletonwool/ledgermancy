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

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/ai"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/config"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/notify"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/plaid"
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

// benchmarkInterval is how often end-of-day benchmark closes are fetched. Once
// a day is the natural cadence for a series that only gains one point a day,
// and it keeps the request count at a free third-party host trivially low.
const benchmarkInterval = 24 * time.Hour

// llmSweepInterval is how often every household is swept for uncategorised
// merchants. Cheap when there is nothing to do — a swept household with no new
// merchants makes zero model calls — so a short interval just keeps latency low.
const llmSweepInterval = 15 * time.Minute

// alertSweepInterval is how often every household's alerts are re-evaluated
// independently of syncs, so a budget crossing the calendar rolls into or a
// quiet household still surfaces. Evaluation is cheap deterministic SQL.
const alertSweepInterval = 30 * time.Minute

// allowancePostInterval is how often scheduled allowances are checked. Hourly
// is not a payment cadence — the cadence lives on each allowance row. This is
// only how often the app asks "is anybody due?", and running it often is what
// lets a server that was off overnight still pay on the right day.
const allowancePostInterval = time.Hour

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

// merchantSuggestInterval is how often every household is swept for merchant
// merge candidates. Daily, because the descriptor space barely moves — a new
// merchant is rare next to a sync — and because the pass compares every key
// against every other, so a tighter cadence would re-do identical work to
// produce an identical review queue.
const merchantSuggestInterval = 24 * time.Hour

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
func NewWorkerClient(pool *pgxpool.Pool, syncer *plaid.Syncer, aiClient *ai.Client, notifier notify.Notifier, appURL string, benchmarks config.BenchmarkConfig) (*river.Client[pgx.Tx], error) {
	workers := river.NewWorkers()
	queries := dbgen.New(pool)
	aiEnabled := aiClient != nil && aiClient.Enabled()

	// The net-worth snapshot does not depend on Plaid, so it is registered
	// whether or not credentials are configured — manual assets alone are
	// enough to make a net-worth figure worth recording.
	if err := river.AddWorkerSafely(workers, &SnapshotNetWorthWorker{Queries: queries}); err != nil {
		return nil, fmt.Errorf("register net worth worker: %w", err)
	}

	// Investment value snapshots, for the same reason and on the same schedule:
	// Plaid keeps no history of what a portfolio was worth, so a day not
	// recorded is a day that can never be recovered. Registered unconditionally
	// — it reads stored holdings and balances, not Plaid.
	if err := river.AddWorkerSafely(workers, &SnapshotInvestmentsWorker{Queries: queries}); err != nil {
		return nil, fmt.Errorf("register investment snapshot worker: %w", err)
	}

	// Benchmark prices are opt-in: this is the only outbound call to a host that
	// is neither Plaid nor the AI provider. Not registered at all when off, so
	// an enqueued job cannot run by accident.
	if benchmarks.Enabled && len(benchmarks.Tickers) > 0 {
		if err := river.AddWorkerSafely(workers, &FetchBenchmarksWorker{
			Queries: queries, Tickers: benchmarks.Tickers,
		}); err != nil {
			return nil, fmt.Errorf("register benchmark worker: %w", err)
		}
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

	// The per-household categorise worker only needs the AI client and queries,
	// so it is registered before construction. The sweep that enqueues it needs
	// the client and is registered afterwards. SyncItemWorker is registered
	// after the client exists too, so it can enqueue follow-up work.
	if aiEnabled {
		if err := river.AddWorkerSafely(workers, &LLMCategoriseWorker{Queries: queries, AI: aiClient}); err != nil {
			return nil, fmt.Errorf("register llm categorise worker: %w", err)
		}
	}

	// Merchant canonicalisation is registered unconditionally: its first pass is
	// pure string work and is the valuable half, with the model only ever seeing
	// the residue. Gating the whole thing on a key would deny a keyless
	// deployment the merges it can compute for free.
	if err := river.AddWorkerSafely(workers, &SuggestMerchantsWorker{
		Queries: queries, AI: aiClient,
	}); err != nil {
		return nil, fmt.Errorf("register merchant suggestion worker: %w", err)
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

	// Scheduled allowance credits. Registered unconditionally: it needs neither
	// Plaid nor an AI key, and a household using the allowance ledger without
	// either is a perfectly ordinary way to run this app.
	if err := river.AddWorkerSafely(workers, &PostAllowancesWorker{Queries: queries}); err != nil {
		return nil, fmt.Errorf("register allowance worker: %w", err)
	}

	config.PeriodicJobs = []*river.PeriodicJob{
		river.NewPeriodicJob(
			river.PeriodicInterval(snapshotInterval),
			func() (river.JobArgs, *river.InsertOpts) {
				return SnapshotNetWorthArgs{}, nil
			},
			&river.PeriodicJobOpts{RunOnStart: true},
		),
		// Rides the same interval as the net-worth snapshot: both record a
		// point-in-time balance that cannot be reconstructed later, and keeping
		// them on one cadence means the two trends never disagree about which
		// days exist.
		river.NewPeriodicJob(
			river.PeriodicInterval(snapshotInterval),
			func() (river.JobArgs, *river.InsertOpts) {
				return SnapshotInvestmentsArgs{}, nil
			},
			&river.PeriodicJobOpts{RunOnStart: true},
		),
		// Hourly rather than daily. The period boundary in allowances is the
		// idempotency key, so running often costs nothing and means a household
		// whose server was asleep at midnight still gets paid.
		river.NewPeriodicJob(
			river.PeriodicInterval(allowancePostInterval),
			func() (river.JobArgs, *river.InsertOpts) {
				return PostAllowancesArgs{}, nil
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
		// Merchant merge suggestions. Deterministic first pass, so like the
		// insight sweep it runs with or without a key. RunOnStart: the queue it
		// fills is inert, so warming it on deploy costs nothing and means a fresh
		// install has something to review immediately.
		river.NewPeriodicJob(
			river.PeriodicInterval(merchantSuggestInterval),
			func() (river.JobArgs, *river.InsertOpts) {
				return SuggestMerchantsAllArgs{}, nil
			},
			&river.PeriodicJobOpts{RunOnStart: true},
		),
	}

	// Benchmark closes only change once a day, after the market settles. Not
	// RunOnStart: a restart loop must not turn into a burst of requests at a
	// third-party host that is doing us a favour by being free.
	if benchmarks.Enabled && len(benchmarks.Tickers) > 0 {
		config.PeriodicJobs = append(config.PeriodicJobs, river.NewPeriodicJob(
			river.PeriodicInterval(benchmarkInterval),
			func() (river.JobArgs, *river.InsertOpts) {
				return FetchBenchmarksArgs{}, nil
			},
			nil,
		))
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

	// Insight generation is deterministic and useful without a key — only the
	// phrasing inside insights.Generate is AI-gated — so the per-household worker
	// is registered unconditionally (unlike the LLM workers). Literal AI-gating
	// would leave the feed empty without a key, defeating a deterministic feed.
	// It carries the client and app URL to push the highest-priority new insights,
	// so it is registered after construction like the sweeps.
	if err := river.AddWorkerSafely(workers, &GenerateInsightsWorker{
		Queries: queries, AI: aiClient, Client: client, AppURL: appURL,
	}); err != nil {
		return nil, fmt.Errorf("register insights worker: %w", err)
	}

	// The insight sweep enqueues per-household jobs, so it needs the client and
	// is registered after construction. Always on, like the alert sweep.
	if err := river.AddWorkerSafely(workers, &GenerateInsightsAllWorker{
		Queries: queries, Client: client,
	}); err != nil {
		return nil, fmt.Errorf("register insights sweep worker: %w", err)
	}

	// The merchant sweep enqueues per-household passes, so it needs the client
	// and is registered after construction.
	if err := river.AddWorkerSafely(workers, &SuggestMerchantsAllWorker{
		Queries: queries, Client: client,
	}); err != nil {
		return nil, fmt.Errorf("register merchant suggestion sweep worker: %w", err)
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

// EnqueueSuggestMerchants runs a merchant canonicalisation pass for a household
// now, rather than waiting for the daily sweep — the "scan for merges" button.
// It returns the error so the button can say when the request did not take;
// unlike a digest, the user is standing in front of the result.
//
// SuggestMerchantsArgs.InsertOpts collapses repeats to one pass per hour, so a
// user leaning on the button costs nothing.
func EnqueueSuggestMerchants(ctx context.Context, client *river.Client[pgx.Tx], householdID uuid.UUID) error {
	if client == nil {
		return fmt.Errorf("background jobs are not available")
	}
	if _, err := client.Insert(ctx, SuggestMerchantsArgs{HouseholdID: householdID}, nil); err != nil {
		return fmt.Errorf("enqueue merchant suggestions: %w", err)
	}
	return nil
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
