package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/riverqueue/river"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// PostAllowancesArgs credits every auto-posting allowance for the current
// period.
type PostAllowancesArgs struct{}

func (PostAllowancesArgs) Kind() string { return "post_allowances" }

// PostAllowancesWorker pays scheduled allowances.
//
// IDEMPOTENCY IS THE WHOLE DESIGN. A job queue redelivers, a worker restarts
// mid-run, two workers can pick up the same tick — and paying a child twice is
// the kind of bug that gets noticed at the dinner table. So the period boundary
// is written to allowances.last_posted_for and every step re-checks it:
//
//   - ListAutoPostAllowances only returns rows whose last_posted_for is older
//     than the current period.
//   - MarkAllowancePosted repeats that predicate in its WHERE clause and
//     reports rows affected, so two workers racing on one period produce
//     exactly one winner.
//   - The credit is only inserted after the mark succeeds. A crash between the
//     two loses a payment rather than duplicating one, which is the direction
//     to fail in: a missing credit is visible in the ledger and a parent can
//     add it, whereas a duplicate is silent.
type PostAllowancesWorker struct {
	river.WorkerDefaults[PostAllowancesArgs]
	Queries *dbgen.Queries
}

func (w *PostAllowancesWorker) Work(ctx context.Context, job *river.Job[PostAllowancesArgs]) error {
	return PostDueAllowances(ctx, w.Queries, time.Now())
}

// PostDueAllowances is the worker's body, taking `now` so tests do not depend
// on the calendar.
func PostDueAllowances(ctx context.Context, q *dbgen.Queries, now time.Time) error {
	posted := 0

	// One pass per cadence: the period boundary differs, and a weekly allowance
	// due today says nothing about whether a monthly one is.
	for _, cadence := range []string{"weekly", "biweekly", "monthly"} {
		periodStart := PeriodStart(cadence, now)

		rows, err := q.ListAutoPostAllowances(ctx, periodStart)
		if err != nil {
			return fmt.Errorf("list auto-post allowances: %w", err)
		}

		for _, a := range rows {
			if a.Cadence == nil || *a.Cadence != cadence || !a.Amount.Valid {
				continue
			}

			// Claim the period first. A zero row count means somebody else
			// already paid it.
			claimed, err := q.MarkAllowancePosted(ctx, dbgen.MarkAllowancePostedParams{
				PersonID: a.PersonID, PeriodStart: periodStart,
			})
			if err != nil {
				return fmt.Errorf("mark allowance posted: %w", err)
			}
			if claimed == 0 {
				continue
			}

			if _, err := q.CreateAllowanceEntry(ctx, dbgen.CreateAllowanceEntryParams{
				PersonID:    a.PersonID,
				HouseholdID: a.HouseholdID,
				Kind:        "allowance",
				Amount:      a.Amount.Decimal,
				OccurredOn:  periodStart,
				// CreatedBy is nil: the schedule posted this, not a person.
				CreatedBy: nil,
			}); err != nil {
				return fmt.Errorf("create allowance entry: %w", err)
			}
			posted++
		}
	}

	if posted > 0 {
		slog.Info("allowances posted", "count", posted)
	}
	return nil
}

// PeriodStart is the first day of the period containing `now` for a cadence.
//
// Weekly and biweekly anchor on Monday. Biweekly counts whole weeks since the
// Unix epoch and keeps even-numbered ones, which is arbitrary but STABLE — the
// property that matters is that the same date always maps to the same period
// boundary, because that boundary is the idempotency key.
func PeriodStart(cadence string, now time.Time) time.Time {
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	switch cadence {
	case "monthly":
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	case "biweekly":
		monday := mondayOf(day)
		weeks := int(monday.Sub(epochMonday).Hours() / (24 * 7))
		if weeks%2 != 0 {
			monday = monday.AddDate(0, 0, -7)
		}
		return monday
	default: // weekly
		return mondayOf(day)
	}
}

// epochMonday is the Monday on or before the Unix epoch, the fixed reference
// biweekly periods are counted from.
var epochMonday = time.Date(1969, 12, 29, 0, 0, 0, 0, time.UTC)

func mondayOf(day time.Time) time.Time {
	// Go's Weekday has Sunday as 0; shift so Monday is 0.
	offset := (int(day.Weekday()) + 6) % 7
	return day.AddDate(0, 0, -offset)
}
