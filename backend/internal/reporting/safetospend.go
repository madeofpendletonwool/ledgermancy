package reporting

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
	"github.com/apex42group/ledgermancy/backend/internal/goals"
)

// Safe-to-spend answers "after everything that's already spoken for, how much is
// left to spend freely this month?" It is a household planning figure, computed
// entirely in SQL/decimal from four inputs that already exist elsewhere:
//
//	safe = expected income − fixed costs − discretionary budgets − goal contributions
//
// Each input is scoped household-shared (uuid.Nil visibility), the same view the
// insight feed and goals use, so a member's private items never leak in and the
// numbers agree across surfaces.
//
// No double-counting: a FIXED category counts once, at its trailing typical cost
// (never its budget); a DISCRETIONARY category counts once, at its budgeted
// envelope. A discretionary category with no budget is simply not subtracted —
// the money for it is exactly what "safe to spend" is meant to cover.
const (
	// safeIncomeMonths / safeFixedMonths are the trailing full-month windows the
	// income and fixed-cost averages are taken over. Matches the projection
	// producer's income window so the two never disagree on "typical income".
	safeIncomeMonths = 6
	safeFixedMonths  = 6
)

// SafeToSpend is the computed figure and its component parts, so the UI can show
// the full breakdown rather than a bare number.
type SafeToSpend struct {
	ExpectedIncome        decimal.Decimal
	FixedCosts            decimal.Decimal
	BudgetedDiscretionary decimal.Decimal
	GoalContributions     decimal.Decimal
	Amount                decimal.Decimal // income − fixed − budgeted − goals
	// IncomeMonths is how many months the income average is based on, so the UI
	// can caveat a thin history ("based on 2 months").
	IncomeMonths int
}

// BuildSafeToSpend assembles the figure for one household as of `now`.
func BuildSafeToSpend(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time) (SafeToSpend, error) {
	shared := uuid.Nil
	mStart := firstOfMonth(now)

	// Expected income: average of prior full months' income (lumpy paychecks make
	// this-month income unreliable). Income is already SQL-computed per month.
	trend, err := q.GetMonthlyTrend(ctx, dbgen.GetMonthlyTrendParams{
		HouseholdID: householdID, UserID: shared,
		Date: mStart.AddDate(0, -safeIncomeMonths, 0), Date_2: mStart.AddDate(0, 0, -1),
	})
	if err != nil {
		return SafeToSpend{}, err
	}
	incomeSum := decimal.Zero
	incomeMonths := 0
	for _, m := range trend {
		if m.Income.IsPositive() {
			incomeSum = incomeSum.Add(m.Income)
			incomeMonths++
		}
	}
	expectedIncome := decimal.Zero
	if incomeMonths > 0 {
		expectedIncome = incomeSum.Div(decimal.NewFromInt(int64(incomeMonths)))
	}

	// Fixed costs: trailing fixed spend over the window, divided by the window
	// length for a clean per-month figure. GetSpendingSummary already isolates
	// fixed spend (is_fixed categories), so this needs no per-category work — and
	// dividing by the exact month count avoids GetCategoryAverages' age()-based
	// divisor, which reads a 6-calendar-month range as ~5 months and would inflate
	// the estimate.
	fixedWindow, err := q.GetSpendingSummary(ctx, dbgen.GetSpendingSummaryParams{
		HouseholdID: householdID, UserID: shared,
		Date: mStart.AddDate(0, -safeFixedMonths, 0), Date_2: mStart.AddDate(0, 0, -1),
	})
	if err != nil {
		return SafeToSpend{}, err
	}
	fixedCosts := fixedWindow.FixedSpending.Div(decimal.NewFromInt(safeFixedMonths))

	// Discretionary budgets: the envelopes the household has set on non-fixed
	// categories (fixed budgets are excluded to avoid double-counting the fixed
	// costs above).
	budgets, err := q.SumHouseholdBudgets(ctx, householdID)
	if err != nil {
		return SafeToSpend{}, err
	}

	goalContrib, err := goalContributions(ctx, q, householdID, now)
	if err != nil {
		return SafeToSpend{}, err
	}

	amount := expectedIncome.
		Sub(fixedCosts).
		Sub(budgets.DiscretionaryBudgeted).
		Sub(goalContrib)

	return SafeToSpend{
		ExpectedIncome:        expectedIncome.Round(2),
		FixedCosts:            fixedCosts.Round(2),
		BudgetedDiscretionary: budgets.DiscretionaryBudgeted.Round(2),
		GoalContributions:     goalContrib.Round(2),
		Amount:                amount.Round(2),
		IncomeMonths:          incomeMonths,
	}, nil
}

// goalContributions sums the monthly amount each active household goal needs to
// stay on schedule (goals.Compute's RequiredMonthly). Open-ended goals (no target
// date) require nothing per month and contribute zero. The surplus argument only
// affects on-track/shortfall, not RequiredMonthly, so it is passed as zero.
func goalContributions(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time) (decimal.Decimal, error) {
	rows, err := q.ListActiveHouseholdGoals(ctx, householdID)
	if err != nil {
		return decimal.Zero, err
	}
	total := decimal.Zero
	for _, g := range rows {
		current, err := goalCurrentProgress(ctx, q, g, now)
		if err != nil {
			return decimal.Zero, err
		}
		f := goals.Compute(g.TargetAmount, current, decimal.Zero, g.TargetDate, now)
		total = total.Add(f.RequiredMonthly)
	}
	return total, nil
}

// goalCurrentProgress mirrors the insight engine's goalProgress: an
// account-backed goal reads the account balance; a category/surplus goal counts
// the household surplus accumulated since the goal was created. Household-shared
// visibility throughout.
func goalCurrentProgress(ctx context.Context, q *dbgen.Queries, g dbgen.Goal, now time.Time) (decimal.Decimal, error) {
	if g.AccountID != nil {
		return q.GetGoalAccountBalance(ctx, dbgen.GetGoalAccountBalanceParams{
			ID: *g.AccountID, HouseholdID: g.HouseholdID,
		})
	}
	sum, err := q.GetSpendingSummary(ctx, dbgen.GetSpendingSummaryParams{
		HouseholdID: g.HouseholdID, UserID: uuid.Nil, Date: g.CreatedAt, Date_2: now,
	})
	if err != nil {
		return decimal.Zero, err
	}
	surplus := sum.Income.Sub(sum.Spending)
	if surplus.IsNegative() {
		surplus = decimal.Zero
	}
	return surplus, nil
}

// firstOfMonth is the UTC first-of-month for t.
func firstOfMonth(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}
