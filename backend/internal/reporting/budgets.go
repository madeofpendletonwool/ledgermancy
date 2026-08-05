package reporting

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// BudgetProgress is one budget alongside what has been spent against it, with
// the envelope carryover already resolved.
//
// It lives here rather than in the HTTP handler because two surfaces now need
// it: the Budgets page and the stored digest. Two copies of the envelope
// arithmetic would eventually disagree, and "your budget page and your digest
// quote different remaining balances" is the exact class of bug this app's
// figures are supposed to be immune to.
type BudgetProgress struct {
	BudgetID   uuid.UUID
	CategoryID uuid.UUID
	Name       string
	Slug       string
	Color      *string

	Budgeted decimal.Decimal
	// Period is weekly|monthly|yearly; PeriodStart/PeriodEnd bound the window
	// the spend was measured over.
	Period      string
	PeriodStart time.Time
	PeriodEnd   time.Time

	Rollover bool
	// Carryover is the balance rolled in from prior months (negative when the
	// envelope was overspent); Available is this period's amount plus that
	// carryover. Both are zero / equal-to-Budgeted for a non-rollover budget.
	Carryover decimal.Decimal
	Available decimal.Decimal
	Spent     decimal.Decimal
	Remaining decimal.Decimal
}

// MonthsInclusive counts calendar months from start's month through target's
// month, inclusive (both are first-of-month dates). 1 when they are the same
// month, 0 or negative when target precedes start.
func MonthsInclusive(start, target time.Time) int {
	return (target.Year()-start.Year())*12 + int(target.Month()) - int(start.Month()) + 1
}

// BuildBudgetProgress resolves every household budget against the window
// [from, to], scoped to what userID can see. ref is "now", which is what a
// weekly or yearly budget measures its own current period from.
func BuildBudgetProgress(
	ctx context.Context,
	q *dbgen.Queries,
	householdID, userID uuid.UUID,
	from, to, ref time.Time,
) ([]BudgetProgress, error) {
	rows, err := q.GetBudgetProgress(ctx, dbgen.GetBudgetProgressParams{
		HouseholdID: householdID,
		UserID:      userID,
		WindowStart: from,
		WindowEnd:   to,
		Ref:         ref,
	})
	if err != nil {
		return nil, err
	}

	out := make([]BudgetProgress, 0, len(rows))
	for _, b := range rows {
		// Envelope math (decimal, from SQL-sourced figures):
		//   carryover (balance entering this month) = amount×(months−1) − prior spend
		//   available this month                    = amount + carryover
		// A non-rollover budget carries nothing and resets each period. Rollover
		// is monthly-only, so the month-based carryover matches the window.
		carryover := decimal.Zero
		available := b.Budgeted
		if b.Rollover {
			if months := MonthsInclusive(b.RolloverStart, from); months > 1 {
				carryover = b.Budgeted.Mul(decimal.NewFromInt(int64(months - 1))).Sub(b.PriorSpent)
			}
			available = b.Budgeted.Add(carryover)
		}
		out = append(out, BudgetProgress{
			BudgetID:    b.BudgetID,
			CategoryID:  b.CategoryID,
			Name:        b.CategoryName,
			Slug:        b.CategorySlug,
			Color:       b.CategoryColor,
			Budgeted:    b.Budgeted,
			Period:      b.Period,
			PeriodStart: b.PeriodStart,
			PeriodEnd:   b.PeriodEnd,
			Rollover:    b.Rollover,
			Carryover:   carryover,
			Available:   available,
			Spent:       b.Spent,
			Remaining:   available.Sub(b.Spent),
		})
	}
	return out, nil
}
