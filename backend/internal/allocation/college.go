package allocation

import (
	"fmt"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/networth"
)

// College is FOUR YEARS OF SPENDING, not a bill on the first day.
//
// The obvious model — inflate the target to the enrollment year and compare it
// to the 529 balance at that instant — misstates funded-% in BOTH directions at
// once. It ignores that years two through four cost more (they inflate for
// another one, two and three years), and it ignores that the balance keeps
// compounding while those years are being paid. The two errors do not cancel;
// they just make the number unexplainable.
//
// So it is modelled as a drawdown, which is barely more code and is actually
// right:
//
//  1. Resolve the enrollment year from the beneficiary's birthdate (or a set
//     target age). Never from the clock — see networth.ResolveAge.
//  2. Inflate the today's-dollars annual cost SEPARATELY for each of the years,
//     at the amount by which college_inflation_rate outruns general inflation —
//     see realCollegeRate, which is the step that is easy to get wrong.
//  3. Walk the bucket forward, drawing each year's inflated cost at the START of
//     that year and compounding the remainder through the year.
//  4. Funded % is the share of the TOTAL inflated cost the bucket covers before
//     running dry; the shortfall is the uncovered remainder in the year it first
//     appears.
//
// "Funded through sophomore year, $18,400 short in junior year" is a sentence a
// parent can act on. A single-point comparison cannot produce it.

// CollegeYear is one year of study.
type CollegeYear struct {
	// Year is 1-based: 1 is freshman year.
	Year int `json:"year"`
	// Cost is that year's cost in the dollars of that year — the today's-dollars
	// annual figure inflated for (yearsToEnrollment + Year − 1) years.
	Cost decimal.Decimal `json:"cost"`
	// Covered is what the bucket could actually pay; Shortfall is the rest.
	Covered   decimal.Decimal `json:"covered"`
	Shortfall decimal.Decimal `json:"shortfall"`
	// BalanceAfter is what is left in the bucket after this year is paid and the
	// remainder has compounded through the year.
	BalanceAfter decimal.Decimal `json:"balance_after"`
}

// CollegeResult is one college goal's standing under the plan.
type CollegeResult struct {
	GoalID uuid.UUID `json:"goal_id"`
	Name   string    `json:"name"`
	// Projectable is false when the beneficiary's horizon could not be resolved
	// or no account is linked. The goal is then REPORTED as unprojectable rather
	// than projected against a guessed enrollment year.
	Projectable bool   `json:"projectable"`
	Note        string `json:"note,omitempty"`

	AccountID   *uuid.UUID `json:"account_id,omitempty"`
	AccountName string     `json:"account_name,omitempty"`
	// BeneficiaryAge is nil when the horizon could not be resolved. It is a
	// pointer for the same reason MonthlyNeeded is: a newborn beneficiary and an
	// unknown one are opposite answers, and both would render as 0.
	BeneficiaryAge    *int `json:"beneficiary_age"`
	YearsToEnrollment int  `json:"years_to_enrollment"`
	Years             int  `json:"years"`

	// AnnualCostToday is the goal's target_amount: ONE year in today's dollars.
	AnnualCostToday decimal.Decimal `json:"annual_cost_today"`
	// InflationRate is the household's college-cost rate as a PERCENT, exactly as
	// it is stored. RealExcessRate is what the drawdown actually compounds the
	// cost at: the amount by which college outruns general inflation, as a
	// FRACTION. Both are reported because the second is the surprising one.
	InflationRate  decimal.Decimal `json:"inflation_rate"`
	RealExcessRate decimal.Decimal `json:"real_excess_rate"`

	// BalanceAtEnrollment is the bucket's projected value on day one of year 1.
	BalanceAtEnrollment decimal.Decimal `json:"balance_at_enrollment"`
	TotalCost           decimal.Decimal `json:"total_cost"`
	TotalCovered        decimal.Decimal `json:"total_covered"`
	TotalShortfall      decimal.Decimal `json:"total_shortfall"`
	// FundedPct is TotalCovered ÷ TotalCost as a percent.
	FundedPct decimal.Decimal `json:"funded_pct"`
	// FirstShortfallYear is the 1-based year the money first runs out. Zero when
	// it never does.
	FirstShortfallYear int `json:"first_shortfall_year"`
	// MonthlyNeeded is the extra monthly contribution, on top of everything the
	// plan already does, that would fund all the years. Nil when the goal is
	// already fully funded or when no amount inside the search bound gets there.
	MonthlyNeeded *decimal.Decimal `json:"monthly_needed,omitempty"`

	Years_ []CollegeYear `json:"years_detail"`
	Basis  string        `json:"basis"`
}

// projectCollege runs the drawdown for every college goal.
//
// The balance it starts from is the bucket's projected value at the ENROLLMENT
// year, taken from the same ProjectRetirement run every other figure comes from
// — not recomputed here. A 529 is custodial, so the engine already freezes it at
// the beneficiary's horizon; this reads that frozen value and spends it.
func projectCollege(
	b Baseline, results []BucketResult, projection networth.RetirementProjection,
) []CollegeResult {
	out := make([]CollegeResult, 0, len(b.CollegeGoals))
	names := map[uuid.UUID]string{}
	for _, r := range results {
		names[r.AccountID] = r.Name
	}

	for _, g := range b.CollegeGoals {
		res := CollegeResult{
			GoalID: g.ID, Name: g.Name,
			Years:           g.Years,
			AnnualCostToday: g.AnnualCost.Round(2),
			InflationRate:   b.CollegeInflation,
			RealExcessRate:  realCollegeRate(b),
			Basis: "Every figure is in TODAY'S dollars, like the rest of the app. Each year of study is inflated " +
				"separately at the amount by which college costs outrun general inflation, drawn at the start of " +
				"that year, and the remainder keeps compounding at the bucket's real assumed return. College costs " +
				"have run well above general CPI, which is why that rate is its own setting.",
			Years_: []CollegeYear{},
		}

		// The horizon is a fact about the BENEFICIARY, so it is reported as soon
		// as it resolves — before the account and projection checks below, which
		// are facts about the money. A goal with no 529 linked yet still knows
		// it is seventeen years out, and saying "17 years, nothing linked" is
		// both more useful and more honest than the 0 it used to report.
		if g.HorizonKnown {
			age := g.BeneficiaryAge
			res.BeneficiaryAge = &age
			res.YearsToEnrollment = g.YearsToEnrollment
		}

		if g.AccountID == nil {
			res.Note = "No account is linked to this goal, so there is no balance to project. " +
				"Link the 529 on the Goals page."
			out = append(out, res)
			continue
		}
		res.AccountID = g.AccountID
		res.AccountName = names[*g.AccountID]

		if !g.HorizonKnown {
			res.Note = "The beneficiary's age is not on file, so the enrollment year cannot be resolved. " +
				"Set a birthdate for the person, or a beneficiary age on the account's contribution plan."
			out = append(out, res)
			continue
		}

		balance, ok := balanceAtYear(projection, g.AccountID.String(), g.YearsToEnrollment)
		if !ok {
			res.Note = "The linked account is not in the projection — it may have no confirmed tax treatment."
			out = append(out, res)
			continue
		}
		res.BalanceAtEnrollment = balance.Round(2)

		rate := returnRateFor(b, results, *g.AccountID)
		draw := drawdown(balance, g, realCollegeRate(b), rate)

		res.Projectable = true
		res.Years_ = draw.years
		res.TotalCost = draw.totalCost.Round(2)
		res.TotalCovered = draw.totalCovered.Round(2)
		res.TotalShortfall = draw.totalShortfall.Round(2)
		res.FirstShortfallYear = draw.firstShortfallYear
		if draw.totalCost.IsPositive() {
			res.FundedPct = draw.totalCovered.Div(draw.totalCost).Mul(hundred).Round(1)
		}
		if draw.totalShortfall.IsPositive() {
			if needed, found := solveCollegeMonthly(b, projection, g, rate); found {
				res.MonthlyNeeded = &needed
			}
		}
		out = append(out, res)
	}
	return out
}

// drawdownResult is the per-goal outcome of walking the four years.
type drawdownResult struct {
	years              []CollegeYear
	totalCost          decimal.Decimal
	totalCovered       decimal.Decimal
	totalShortfall     decimal.Decimal
	firstShortfallYear int
}

// realCollegeRate is the rate the drawdown actually uses: the EXCESS of college
// inflation over the household's general inflation assumption, as a fraction.
//
// This is the one piece of arithmetic here that is easy to get wrong in the
// flattering-looking direction and be badly wrong in the other. The bucket's
// balance is projected at a REAL return, so it is already in today's dollars.
// Inflating the cost at the full 5.5% against a balance that has already had
// 3% inflation stripped out of it would count general inflation twice and
// overstate the shortfall by several years of compounding. What the cost needs
// on top of today's dollars is only the amount by which college outruns
// everything else:
//
//	(1 + college) / (1 + general) − 1
//
// A household that sets general inflation ABOVE the college rate gets a
// negative excess and costs that shrink in real terms. That is the honest
// consequence of its own assumptions, not a bug to clamp away.
func realCollegeRate(b Baseline) decimal.Decimal {
	one := decimal.NewFromInt(1)
	college := one.Add(b.CollegeInflation.Div(hundred))
	general := one.Add(b.InflationRate)
	if !general.IsPositive() {
		return b.CollegeInflation.Div(hundred)
	}
	return college.Div(general).Sub(one)
}

// drawdown walks the years. realInflation and realReturn are both FRACTIONS and
// both REAL, so every figure it produces is in today's dollars.
func drawdown(
	balance decimal.Decimal, g CollegeGoal, realInflation, realReturn decimal.Decimal,
) drawdownResult {
	out := drawdownResult{years: []CollegeYear{}}
	if g.Years <= 0 {
		return out
	}

	growth := decimal.NewFromInt(1).Add(realReturn)
	inflate := decimal.NewFromInt(1).Add(realInflation)

	for y := 1; y <= g.Years; y++ {
		// Year y is paid (yearsToEnrollment + y − 1) years from today, so it has
		// inflated for that many years. This is the whole reason a single-point
		// comparison is wrong: year four has inflated for three more years than
		// year one, and the four-year total is strictly greater than four times
		// the enrollment-year cost.
		exponent := int32(g.YearsToEnrollment + y - 1)
		cost := g.AnnualCost.Mul(inflate.Pow(decimal.NewFromInt32(exponent))).Round(2)

		covered, shortfall := cost, decimal.Zero
		if balance.LessThan(cost) {
			covered = balance
			if covered.IsNegative() {
				covered = decimal.Zero
			}
			shortfall = cost.Sub(covered)
			if out.firstShortfallYear == 0 {
				out.firstShortfallYear = y
			}
		}
		balance = balance.Sub(covered)
		if balance.IsNegative() {
			balance = decimal.Zero
		}
		// The remainder compounds through the year it is sitting out.
		balance = balance.Mul(growth)

		out.totalCost = out.totalCost.Add(cost)
		out.totalCovered = out.totalCovered.Add(covered)
		out.totalShortfall = out.totalShortfall.Add(shortfall)
		out.years = append(out.years, CollegeYear{
			Year: y, Cost: cost, Covered: covered.Round(2),
			Shortfall: shortfall.Round(2), BalanceAfter: balance.Round(2),
		})
	}
	return out
}

// balanceAtYear reads the projected balance for one account N years out.
//
// It reads the SERIES rather than recomputing: the series has to exist anyway,
// and a second compounding loop here would be a second answer to the same
// question. A horizon beyond the projected months clamps to the last point, and
// the caller's own horizon note covers it.
func balanceAtYear(p networth.RetirementProjection, accountID string, years int) (decimal.Decimal, bool) {
	if len(p.Points) == 0 {
		return decimal.Zero, false
	}
	idx := years * 12
	if idx < 1 {
		idx = 1
	}
	if idx > len(p.Points) {
		idx = len(p.Points)
	}
	pt, ok := p.Points[idx-1].ByAccount[accountID]
	if !ok {
		return decimal.Zero, false
	}
	return pt.Balance, true
}

// returnRateFor is the real return this account is compounding at: a split
// override first (an allocation_plan that named this account with its own rate
// — the most explicit statement), then the account's OWN assumed rate from its
// contribution plan, then the household rate.
//
// The middle path is the one the standing college_projection takes: it runs
// with no split, so results is empty and without this lookup a 529 would
// compound at the household rate regardless of what the household set on the
// account. The account's own rate wins over the household rate for the same
// reason it exists: a 529 and a brokerage are not the household's conservative
// default. A VALID account rate is honoured as-is, including a genuine 0% (a
// cash-like holding modelled flat); only an unset (Invalid) rate falls through.
func returnRateFor(b Baseline, results []BucketResult, accountID uuid.UUID) decimal.Decimal {
	for _, r := range results {
		if r.AccountID == accountID && r.Kind == BucketInvestment {
			return r.ReturnRate
		}
	}
	for _, p := range b.Plans {
		if p.ID == accountID.String() && p.RealReturnRate.Valid {
			return p.RealReturnRate.Decimal
		}
	}
	return b.Assumptions.RealReturnRate
}

// collegeSolveIterations and collegeSolveMax bound the bisection for "what would
// it take". The bound is the point: without one, an unreachable target either
// runs away or converges on an absurdity, and "save $40,000 a month" is a bug
// with a decimal point rather than an answer.
const collegeSolveIterations = 40

var collegeSolveMax = decimal.NewFromInt(100000)

// solveCollegeMonthly finds the extra monthly contribution that funds every year.
//
// It bisects over the SAME drawdown the result reports, for the same reason
// goals.requiredPayment bisects over its own simulation: a closed form would be
// a second, separately-wrong model of the thing, and the answer this way can be
// fed straight back through the drawdown to check it.
//
// The extra contribution compounds from today to enrollment at the bucket's real
// return, month by month — the same "growth before the deposit" rule the
// projection uses, so the two agree.
func solveCollegeMonthly(
	b Baseline, projection networth.RetirementProjection, g CollegeGoal, rate decimal.Decimal,
) (decimal.Decimal, bool) {
	base, ok := balanceAtYear(projection, g.AccountID.String(), g.YearsToEnrollment)
	if !ok {
		return decimal.Zero, false
	}
	months := g.YearsToEnrollment * 12
	if months <= 0 {
		// Already enrolled: no time left to contribute, so no monthly figure
		// funds it. Saying so beats printing a number that cannot help.
		return decimal.Zero, false
	}
	monthlyRate := decimal.Zero
	if rate.IsPositive() {
		monthlyRate = rate.Div(twelve)
	}

	funds := func(extra decimal.Decimal) bool {
		accrued := decimal.Zero
		for m := 0; m < months; m++ {
			accrued = accrued.Add(accrued.Mul(monthlyRate)).Add(extra)
		}
		return !drawdown(base.Add(accrued), g, realCollegeRate(b), rate).totalShortfall.IsPositive()
	}

	if funds(decimal.Zero) {
		return decimal.Zero, false // nothing extra needed
	}
	if !funds(collegeSolveMax) {
		return decimal.Zero, false // not reachable inside the bound
	}

	lo, hi := decimal.Zero, collegeSolveMax
	two := decimal.NewFromInt(2)
	for i := 0; i < collegeSolveIterations; i++ {
		mid := lo.Add(hi).Div(two)
		if funds(mid) {
			hi = mid
		} else {
			lo = mid
		}
	}
	// hi is the smallest tested amount that funds it, so it is the side to
	// report: rounding down to lo would name a figure that does not.
	//
	// And it is rounded UP to the whole cent, for the same reason
	// goals.requiredPayment does it — A CENT SHORT IS A PLAN THAT DOES NOT WORK.
	// Round(2) rounds to nearest, so a solved $734.5849 became $734.58, and
	// four-tenths of a cent a month compounded over ten years left the goal
	// eight cents underfunded at the very figure the app had just named as
	// sufficient. Nobody would ever find that by hand; the round-trip test does.
	return ceilCents(hi), true
}

// ceilCents rounds up to the next whole cent.
func ceilCents(d decimal.Decimal) decimal.Decimal {
	return d.Shift(2).Ceil().Shift(-2)
}

// CollegeSummaryLine is the one-sentence version, for a chat tool result or an
// insight body. It is built from the computed figures and nothing else.
func (c CollegeResult) CollegeSummaryLine() string {
	if !c.Projectable {
		return c.Note
	}
	if c.FirstShortfallYear == 0 {
		return fmt.Sprintf("Fully funded: $%s covers all %d years, which total $%s in today's dollars.",
			c.BalanceAtEnrollment.StringFixed(2), c.Years, c.TotalCost.StringFixed(2))
	}
	return fmt.Sprintf("Funded through year %d; $%s short in year %d, and $%s short across all %d years (%s%% funded).",
		c.FirstShortfallYear-1, c.Years_[c.FirstShortfallYear-1].Shortfall.StringFixed(2),
		c.FirstShortfallYear, c.TotalShortfall.StringFixed(2), c.Years, c.FundedPct.String())
}
