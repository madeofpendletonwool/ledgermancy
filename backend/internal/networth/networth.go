// Package networth computes what a household owns minus what it owes, and
// records it over time.
package networth

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// Breakdown is the composition of a net-worth figure. Stored alongside each
// snapshot so the make-up of a past month survives even after accounts are
// closed or unlinked.
type Breakdown struct {
	Cash         decimal.Decimal `json:"cash"`
	Investments  decimal.Decimal `json:"investments"`
	OtherAssets  decimal.Decimal `json:"other_assets"`
	ManualAssets decimal.Decimal `json:"manual_assets"`
	CreditDebt   decimal.Decimal `json:"credit_debt"`
	LoanDebt     decimal.Decimal `json:"loan_debt"`
	ManualDebt   decimal.Decimal `json:"manual_debt"`
}

// Result is a computed net-worth position.
type Result struct {
	AssetsTotal      decimal.Decimal
	LiabilitiesTotal decimal.Decimal
	NetWorth         decimal.Decimal
	Breakdown        Breakdown
}

// Compute totals a household's current position.
//
// Plaid reports credit and loan balances as positive numbers representing the
// amount owed, so they are summed as-is into liabilities and subtracted —
// never added with a sign flip, which would double the error if a provider
// ever changed convention.
func Compute(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID) (Result, error) {
	row, err := q.ComputeNetWorth(ctx, householdID)
	if err != nil {
		return Result{}, fmt.Errorf("compute net worth: %w", err)
	}

	b := Breakdown{
		Cash:         row.Cash,
		Investments:  row.Investments,
		OtherAssets:  row.OtherAssets,
		ManualAssets: row.ManualAssets,
		CreditDebt:   row.CreditDebt,
		LoanDebt:     row.LoanDebt,
		ManualDebt:   row.ManualDebt,
	}

	assets := b.Cash.Add(b.Investments).Add(b.OtherAssets).Add(b.ManualAssets)
	liabilities := b.CreditDebt.Add(b.LoanDebt).Add(b.ManualDebt)

	return Result{
		AssetsTotal:      assets,
		LiabilitiesTotal: liabilities,
		NetWorth:         assets.Sub(liabilities),
		Breakdown:        b,
	}, nil
}

// Snapshot computes and records the household's position for a given day.
//
// Snapshots exist because balances have no history: Plaid reports what an
// account holds right now, so a net-worth trend can only be built by writing
// the figure down as time passes. Re-running on the same day replaces that
// day's row rather than adding a second one.
func Snapshot(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, asOf *time.Time) (Result, error) {
	result, err := Compute(ctx, q, householdID)
	if err != nil {
		return Result{}, err
	}

	breakdown, err := json.Marshal(result.Breakdown)
	if err != nil {
		return Result{}, fmt.Errorf("marshal breakdown: %w", err)
	}

	if _, err := q.UpsertNetWorthSnapshot(ctx, dbgen.UpsertNetWorthSnapshotParams{
		HouseholdID:      householdID,
		AsOf:             asOf,
		AssetsTotal:      result.AssetsTotal,
		LiabilitiesTotal: result.LiabilitiesTotal,
		NetWorth:         result.NetWorth,
		Breakdown:        breakdown,
	}); err != nil {
		return Result{}, fmt.Errorf("save snapshot: %w", err)
	}

	return result, nil
}

// SnapshotAll records a snapshot for every household. Used by the scheduled job.
func SnapshotAll(ctx context.Context, q *dbgen.Queries) (int, error) {
	ids, err := q.ListHouseholdIDs(ctx)
	if err != nil {
		return 0, fmt.Errorf("list households: %w", err)
	}

	count := 0
	for _, id := range ids {
		if _, err := Snapshot(ctx, q, id, nil); err != nil {
			return count, err
		}
		// The per-account balance points are recorded alongside the household
		// total so a quiet Plaid account still gets a point on its own trend on
		// the same schedule — the same reason this loop runs at all when a
		// household's institutions have nothing to sync.
		if _, err := SnapshotAccountBalances(ctx, q, id); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

// SnapshotAccountBalances records each Plaid account's current balance for
// today — the per-account analog of Snapshot above. A Plaid account's balance
// carries no history (Plaid reports only the current figure), so a per-account
// trend can only be built by writing the figure down as it goes, exactly as
// Snapshot does for the household total. Manual accounts are skipped: their
// history is the user's own writes, and snapshotting their current_balance
// daily would interleave app-written repeats of a figure the user owns.
//
// Re-running on the same day replaces that day's row per account rather than
// adding a second one (UNIQUE account_id, as_of), so a sync and the daily sweep
// landing on the same household compose to one point per account per day — the
// same idempotency rule UpsertNetWorthSnapshot carries.
//
// Called from the daily snapshot sweep (SnapshotAll) and from the post-sync
// path (plaid.Syncer.SyncItem), mirroring the two triggers Snapshot itself has.
func SnapshotAccountBalances(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID) (int, error) {
	rows, err := q.ListPlaidAccountBalancesForHousehold(ctx, householdID)
	if err != nil {
		return 0, fmt.Errorf("list plaid accounts: %w", err)
	}

	today := time.Now().UTC().Truncate(24 * time.Hour)
	count := 0
	for _, a := range rows {
		if !a.CurrentBalance.Valid {
			continue
		}
		if _, err := q.InsertAccountBalanceHistory(ctx, dbgen.InsertAccountBalanceHistoryParams{
			AccountID: a.ID,
			AsOf:      today,
			Balance:   a.CurrentBalance.Decimal,
			Reason:    "snapshot",
		}); err != nil {
			return count, fmt.Errorf("snapshot account balance %s: %w", a.ID, err)
		}
		count++
	}
	return count, nil
}
