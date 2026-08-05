package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/ai"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/logos"
)

// FetchMerchantLogosArgs runs the opt-in logo pass for one household.
type FetchMerchantLogosArgs struct {
	HouseholdID uuid.UUID `json:"household_id"`
}

func (FetchMerchantLogosArgs) Kind() string { return "fetch_merchant_logos" }

// InsertOpts collapses a burst of enqueues for one household into a single pass
// per hour, exactly as SuggestMerchantsArgs does and for the same reason: the
// set of merchants without a cached logo shrinks to zero and stays there, so
// running the pass more often only re-reads an empty list.
func (a FetchMerchantLogosArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: QueueDefault,
		UniqueOpts: river.UniqueOpts{
			ByArgs:   true,
			ByState:  append(rivertype.UniqueOptsByStateDefault(), rivertype.JobStateRetryable),
			ByPeriod: time.Hour,
		},
	}
}

// FetchMerchantLogosWorker resolves merchant names to domains via the AI
// provider and caches the matching logos from Logo.dev.
//
// Registered only when the operator opted in AND an AI key is configured (see
// NewWorkerClient), so an enqueued job cannot make an outbound request by
// accident. It checks the household's own preference again inside the pass,
// because an operator switch is not a household's consent.
type FetchMerchantLogosWorker struct {
	river.WorkerDefaults[FetchMerchantLogosArgs]
	Queries *dbgen.Queries
	AI      *ai.Client
	Fetcher *logos.Fetcher
}

func (w *FetchMerchantLogosWorker) Work(ctx context.Context, job *river.Job[FetchMerchantLogosArgs]) error {
	res, err := logos.FetchHousehold(ctx, w.Queries, w.AI, w.Fetcher, job.Args.HouseholdID, sharedUser)
	if err != nil {
		return fmt.Errorf("fetch merchant logos for household %s: %w", job.Args.HouseholdID, err)
	}
	if res.Considered > 0 {
		slog.Info("merchant logos fetched",
			"household_id", job.Args.HouseholdID,
			"considered", res.Considered, "found", res.Found, "no_logo", res.NoLogo)
	}
	return nil
}

// Timeout bounds a pass that makes a handful of model calls and up to a couple
// of hundred small image requests. Generous, because the first pass on an
// imported history is the only one that ever does real work.
func (w *FetchMerchantLogosWorker) Timeout(*river.Job[FetchMerchantLogosArgs]) time.Duration {
	return 15 * time.Minute
}

// FetchMerchantLogosAllArgs sweeps every household.
type FetchMerchantLogosAllArgs struct{}

func (FetchMerchantLogosAllArgs) Kind() string { return "fetch_merchant_logos_all" }

// FetchMerchantLogosAllWorker enqueues a per-household pass.
type FetchMerchantLogosAllWorker struct {
	river.WorkerDefaults[FetchMerchantLogosAllArgs]
	Queries *dbgen.Queries
	Client  *river.Client[pgx.Tx]
}

func (w *FetchMerchantLogosAllWorker) Work(ctx context.Context, job *river.Job[FetchMerchantLogosAllArgs]) error {
	ids, err := w.Queries.ListHouseholdIDs(ctx)
	if err != nil {
		return fmt.Errorf("list households: %w", err)
	}
	for _, id := range ids {
		if _, err := w.Client.Insert(ctx, FetchMerchantLogosArgs{HouseholdID: id}, nil); err != nil {
			slog.Error("enqueue merchant logos", "error", err, "household_id", id)
		}
	}
	return nil
}
