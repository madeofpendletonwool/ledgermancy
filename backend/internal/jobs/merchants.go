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
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/merchants"
)

// sharedUser is the representative user id the reporting queries are given from
// a background job, matching insights.sharedUser: uuid.Nil belongs to no real
// Plaid item, so `user_id = $2 OR is_shared` collapses to the shared items —
// the ones the whole household can see. A merge suggestion is household-wide, so
// proposing one off a private account's descriptors would leak it.
var sharedUser = uuid.Nil

// SuggestMerchantsArgs runs the merchant canonicalisation suggestion pass for
// one household.
type SuggestMerchantsArgs struct {
	HouseholdID uuid.UUID `json:"household_id"`
}

func (SuggestMerchantsArgs) Kind() string { return "suggest_merchants" }

// InsertOpts collapses a burst of enqueues for one household into a single pass
// per hour. Descriptor space changes slowly — a new merchant is a rare event
// next to a sync — and every pass re-examines every key, so running it more
// often buys nothing. Starts from River's required state set (see SyncItemArgs)
// so the insert is not silently rejected.
func (a SuggestMerchantsArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: QueueDefault,
		UniqueOpts: river.UniqueOpts{
			ByArgs:   true,
			ByState:  append(rivertype.UniqueOptsByStateDefault(), rivertype.JobStateRetryable),
			ByPeriod: time.Hour,
		},
	}
}

// SuggestMerchantsWorker proposes merges between fragmented merchant
// descriptors.
//
// Everything it writes is a *suggestion*: an alias row with source 'suggested',
// which every reporting query excludes. A pass therefore cannot change a single
// number in the app — it can only add rows to a review queue. That is what makes
// it safe to run unattended, and it is why the AI half of it is allowed to guess.
type SuggestMerchantsWorker struct {
	river.WorkerDefaults[SuggestMerchantsArgs]
	Queries *dbgen.Queries
	// AI may be nil or disabled; the deterministic pass runs regardless and is
	// the more valuable half.
	AI *ai.Client
}

func (w *SuggestMerchantsWorker) Work(ctx context.Context, job *river.Job[SuggestMerchantsArgs]) error {
	n, err := merchants.SuggestHousehold(ctx, w.Queries, w.AI, job.Args.HouseholdID, sharedUser)
	if err != nil {
		return fmt.Errorf("suggest merchants for household %s: %w", job.Args.HouseholdID, err)
	}
	if n > 0 {
		slog.Info("merchant merge suggestions written",
			"household_id", job.Args.HouseholdID, "descriptors_proposed", n)
	}
	return nil
}

// Timeout bounds a pass that may fan out to several batched model calls.
func (w *SuggestMerchantsWorker) Timeout(*river.Job[SuggestMerchantsArgs]) time.Duration {
	return 5 * time.Minute
}

// SuggestMerchantsAllArgs sweeps every household.
type SuggestMerchantsAllArgs struct{}

func (SuggestMerchantsAllArgs) Kind() string { return "suggest_merchants_all" }

// SuggestMerchantsAllWorker enqueues a per-household suggestion pass.
type SuggestMerchantsAllWorker struct {
	river.WorkerDefaults[SuggestMerchantsAllArgs]
	Queries *dbgen.Queries
	Client  *river.Client[pgx.Tx]
}

func (w *SuggestMerchantsAllWorker) Work(ctx context.Context, job *river.Job[SuggestMerchantsAllArgs]) error {
	ids, err := w.Queries.ListHouseholdIDs(ctx)
	if err != nil {
		return fmt.Errorf("list households: %w", err)
	}
	for _, id := range ids {
		if _, err := w.Client.Insert(ctx, SuggestMerchantsArgs{HouseholdID: id}, nil); err != nil {
			slog.Error("enqueue merchant suggestions", "error", err, "household_id", id)
		}
	}
	return nil
}
