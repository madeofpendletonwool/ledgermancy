package reporting

import (
	"context"
	"sort"
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
//
// Two properties protect the trailing inputs from a single unusual month, and
// both are load-bearing:
//
//   - MEDIAN, not mean. This figure's whole job is to describe a TYPICAL month,
//     and a mean does not. A $14,295 auto-loan payoff — correctly categorised,
//     correctly counted as spent — used to enter the six-month fixed average at
//     ~$2,383/month and stay there for six months, telling a household its
//     recurring bills had quadrupled at the exact moment they had FALLEN (the
//     monthly payment died with the loan). A median ignores it outright.
//     Symmetrically, a one-off bonus no longer inflates expected income.
//
//   - One-time rows excluded. Every query below passes ExcludeOneTime, so a
//     transaction the household has flagged as non-repeating cannot reach these
//     baselines at all. The Spending page still shows it: it happened. See the
//     header of reports.sql for why the two flags are separate.
//
// The two are independent on purpose. The median handles the outlier nobody
// flagged; the flag handles the outlier that is not extreme enough to be a
// statistical outlier but is still known not to repeat.
const (
	// safeWindowMonths is the trailing full-month window BOTH medians are taken
	// over. Income and fixed costs used to carry separate constants that were
	// always equal; one window is now load-bearing rather than coincidental,
	// because the per-category fixed medians are zero-filled against the set of
	// months the income query returned. Matches the projection producer's income
	// window so the two never disagree on "typical income".
	safeWindowMonths = 6
)

// medianOf returns the median of xs, or zero for an empty slice. Even counts
// average the two middle values.
//
// It sorts a COPY: callers pass slices derived from query results that other
// code still reads in date order, and reordering those under them would be a
// silent, month-shuffling bug.
func medianOf(xs []decimal.Decimal) decimal.Decimal {
	if len(xs) == 0 {
		return decimal.Zero
	}
	sorted := make([]decimal.Decimal, len(xs))
	copy(sorted, xs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].LessThan(sorted[j]) })

	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return sorted[mid-1].Add(sorted[mid]).Div(decimal.NewFromInt(2))
}

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
	//
	// It is REPORTED, NOT SUBTRACTED, and it is not the amount that went into
	// FixedCostsAfterBills: that swap uses the whole month and only its fixed
	// categories, while this counts every remaining occurrence including
	// discretionary ones. UI copy that presents this as the driver of the
	// bill-aware figure is wrong. See buildBillAware.
	UpcomingObligations decimal.Decimal
	// FixedCostsAfterBills is FixedCosts recomputed per category, from the whole
	// month's scheduled obligations, so no bill is counted twice. See
	// buildBillAware for the rule.
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

	windowStart := mStart.AddDate(0, -safeWindowMonths, 0)
	windowEnd := mStart.AddDate(0, 0, -1)

	trend, err := q.GetMonthlyTrend(ctx, dbgen.GetMonthlyTrendParams{
		HouseholdID: householdID, UserID: shared,
		Date: windowStart, Date_2: windowEnd,
		ExcludeOneTime: true,
	})
	if err != nil {
		return SafeToSpend{}, err
	}

	// Expected income: the MEDIAN of prior full months (lumpy paychecks make
	// this-month income unreliable, and a bonus month should not raise the
	// household's idea of normal). Months with no income at all are skipped
	// rather than counted as zero — a household that linked its accounts three
	// months ago has three months of history, not six months of poverty.
	incomeMonthly := make([]decimal.Decimal, 0, len(trend))
	for _, m := range trend {
		if m.Income.IsPositive() {
			incomeMonthly = append(incomeMonthly, m.Income)
		}
	}
	expectedIncome := medianOf(incomeMonthly)
	incomeMonths := len(incomeMonthly)

	// Fixed costs, per category, as the median month. Computed once and reused by
	// the bill-aware variant so the two can never disagree about what a category
	// typically costs.
	trailingByCategory, err := trailingFixedByCategory(ctx, q, householdID, windowStart, windowEnd, trend)
	if err != nil {
		return SafeToSpend{}, err
	}
	fixedCosts := decimal.Zero
	for _, typical := range trailingByCategory {
		fixedCosts = fixedCosts.Add(typical)
	}

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

	bills, err := buildBillAware(ctx, q, householdID, now, fixedCosts, trailingByCategory)
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

// trailingFixedByCategory returns, per fixed category, what that category costs
// in a MEDIAN month of the window. Summing the result gives the household's
// fixed-cost estimate; the map itself is what the bill-aware variant swaps
// against. One definition, two consumers, so they cannot drift.
//
// A month in which a category did not appear counts as a ZERO for that category,
// not as a missing observation. The distinction matters: a household that paid
// rent five times in a six-month window really does have a month with no rent in
// it, and dropping that month would report the cost of rent-paying months rather
// than the cost of an average month. The months are taken from `trend`, i.e.
// months in which the household had ANY activity — a window that extends before
// the household linked its accounts contributes no phantom zeros.
//
// Consequence worth knowing: a bill that lands 4 times in 6 months (quarterly,
// or an annual premium) medians to zero. That is the correct answer for "what
// does a typical month cost" and the wrong one for "what will I pay this month",
// which is precisely the question the bill-aware variant answers instead.
func trailingFixedByCategory(
	ctx context.Context,
	q *dbgen.Queries,
	householdID uuid.UUID,
	windowStart, windowEnd time.Time,
	trend []dbgen.GetMonthlyTrendRow,
) (map[uuid.UUID]decimal.Decimal, error) {
	rows, err := q.GetMonthlyFixedSpendByCategory(ctx, dbgen.GetMonthlyFixedSpendByCategoryParams{
		HouseholdID: householdID, UserID: uuid.Nil,
		Date: windowStart, Date_2: windowEnd,
		ExcludeOneTime: true,
	})
	if err != nil {
		return nil, err
	}

	// Index the observed spend so absent (category, month) pairs can be filled
	// with zero below.
	type key struct {
		category uuid.UUID
		month    time.Time
	}
	observed := make(map[key]decimal.Decimal, len(rows))
	categories := make([]uuid.UUID, 0, 8)
	for _, r := range rows {
		if _, seen := observed[key{r.CategoryID, r.Month}]; !seen {
			if _, known := indexOf(categories, r.CategoryID); !known {
				categories = append(categories, r.CategoryID)
			}
		}
		observed[key{r.CategoryID, r.Month}] = r.Total
	}

	out := make(map[uuid.UUID]decimal.Decimal, len(categories))
	for _, catID := range categories {
		monthly := make([]decimal.Decimal, 0, len(trend))
		for _, m := range trend {
			monthly = append(monthly, observed[key{catID, m.Month}]) // zero value when absent
		}
		if typical := medianOf(monthly); typical.IsPositive() {
			out[catID] = typical
		}
	}
	return out, nil
}

// indexOf reports whether id is already in ids. Linear because a household has a
// handful of fixed categories, not thousands.
func indexOf(ids []uuid.UUID, id uuid.UUID) (int, bool) {
	for i, existing := range ids {
		if existing == id {
			return i, true
		}
	}
	return 0, false
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
//   - A fixed category with obligation coverage counts as its scheduled
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
// THE TWO WINDOWS ARE DIFFERENT, AND THAT IS THE FIX RATHER THAN THE BUG:
//
//   - The fixed-cost swap uses the WHOLE month, [mStart, monthEnd].
//   - `upcoming` — the "still to pay" line the UI prints — uses the REMAINDER of
//     the month, [today, monthEnd].
//
// The swap replaces a full-month trailing typical figure, so its replacement
// must also be full-month; and it is subtracted from a full month of expected
// income. Feeding it only the days left made the estimate drift upward as the
// month aged: on the 30th, a household whose childcare typically runs $1,496 a
// month had that swapped for the single $345.25 occurrence still to fall, and
// safe-to-spend read $1,151 richer than the headline for no reason but the date.
// `upcoming` keeps the remaining-days window because "what is still to pay" is
// exactly what it means. It is reported, never subtracted.
//
// trailingFixed is the whole-household figure BuildSafeToSpend already computed;
// it is the answer when nothing is known about the schedule, so this can never
// make the estimate worse than it was. trailingByCategory is that same figure
// decomposed — it sums to trailingFixed by construction (see
// trailingFixedByCategory), which is what keeps the bill-aware number from
// jumping the moment the first obligation is detected.
func buildBillAware(
	ctx context.Context,
	q *dbgen.Queries,
	householdID uuid.UUID,
	now time.Time,
	trailingFixed decimal.Decimal,
	trailingByCategory map[uuid.UUID]decimal.Decimal,
) (billAware, error) {
	shared := uuid.Nil
	mStart := firstOfMonth(now)
	today := firstOfDay(now)
	monthEnd := mStart.AddDate(0, 1, -1)

	// Everything scheduled this month, whether already paid or not. This is what
	// displaces the trailing figures.
	occurrences, err := obligations.ListUpcoming(ctx, q, householdID, shared, mStart, monthEnd)
	if err != nil {
		return billAware{}, err
	}

	// Separately: what is still to fall due, for display only.
	remaining, err := obligations.ListUpcoming(ctx, q, householdID, shared, today, monthEnd)
	if err != nil {
		return billAware{}, err
	}
	upcoming := decimal.Zero
	for _, o := range remaining {
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
		// Accumulating kinds only. A debt-payoff goal's payment is already in
		// fixed costs, so counting it here would subtract it twice — but a
		// COLLEGE goal's contribution is real money leaving every month that
		// nothing else in this calculation sees, and omitting it would overstate
		// safe-to-spend. Which is the error direction that actually hurts.
		if g.Kind != "savings" && g.Kind != "college" {
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

// GoalProgress is how far along one goal is: an account-backed goal reads the
// account balance; a category/surplus goal counts the household surplus
// accumulated since the goal was created. Household-shared visibility
// throughout.
//
// Exported because it had already been copied once (the insight engine's
// goalProgress) and doc 24's advisor needed it a third time. Progress is DERIVED
// and never stored (the 00012 header rule), so every surface that shows a goal
// re-derives it — and three implementations of "how far along is this goal" is
// three chances for the Goals page, the feed and the advisor to disagree about
// the same goal.
func GoalProgress(ctx context.Context, q *dbgen.Queries, g dbgen.Goal, now time.Time) (decimal.Decimal, error) {
	return goalCurrentProgress(ctx, q, g, now)
}

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
