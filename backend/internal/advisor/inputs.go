package advisor

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/goals"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/networth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/reporting"
)

// sharedUser is the household-shared visibility scope. EVERY query in this file
// passes it.
//
// It is the same scope BuildSafeToSpend runs under, and that agreement is not
// cosmetic: an advisor that ranked options over a member's PRIVATE debts while
// computing slack from only SHARED income would be allocating money it cannot
// see against money the rest of the household cannot see either. One scope, so
// a private account changes neither the slack figure nor any option.
var sharedUser = uuid.Nil

// hurdleFloor is the lowest the APR hurdle may fall, as a PERCENTAGE.
//
// The hurdle is the household's own assumed real return, which is the honest
// anchor — but a household that has set a 2% assumption has not thereby made a
// 3% car loan a good use of a marginal dollar. Six percent is the floor at
// which "guaranteed beats assumed" stops being obviously true.
var hurdleFloor = decimal.NewFromInt(6)

// debt is one liability with its terms already resolved.
type debt struct {
	id      uuid.UUID
	name    string
	balance decimal.Decimal
	terms   goals.DebtTerms
}

// hasAPR reports whether a rate is known from anywhere. The SOURCE is what is
// tested, never the value: a genuine 0% promotional card and a card whose rate
// nobody knows are different facts, and collapsing them is how a debt with an
// unknown rate gets sorted as though it were free.
func (d debt) hasAPR() bool { return d.terms.APRSource != goals.TermSourceNone }

// hasPayment reports whether a monthly payment is known. Same rule.
func (d debt) hasPayment() bool { return d.terms.PaymentSource != goals.TermSourceNone }

// goalInput is one savings goal with its feasibility already computed.
type goalInput struct {
	id   uuid.UUID
	name string
	feas goals.Feasibility
}

// matchInput is one retirement account with unclaimed employer match.
type matchInput struct {
	accountID string
	name      string
	full      decimal.Decimal // match available at a full deferral
	captured  decimal.Decimal // match actually paid at today's deferral
	unclaimed decimal.Decimal // full − captured, always positive here
}

// headroomInput is one shared-limit group's remaining tax-advantaged room.
type headroomInput struct {
	group    string
	label    string
	limit    decimal.Decimal
	planned  decimal.Decimal
	headroom decimal.Decimal
}

// inputs is everything the ranker needs, all of it already computed.
type inputs struct {
	now   time.Time
	slack slackResult

	hurdle      decimal.Decimal
	hurdleBasis string
	realReturn  decimal.Decimal // fraction, e.g. 0.05

	// Emergency fund.
	liquid        decimal.Decimal
	starterTarget decimal.Decimal // one month of trailing fixed costs
	fullTarget    decimal.Decimal // efMonths × trailing fixed costs
	efMonths      int

	debts    []debt
	matches  []matchInput
	headroom []headroomInput
	goals    []goalInput

	// matchPeriodsLeft is how many pay periods remain in the tax year, and
	// matchPeriodsBasis says how that was determined. Zero periods means the
	// employer-match option is not offered at all — the match is captured per
	// pay period, so headroom unclaimed by the last cheque of the year is gone,
	// not merely late.
	matchPeriodsLeft  int
	matchPeriodsBasis string
	// matchMonthsLeft is the number of whole months left in the tax year,
	// including the current one. It is what an "extra per month" figure is
	// spread over.
	matchMonthsLeft int
}

// gather loads every deterministic input. One failure sinks the run: unlike the
// insight engine, where a producer failing must not take the feed with it, a
// PARTIAL advisor list is actively harmful — an option missing because its query
// errored is indistinguishable from an option the waterfall correctly did not
// offer, and the household would be reading a ranking built on inputs the app
// silently failed to load.
func gather(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time, slack slackResult) (inputs, error) {
	in := inputs{now: now, slack: slack}

	assumptions, err := q.GetProjectionAssumptions(ctx, householdID)
	if err != nil {
		return inputs{}, err
	}
	in.realReturn = assumptions.RealReturnRate
	in.hurdle, in.hurdleBasis = resolveHurdle(in.realReturn)

	// Emergency fund. The target is measured in months of TRAILING TYPICAL fixed
	// costs, not the bill-aware figure: an emergency fund covers a typical month
	// of outgoings, not this particular month's dated bills.
	in.liquid, err = liquidSavings(ctx, q, householdID)
	if err != nil {
		return inputs{}, err
	}
	in.efMonths = emergencyMonthsFor(ctx, q, householdID)
	in.starterTarget = slack.fixedCosts
	in.fullTarget = slack.fixedCosts.Mul(decimal.NewFromInt(int64(in.efMonths)))

	if in.debts, err = loadDebts(ctx, q, householdID); err != nil {
		return inputs{}, err
	}

	age := storedAge(assumptions, now)
	if in.matches, in.headroom, err = loadRetirement(ctx, q, householdID, age, now); err != nil {
		return inputs{}, err
	}

	in.matchMonthsLeft = monthsLeftInYear(now)
	in.matchPeriodsLeft, in.matchPeriodsBasis, err = matchWindow(ctx, q, householdID, now)
	if err != nil {
		return inputs{}, err
	}

	if in.goals, err = loadGoals(ctx, q, householdID, now); err != nil {
		return inputs{}, err
	}

	return in, nil
}

// resolveHurdle derives the APR above which a guaranteed return genuinely beats
// the household's assumed one.
//
// THIS IS THE HONEST PART OF THE RANKING RULE. It is derived from the
// household's OWN stated assumption rather than a constant, which is what lets
// the ranker order debt against investing without claiming to know which is
// better in general: steps 3 and 7 of the waterfall sit on opposite sides of it,
// and a household that assumes a 4% real return and one that assumes 8% get
// genuinely different orderings from the same rule.
//
// The stored rate is a FRACTION (0.05); the hurdle is a PERCENTAGE (5), because
// that is the unit every APR in the app is stored in.
func resolveHurdle(realReturn decimal.Decimal) (decimal.Decimal, string) {
	assumed := realReturn.Mul(decimal.NewFromInt(100))
	if assumed.GreaterThan(hurdleFloor) {
		return assumed.Round(2), "your assumed real return"
	}
	return hurdleFloor, "the 6% floor (your assumed real return is lower)"
}

// storedAge resolves the household's age for contribution-limit purposes.
//
// The advisor has no CALLING MEMBER — it runs for a household, from a job as
// often as from a request — so unlike the retirement endpoint it cannot prefer
// "the caller's birthdate". It uses the household's stored age and nothing else.
// With no age at all, AnnualLimitFor returns the base limit without catch-up,
// which UNDERSTATES headroom. That is the conservative direction, and the same
// one FamilyHSA defaults in.
func storedAge(a dbgen.ProjectionAssumption, now time.Time) int {
	stored := 0
	if a.CurrentAge != nil {
		stored = int(*a.CurrentAge)
	}
	if age, ok := networth.ResolveAge(nil, stored, now); ok {
		return age
	}
	return 0
}

// liquidSavings is what the household could actually reach in an emergency:
// shared depository balances.
//
// Depository only. An emergency fund's defining property is that it is there on
// the day the boiler dies, so a brokerage balance does not count (it may be down
// 20% that week and it settles in days), and a retirement balance REALLY does
// not — reaching it means a penalty, which is the thing an emergency fund exists
// to avoid. Understating this is the safe direction: it keeps the household at
// tier 1 slightly longer.
func liquidSavings(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID) (decimal.Decimal, error) {
	rows, err := q.ListVisibleAccounts(ctx, dbgen.ListVisibleAccountsParams{
		HouseholdID: householdID, UserID: sharedUser,
	})
	if err != nil {
		return decimal.Zero, err
	}
	total := decimal.Zero
	for _, r := range rows {
		if r.Type != "depository" || !r.CurrentBalance.Valid {
			continue
		}
		// A negative depository balance is an overdraft, not negative savings to
		// be netted against another account's positive one.
		if r.CurrentBalance.Decimal.IsPositive() {
			total = total.Add(r.CurrentBalance.Decimal)
		}
	}
	return total.Round(2), nil
}

// loadDebts reads every shared debt with its terms merged through the SAME
// helper the Net Worth and Goals pages use.
func loadDebts(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID) ([]debt, error) {
	rows, err := q.ListVisibleLiabilities(ctx, dbgen.ListVisibleLiabilitiesParams{
		HouseholdID: householdID, UserID: sharedUser,
	})
	if err != nil {
		return nil, err
	}
	out := make([]debt, 0, len(rows))
	for _, r := range rows {
		bal := decimal.Zero
		if r.CurrentBalance.Valid {
			bal = r.CurrentBalance.Decimal
		}
		// A cleared debt is not an option; offering to pay down a zero balance
		// is noise.
		if !bal.IsPositive() {
			continue
		}
		out = append(out, debt{
			id:      r.AccountID,
			name:    r.AccountName,
			balance: bal,
			terms: goals.MergeDebtTerms(
				r.ManualApr, r.Apr, r.InterestRatePercentage,
				r.ObligationAmount, r.ManualMinimumPayment, r.MinimumPayment,
			),
		})
	}
	return out, nil
}

// loadRetirement builds both retirement-flavoured inputs in one pass over the
// projectable accounts: unclaimed employer match per account, and
// tax-advantaged headroom per SHARED-LIMIT GROUP.
//
// Grouping matters for headroom and not for match. Two 401(k)s share one
// elective limit, so offering each of them the whole of it would tell a
// household it may contribute twice what the IRS allows — networth.LimitGroup is
// the same pooling the projection's own capping uses. A match, by contrast, is
// per employer and per account: two accounts with two matches really are two
// separate pots of money on the table.
func loadRetirement(
	ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, age int, now time.Time,
) ([]matchInput, []headroomInput, error) {
	rows, err := q.ListProjectableAccounts(ctx, dbgen.ListProjectableAccountsParams{
		HouseholdID: householdID, UserID: sharedUser,
	})
	if err != nil {
		return nil, nil, err
	}

	limits, configured := networth.Limits(now.Year())

	var matches []matchInput
	type groupAcc struct {
		label     string
		treatment string
		planned   decimal.Decimal
	}
	groups := map[string]*groupAcc{}
	order := []string{}

	for _, r := range rows {
		p := toPlan(r, now)

		// Employer match. annualMatch REFUSES TO GUESS when the terms or the
		// salary are missing, and so does this: a household with a match
		// percentage but no stated salary produces no match option at all,
		// rather than a zero or an invented figure.
		full := networth.FullAnnualMatch(p)
		if full.IsPositive() {
			captured := networth.AnnualMatch(p)
			if unclaimed := full.Sub(captured); unclaimed.IsPositive() {
				matches = append(matches, matchInput{
					accountID: p.ID,
					name:      p.Name,
					full:      full.Round(2),
					captured:  captured.Round(2),
					unclaimed: unclaimed.Round(2),
				})
			}
		}

		// Tax-advantaged headroom. An untagged account has no known limit and is
		// skipped rather than defaulted — the same rule the projection applies.
		if p.Treatment == "" || !configured {
			continue
		}
		g := networth.LimitGroup(p.Treatment)
		if groups[g] == nil {
			groups[g] = &groupAcc{label: p.Treatment, treatment: p.Treatment}
			order = append(order, g)
		}
		groups[g].planned = groups[g].planned.Add(p.MonthlyContribution.Mul(decimal.NewFromInt(12)))
	}

	headroom := make([]headroomInput, 0, len(order))
	for _, g := range order {
		acc := groups[g]
		limit, capped := networth.AnnualLimitFor(acc.treatment, age, limits, false)
		if !capped {
			continue // taxable, 529, trust, UTMA: no federal annual deferral cap
		}
		room := limit.Sub(acc.planned)
		if !room.IsPositive() {
			continue
		}
		headroom = append(headroom, headroomInput{
			group:    g,
			label:    acc.label,
			limit:    limit,
			planned:  acc.planned.Round(2),
			headroom: room.Round(2),
		})
	}

	return matches, headroom, nil
}

// toPlan maps a projectable-account row onto the engine's plan type, through the
// one definition of that mapping (networth.PlanFromRow).
//
// It used to be a hand-written copy that dropped the beneficiary horizon on the
// grounds that "a custodial account contributes neither a match option nor
// household headroom". That was true of THIS file's caller and false of the
// other one: fillRetirement maps rows through here and then runs
// ProjectRetirement for the FI age, where a 529 with no horizon never stops
// compounding and is counted as retirement money it will never be. Sharing the
// mapping fixes that and the per-account assumed real return in one move.
func toPlan(r dbgen.ListProjectableAccountsRow, now time.Time) networth.AccountPlan {
	return networth.PlanFromRow(r, now)
}

// loadGoals reads the household's active SAVINGS goals with their feasibility
// computed against the household's actual pace.
//
// Savings only. goals.Compute is accumulation arithmetic with no notion of
// interest, so running it over a debt-payoff goal would read the debt's balance
// as progress — the same exclusion reporting.goalContributions and the goal
// producer both make. Debt is ranked here as a DEBT option, with amortization,
// which is the arithmetic it actually needs.
//
// Household-scoped goals only, for the same reason the goal producer coaches
// only those: the advisor's output rides a household-shared surface, and
// surfacing a personal goal on it would leak it to every member.
func loadGoals(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time) ([]goalInput, error) {
	rows, err := q.ListActiveHouseholdGoals(ctx, householdID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}

	// The same trailing surplus the goal producer and the forecast default to,
	// so "behind schedule" means one thing across the app.
	surplus, err := networth.DefaultMonthlySurplus(ctx, q, householdID, sharedUser, now)
	if err != nil {
		return nil, err
	}

	out := make([]goalInput, 0, len(rows))
	for _, g := range rows {
		if g.Kind != "savings" {
			continue
		}
		current, err := reporting.GoalProgress(ctx, q, g, now)
		if err != nil {
			return nil, err
		}
		f := goals.Compute(g.TargetAmount, current, surplus, g.TargetDate, now)
		if f.Achieved {
			continue
		}
		out = append(out, goalInput{id: g.ID, name: g.Name, feas: f})
	}
	return out, nil
}

// monthsLeftInYear counts whole months remaining in the calendar year including
// the current one, so December returns 1 and never zero.
func monthsLeftInYear(now time.Time) int {
	return 12 - int(now.UTC().Month()) + 1
}
