package reporting

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/goals"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/obligations"
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

	// The bill-aware view. These are ADDITIONAL fields, never a redefinition of
	// Amount: the Budgets page and the chat tool both consume Amount, and
	// quietly changing what it means is how two surfaces start disagreeing about
	// the same number.
	//
	// UpcomingObligations is every known bill still to fall due between today
	// and the end of this month — the figure a person actually wants when they
	// ask "can I spend this?" on the 8th with rent due on the 10th.
	UpcomingObligations decimal.Decimal
	// FixedCostsAfterBills is FixedCosts recomputed per category so no bill is
	// counted twice. See buildBillAware for the rule.
	FixedCostsAfterBills decimal.Decimal
	// AmountAfterBills is income − FixedCostsAfterBills − budgeted − goals.
	AmountAfterBills decimal.Decimal
	// ObligationCoverage is how many of the household's fixed categories have a
	// known obligation behind them. Zero means the bill-aware figure is just the
	// ordinary one, and the UI should say so rather than imply new information.
	ObligationCoverage int
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

	sts := SafeToSpend{
		ExpectedIncome:        expectedIncome.Round(2),
		FixedCosts:            fixedCosts.Round(2),
		BudgetedDiscretionary: budgets.DiscretionaryBudgeted.Round(2),
		GoalContributions:     goalContrib.Round(2),
		Amount:                amount.Round(2),
		IncomeMonths:          incomeMonths,
	}

	bills, err := buildBillAware(ctx, q, householdID, now, fixedCosts)
	if err != nil {
		return SafeToSpend{}, err
	}
	sts.UpcomingObligations = bills.upcoming.Round(2)
	sts.FixedCostsAfterBills = bills.fixed.Round(2)
	sts.ObligationCoverage = bills.coverage
	sts.AmountAfterBills = expectedIncome.
		Sub(bills.fixed).
		Sub(budgets.DiscretionaryBudgeted).
		Sub(goalContrib).
		Round(2)

	return sts, nil
}

// billAware is buildBillAware's result: the recomputed fixed component, the
// headline "still to pay this month" figure, and how much of the fixed side the
// obligations actually cover.
type billAware struct {
	fixed    decimal.Decimal
	upcoming decimal.Decimal
	coverage int
}

// buildBillAware recomputes the fixed-cost input from known obligations without
// counting a single bill twice.
//
// This is the whole risk of the bill calendar touching safe-to-spend. The
// existing fixed input is TRAILING TYPICAL fixed spend: a six-month average that
// already contains the rent, the insurance and every other recurring charge the
// household actually paid. Adding upcoming obligations on top of it would
// subtract those same bills a second time and make the figure absurdly
// pessimistic.
//
// The rule, mirroring the no-double-counting rule at the top of this file: for
// the current month a fixed cost counts EXACTLY ONCE, and which side it counts
// on is decided per category.
//
//   - A fixed category with obligation coverage counts as its remaining unpaid
//     obligations. Its trailing typical figure is dropped — the obligations are
//     the better, dated answer for the same money.
//   - A fixed category with no obligation coverage keeps its trailing typical
//     figure, because nothing better is known about it.
//   - An obligation in a DISCRETIONARY category is skipped entirely. That money
//     is already inside BudgetedDiscretionary; adding it here would double-count
//     it against the budget envelope instead of against the fixed average.
//   - An obligation with no category at all is counted in full. It cannot
//     displace a category's trailing figure because it names none, and by
//     construction the bills that need manual entry (an annual premium paid by
//     cheque) are the ones the transaction history never saw.
//
// trailingFixed is the whole-household figure BuildSafeToSpend already computed;
// it is used as the fallback when the per-category breakdown is unavailable, so
// this can never make the estimate worse than it was.
func buildBillAware(
	ctx context.Context,
	q *dbgen.Queries,
	householdID uuid.UUID,
	now time.Time,
	trailingFixed decimal.Decimal,
) (billAware, error) {
	shared := uuid.Nil
	mStart := firstOfMonth(now)
	today := firstOfDay(now)
	monthEnd := mStart.AddDate(0, 1, -1)

	// Obligations still to fall due this month, household-shared scope.
	upcoming := decimal.Zero
	occurrences, err := obligations.ListUpcoming(ctx, q, householdID, shared, today, monthEnd)
	if err != nil {
		return billAware{}, err
	}
	for _, o := range occurrences {
		upcoming = upcoming.Add(o.Amount)
	}
	if len(occurrences) == 0 {
		// Nothing known: the bill-aware figure is the ordinary one, exactly.
		return billAware{fixed: trailingFixed, upcoming: decimal.Zero}, nil
	}

	// Which categories are fixed. Read from the category definitions rather than
	// inferred from spend, so a fixed category with no history is still known to
	// be fixed.
	cats, err := q.ListCategories(ctx, &householdID)
	if err != nil {
		return billAware{}, err
	}
	isFixed := make(map[uuid.UUID]bool, len(cats))
	for _, c := range cats {
		isFixed[c.ID] = c.IsFixed
	}

	// Trailing typical spend per fixed category, over the same window and the
	// same definition BuildSafeToSpend used, so the two agree by construction:
	// GetSpendingByCategory's fixed rows sum to GetSpendingSummary's
	// fixed_spending over the same range.
	byCategory, err := q.GetSpendingByCategory(ctx, dbgen.GetSpendingByCategoryParams{
		HouseholdID: householdID, UserID: shared,
		Date: mStart.AddDate(0, -safeFixedMonths, 0), Date_2: mStart.AddDate(0, 0, -1),
	})
	if err != nil {
		return billAware{}, err
	}
	months := decimal.NewFromInt(safeFixedMonths)
	trailingByCategory := make(map[uuid.UUID]decimal.Decimal, len(byCategory))
	for _, row := range byCategory {
		if row.IsFixed {
			trailingByCategory[row.CategoryID] = row.Total.Div(months)
		}
	}

	covered := make(map[uuid.UUID]bool)
	oblFixed := decimal.Zero
	for _, o := range occurrences {
		if o.CategoryID == nil {
			oblFixed = oblFixed.Add(o.Amount)
			continue
		}
		if !isFixed[*o.CategoryID] {
			continue // already inside the discretionary budget envelope
		}
		oblFixed = oblFixed.Add(o.Amount)
		covered[*o.CategoryID] = true
	}

	fixed := oblFixed
	for catID, typical := range trailingByCategory {
		if !covered[catID] {
			fixed = fixed.Add(typical)
		}
	}

	return billAware{fixed: fixed, upcoming: upcoming, coverage: len(covered)}, nil
}

// firstOfDay is the UTC calendar date of t.
func firstOfDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// goalContributions sums the monthly amount each active household savings goal
// needs to stay on schedule (goals.Compute's RequiredMonthly). Open-ended goals
// (no target date) require nothing per month and contribute zero. The surplus
// argument only affects on-track/shortfall, not RequiredMonthly, so it is passed
// as zero.
//
// Debt-payoff goals are deliberately excluded, for two reasons. Their arithmetic
// is amortization, not accumulation — goals.Compute would read the debt's
// balance as progress and return a meaningless figure. And their monthly payment
// is already a bill in the cashflow this function is reserving against, so
// counting it again would take the same dollars out twice.
func goalContributions(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time) (decimal.Decimal, error) {
	rows, err := q.ListActiveHouseholdGoals(ctx, householdID)
	if err != nil {
		return decimal.Zero, err
	}
	total := decimal.Zero
	for _, g := range rows {
		if g.Kind != "savings" {
			continue
		}

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
