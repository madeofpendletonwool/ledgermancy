// Command backfill-transfer-pairs links the internal transfers that predate the
// structural pairer.
//
// The pairer runs after every Plaid sync, but only over a 60-day lookback —
// enough to catch a transfer whose two legs straddle a sync boundary, and
// nowhere near enough to reach a household's existing history. So on the day the
// feature ships every historical transfer is still unlinked, and the leg the
// merchant cache misfiled keeps its wrong label forever. categorize
// .PairAllHouseholds was written for exactly this and had no caller; this is it.
//
// It defaults to a dry run. Pass --apply to write.
//
// It must move zero money. Pairing changes a transaction's CATEGORY (to the
// household's transfer-out / transfer-in) and records the link; it never touches
// an amount or a date. Both categories are is_transfer, and so is the
// "Investments"-style user category a misfiled leg tends to be sitting in, which
// means the household's spending total does not move either — this makes the
// ledger say what it means, it does not recover money.
//
// A manually categorised leg is never overridden: manual rows are excluded from
// the candidate set, and ApplyCategory refuses them a second time.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/categorize"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

func main() {
	apply := flag.Bool("apply", false, "write the pairs; without this the command only reports what it would match")
	databaseURL := flag.String("database-url", os.Getenv("DATABASE_URL"), "connection string; defaults to $DATABASE_URL")
	days := flag.Int("days", categorize.BackfillLookback, "how far back to scan, in days")
	flag.Parse()

	if *databaseURL == "" {
		fmt.Fprintln(os.Stderr, "error: set DATABASE_URL or pass --database-url")
		os.Exit(1)
	}

	if err := run(context.Background(), *databaseURL, *days, *apply); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// run takes the connection string directly rather than going through
// config.Load, for the same reason normalise-merchant-keys does: a maintenance
// command that reads transactions and writes categories has no business
// demanding an encryption key and a valid backup configuration before it starts.
func run(ctx context.Context, databaseURL string, days int, apply bool) error {
	pool, err := db.Connect(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	q := dbgen.New(pool)
	now := time.Now().UTC()

	households, err := q.ListHouseholdIDs(ctx)
	if err != nil {
		return fmt.Errorf("list households: %w", err)
	}

	// The dry run reports through the SAME candidate query and the SAME matching
	// function the apply path uses — categorize.MatchPairs is pure, so this is
	// the real answer rather than a second implementation of the rule that could
	// disagree with it.
	total := 0
	for _, householdID := range households {
		rows, err := q.ListTransferPairCandidates(ctx, dbgen.ListTransferPairCandidatesParams{
			HouseholdID: householdID,
			Date:        now.AddDate(0, 0, -days),
		})
		if err != nil {
			return fmt.Errorf("list candidates for %s: %w", householdID, err)
		}
		candidates := make([]categorize.PairCandidate, 0, len(rows))
		for _, r := range rows {
			candidates = append(candidates, categorize.PairCandidate{
				ID: r.ID, AccountID: r.AccountID, Amount: r.Amount, Date: r.Date,
			})
		}
		pairs := categorize.MatchPairs(candidates)
		total += len(pairs)
		if len(pairs) == 0 {
			continue
		}

		names, err := accountNames(ctx, pool, householdID)
		if err != nil {
			return fmt.Errorf("read account names for %s: %w", householdID, err)
		}
		fmt.Printf("Household %s: %d pair(s) from %d candidate transaction(s).\n",
			householdID, len(pairs), len(rows))
		sort.Slice(pairs, func(i, j int) bool { return pairs[i].Out.Date.Before(pairs[j].Out.Date) })
		for _, p := range pairs {
			fmt.Printf("  %12s  %s %-24s -> %s %s\n",
				p.Amount.StringFixed(2),
				p.Out.Date.Format(time.DateOnly), truncate(names[p.Out.AccountID], 24),
				p.In.Date.Format(time.DateOnly), names[p.In.AccountID])
		}
	}

	if total == 0 {
		fmt.Println("No unpaired internal transfers found. Nothing to do.")
		return nil
	}

	if !apply {
		fmt.Printf("\nDry run — %d pair(s) would be linked and relabelled. Re-run with --apply to commit.\n", total)
		return nil
	}

	// Re-run through PairAllHouseholds rather than writing here: it is the
	// function the sync path calls, so the backfill and the ongoing pass cannot
	// drift. It re-reads the candidates, which is why the count it returns is
	// the one reported as applied.
	applied, err := categorize.PairAllHouseholds(ctx, q, now)
	if err != nil {
		return err
	}
	fmt.Printf("\nApplied. %d pair(s) linked.\n", applied)
	return nil
}

// accountNames is display only — the matching never reads a name, which is the
// entire point of the structural rule.
func accountNames(ctx context.Context, pool *pgxpool.Pool, householdID uuid.UUID) (map[uuid.UUID]string, error) {
	rows, err := pool.Query(ctx, `
		SELECT a.id, a.name
		FROM accounts a
		JOIN account_access v ON v.account_id = a.id
		WHERE v.household_id = $1`, householdID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[uuid.UUID]string{}
	for rows.Next() {
		var id uuid.UUID
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
