package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/plaid"
)

// PostScheduledTransactionsArgs materialises every due occurrence of every
// auto-posting obligation.
type PostScheduledTransactionsArgs struct{}

func (PostScheduledTransactionsArgs) Kind() string { return "post_scheduled_transactions" }

// maxBackfill bounds how far back a newly-enabled obligation reaches.
//
// An obligation's anchor_date is the first occurrence ever, which for a bill
// entered years ago is years ago. Without a floor, switching auto_post on for
// that row posts every occurrence since — hundreds of transactions, and for a
// manual investment account, a balance inflated by hundreds of contributions
// that never happened. Ninety days is long enough to cover a server that was
// off for a season and short enough that the mistake is obvious and reversible.
const maxBackfill = 90 * 24 * time.Hour

// PostScheduledTransactionsWorker turns due obligations into real rows.
//
// Doc 13 built the bill calendar and deliberately posted nothing: an obligation
// was a claim about the future, and materialising it would have competed with
// the Plaid sync for the same reality. That reasoning holds for a linked
// account and fails completely for a manual one, where nothing else will ever
// write the row. This worker is the opt-in, gated per obligation by auto_post.
//
// ATOMICITY IS THE WHOLE DESIGN, for the same reason it is in
// PostAllowancesWorker: a job queue redelivers, a worker restarts mid-run, and
// double-crediting a retirement account is the kind of error that is invisible
// until it has been compounding for a year. Three defences, in order of how
// much they are relied upon:
//
//   - All of one obligation's occurrences, and the cursor advance that accounts
//     for them, commit in ONE transaction. A failure anywhere leaves the
//     obligation exactly as it was.
//   - last_posted_date advances only inside that commit, so a crash replays
//     from the same cursor rather than skipping.
//   - A partial unique index on (obligation_id, date) makes a duplicate posting
//     impossible at the storage layer. The cursor alone is what Plaid's sync
//     relies on and what doc 30 recommends; it is not enough here, because a
//     replay that Plaid would resolve by re-reading the truth would here be a
//     second withdrawal nobody made.
type PostScheduledTransactionsWorker struct {
	river.WorkerDefaults[PostScheduledTransactionsArgs]
	Pool    *pgxpool.Pool
	Queries *dbgen.Queries
}

func (w *PostScheduledTransactionsWorker) Work(
	ctx context.Context, job *river.Job[PostScheduledTransactionsArgs],
) error {
	return PostDueScheduledTransactions(ctx, w.Pool, w.Queries, time.Now())
}

// PostDueScheduledTransactions is the worker's body, taking `now` so tests do
// not depend on the calendar.
func PostDueScheduledTransactions(
	ctx context.Context, pool *pgxpool.Pool, q *dbgen.Queries, now time.Time,
) error {
	today := now.UTC().Truncate(24 * time.Hour)

	rows, err := q.ListObligationsDueForPosting(ctx, dbgen.ListObligationsDueForPostingParams{
		Today:    today,
		Earliest: today.Add(-maxBackfill),
	})
	if err != nil {
		return fmt.Errorf("list obligations due for posting: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}

	// The query orders by obligation, so occurrences arrive already grouped and
	// a single pass can close each group without buffering the whole result.
	posted := 0
	start := 0
	for i := 1; i <= len(rows); i++ {
		if i < len(rows) && rows[i].ObligationID == rows[start].ObligationID {
			continue
		}
		n, err := postObligation(ctx, pool, q, rows[start:i], today)
		if err != nil {
			// One bad obligation must not stop the rest. Its cursor did not
			// advance, so the next run retries it.
			slog.Error("post scheduled obligation",
				"obligation", rows[start].ObligationID, "err", err)
		}
		posted += n
		start = i
	}

	if posted > 0 {
		slog.Info("scheduled transactions posted", "rows", posted)
	}
	return nil
}

// postObligation writes one obligation's due occurrences and advances its
// cursor, all or nothing.
func postObligation(
	ctx context.Context,
	pool *pgxpool.Pool,
	q *dbgen.Queries,
	occurrences []dbgen.ListObligationsDueForPostingRow,
	today time.Time,
) (int, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)
	qtx := q.WithTx(tx)

	written := 0
	for _, o := range occurrences {
		// merchant_key is what the categoriser and the merchant views key on.
		// Deriving it from the label the same way a manual transaction does
		// means a scheduled Voya contribution groups with any hand-entered one.
		key := plaid.MerchantKey(o.Label, o.Label)

		_, err := qtx.InsertScheduledTransaction(ctx, dbgen.InsertScheduledTransactionParams{
			AccountID:    o.TargetAccountID,
			Amount:       o.Amount,
			Date:         o.DueDate,
			Name:         o.Label,
			MerchantKey:  &key,
			CategoryID:   o.CategoryID,
			ObligationID: &o.ObligationID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			// ON CONFLICT DO NOTHING matched, so RETURNING produced nothing:
			// this occurrence was already posted by an earlier run whose cursor
			// did not advance. Skipping the investment side too is precisely
			// what keeps the replay harmless — re-running the contribution
			// would move the balance a second time.
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("insert transaction for %s: %w", o.DueDate.Format(time.DateOnly), err)
		}
		written++

		if !isManualInvestment(o) {
			continue
		}
		if err := postContribution(ctx, qtx, o); err != nil {
			return 0, err
		}
	}

	// The cursor accounts for every occurrence up to today, including ones
	// skipped as already-posted — they are, by definition, accounted for.
	if _, err := qtx.MarkObligationPosted(ctx, dbgen.MarkObligationPostedParams{
		ID:            occurrences[0].ObligationID,
		PostedThrough: &today,
	}); err != nil {
		return 0, fmt.Errorf("advance cursor: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return written, nil
}

// isManualInvestment reports whether a posting should also move holdings-side
// state. Only a manual investment account qualifies: a Plaid account's balance
// is the institution's to report, and writing it here would produce a figure
// the next sync silently overwrites.
func isManualInvestment(o dbgen.ListObligationsDueForPostingRow) bool {
	return o.TargetSource == "manual" &&
		(o.TargetType == "investment" || o.TargetType == "brokerage")
}

// postContribution records the investment-side effect of a scheduled posting:
// an external cash flow into the portfolio, and the balance movement it caused.
//
// SIGN CONVENTIONS, which are the easy thing to get wrong here because three
// columns use three different ones for the same event:
//
//   - transactions.amount is POSITIVE for money out (Plaid's convention, kept
//     so the two never disagree). A contribution leaving checking is positive.
//   - investment_transactions.amount is NEGATIVE for money into the portfolio,
//     which is what reporting.ExternalFlows expects (reporting/investments.go).
//     Getting this backwards does not error — it inverts every TWR and MWR
//     figure the account appears in.
//   - accounts.current_balance GROWS by the contribution.
func postContribution(
	ctx context.Context, qtx *dbgen.Queries, o dbgen.ListObligationsDueForPostingRow,
) error {
	subtype := "contribution"
	name := o.Label
	if _, err := qtx.CreateManualInvestmentTransactionUnscoped(
		ctx, dbgen.CreateManualInvestmentTransactionUnscopedParams{
			AccountID: o.TargetAccountID,
			Source:    "scheduled",
			Type:      "cash",
			Subtype:   &subtype,
			Amount:    o.Amount.Neg(),
			Date:      o.DueDate,
			Name:      &name,
		}); err != nil {
		return fmt.Errorf("insert investment transaction: %w", err)
	}

	balance, err := qtx.AddToManualAccountBalance(ctx, dbgen.AddToManualAccountBalanceParams{
		ID:    o.TargetAccountID,
		Delta: decimal.NullDecimal{Decimal: o.Amount, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("move balance: %w", err)
	}

	// The history row carries the balance the account now holds, read back from
	// the UPDATE rather than recomputed, so the trail and the headline figure
	// cannot disagree even if two contributions land on the same day.
	note := o.Label
	if _, err := qtx.InsertAccountBalanceHistory(ctx, dbgen.InsertAccountBalanceHistoryParams{
		AccountID: o.TargetAccountID,
		AsOf:      o.DueDate,
		Balance:   balanceOrZero(balance),
		Reason:    "scheduled",
		Note:      &note,
	}); err != nil {
		return fmt.Errorf("record balance history: %w", err)
	}
	return nil
}

func balanceOrZero(b decimal.NullDecimal) decimal.Decimal {
	if !b.Valid {
		return decimal.Zero
	}
	return b.Decimal
}
