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
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/networth"
)

// Automatic bond revaluation.
//
// # Why this one writes without asking
//
// Everywhere else in doc 26 an estimate is a PROPOSAL the user confirms, and
// for good reason: net worth must not move on a guess. This job is the single
// exception, and what licenses it is DETERMINISM, not convenience.
//
// A savings bond's value is not an estimate. It is arithmetic over rates the
// Treasury published, and the same bond on the same date gives the same answer
// to the cent no matter who computes it — the figures are checked against
// Treasury's own calculator in networth/bonds_test.go. There is no judgement in
// it to ask the user to ratify.
//
// The moment a valuation depends on a judgement it goes back to being a
// proposal. Do not extend this job to cover depreciation.
//
// # And why it refuses rather than approximating
//
// A bond whose valuation is not complete — a rate period the table does not
// cover, an EE bond from before the fixed-rate era, a missing issue date —
// writes NOTHING. Carrying the last known rate forward would invent a return,
// and a stale-but-plausible number in net worth is exactly what this feature
// exists to remove. The asset keeps whatever value it had and the UI says how
// far the rate table reached.

// RevalueBondsArgs revalues one household's directly-held bonds.
type RevalueBondsArgs struct {
	HouseholdID uuid.UUID `json:"household_id"`
}

func (RevalueBondsArgs) Kind() string { return "revalue_bonds" }

// RevalueBondsWorker walks a household's bonds and writes today's value for
// each one it can value completely.
type RevalueBondsWorker struct {
	river.WorkerDefaults[RevalueBondsArgs]
	Pool    *pgxpool.Pool
	Queries *dbgen.Queries
}

func (w *RevalueBondsWorker) Work(ctx context.Context, job *river.Job[RevalueBondsArgs]) error {
	written, refused, err := RevalueBonds(ctx, w.Pool, w.Queries, job.Args.HouseholdID, time.Now().UTC())
	if err != nil {
		return err
	}
	if written > 0 || refused > 0 {
		slog.Info("bonds revalued",
			"household_id", job.Args.HouseholdID, "written", written, "refused", refused)
	}
	return nil
}

// RevalueBonds is the worker's body, exported so it can be tested against a
// real database without going through the queue.
//
// `now` is a parameter, so a test never depends on the clock and the same
// inputs always produce the same writes.
func RevalueBonds(
	ctx context.Context,
	pool *pgxpool.Pool,
	q *dbgen.Queries,
	householdID uuid.UUID,
	now time.Time,
) (written, refused int, err error) {
	assets, err := q.ListBondAssets(ctx, householdID)
	if err != nil {
		return 0, 0, fmt.Errorf("list bond assets: %w", err)
	}
	if len(assets) == 0 {
		return 0, 0, nil
	}

	rates, err := networth.LoadBondRates(ctx, q)
	if err != nil {
		return 0, 0, fmt.Errorf("load savings bond rates: %w", err)
	}

	asOf := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	for _, row := range assets {
		bond, ok := networth.BondFromAssetRow(row)
		if !ok {
			refused++
			continue
		}

		v := networth.ValueBond(bond, rates, now)
		if !v.OK {
			// The refusal is the point. Log it at debug — a bond waiting on the
			// next rate announcement is an expected state, not a fault.
			slog.Debug("bond not revalued",
				"asset_id", row.ID, "reason", v.Reason)
			refused++
			continue
		}

		// Nothing to write if the value has not moved. A bond only changes on
		// an accrual date, so most daily runs are no-ops, and skipping them
		// keeps the history a record of changes rather than one row per day.
		if v.Redemption.Equal(row.Value) {
			continue
		}

		if err := writeValuation(ctx, pool, q, householdID, row.ID, v.Redemption, asOf,
			valuationNote(v)); err != nil {
			return written, refused, fmt.Errorf("revalue %s: %w", row.Name, err)
		}
		written++
	}
	return written, refused, nil
}

// writeValuation updates the current value and appends to the history in ONE
// transaction.
//
// manual_assets.value is what every net-worth query reads and asset_valuations
// is the trend behind it. A failure that left one without the other would make
// the headline figure and its own explanation disagree.
func writeValuation(
	ctx context.Context,
	pool *pgxpool.Pool,
	q *dbgen.Queries,
	householdID, assetID uuid.UUID,
	value decimal.Decimal,
	asOf time.Time,
	note string,
) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	qtx := q.WithTx(tx)

	if _, err := qtx.InsertAssetValuation(ctx, dbgen.InsertAssetValuationParams{
		ManualAssetID: assetID,
		Value:         value,
		AsOf:          asOf,
		Source:        "estimated",
		Note:          &note,
	}); err != nil {
		return err
	}
	if _, err := qtx.SetManualAssetValue(ctx, dbgen.SetManualAssetValueParams{
		ID: assetID, HouseholdID: householdID, Value: value, AsOf: asOf,
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// valuationNote records which published periods produced the figure, so a
// history row can be walked back to treasurydirect.gov by hand rather than
// being taken on trust.
func valuationNote(v networth.BondValue) string {
	note := fmt.Sprintf("Redemption value from %d published rate period(s), valued %s.",
		len(v.Rates), v.AsOf.Format("January 2006"))
	if len(v.Rates) > 0 {
		last := v.Rates[len(v.Rates)-1]
		note += fmt.Sprintf(" Current period from %s at %s%%.",
			last.Announced.Format("January 2006"), last.Composite.StringFixed(2))
	}
	if v.PenaltyApplied {
		note += " Under five years old, so the last three months' interest is forfeit on redemption."
	}
	if v.DoublingApplied {
		note += " Treasury's 20-year doubling guarantee applies."
	}
	if v.Matured {
		note += " The bond has reached final maturity and stopped earning."
	}
	return note
}

// RevalueBondsAllArgs fans the revaluation out across every household.
type RevalueBondsAllArgs struct{}

func (RevalueBondsAllArgs) Kind() string { return "revalue_bonds_all" }

type RevalueBondsAllWorker struct {
	river.WorkerDefaults[RevalueBondsAllArgs]
	Client  *river.Client[pgx.Tx]
	Queries *dbgen.Queries
}

func (w *RevalueBondsAllWorker) Work(ctx context.Context, job *river.Job[RevalueBondsAllArgs]) error {
	ids, err := w.Queries.ListHouseholdIDs(ctx)
	if err != nil {
		return fmt.Errorf("list households: %w", err)
	}
	for _, id := range ids {
		if _, err := w.Client.Insert(ctx, RevalueBondsArgs{HouseholdID: id}, nil); err != nil {
			slog.Error("enqueue bond revaluation", "household_id", id, "error", err)
		}
	}
	return nil
}
