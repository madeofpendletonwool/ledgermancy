package advisor

import (
	"fmt"
	"sort"

	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/goals"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/moneyfmt"
)

// THE RANKING RULE. A user will eventually ask why one option outranked another,
// and the answer must not be "the model decided". This is that answer, and it is
// written here verbatim because it is the thing the feature is accountable for.
//
//  1. Starter emergency fund. Until the household holds one month of trailing
//     fixed costs in liquid savings, that is the only option offered. No return
//     beats not borrowing at card rates for the next flat tyre.
//  2. Unclaimed employer match, while pay periods remain in the tax year.
//     Instant guaranteed 50-100%, and it expires.
//  3. Debt whose resolved APR exceeds the hurdle (the household's assumed real
//     return from projection_assumptions, floored at 6%). Above the hurdle a
//     guaranteed return genuinely beats an assumed one.
//  4. Full emergency fund to the household's target months.
//  5. Tax-advantaged contribution headroom that expires this tax year.
//  6. Goal acceleration, highest shortfall-against-target-date first.
//  7. Debt below the hurdle, and taxable investing, presented side by side as a
//     tradeoff rather than ranked against each other — the app does not have an
//     opinion on 3.5% guaranteed versus 7% assumed, and pretending to would be
//     the recommendation this feature says it does not make.
//
// Ties inside a tier break by larger computed dollar value, then by account
// name, so the order is TOTAL and STABLE: no two options can compare equal, and
// the same inputs give the same order twice.
//
// The rule REPLACED an earlier "guaranteed return first (debt APR), then
// limit-expiring opportunities, then goals", which is wrong in two directions
// that matter. Applied literally it pays down a 3.5% mortgage ahead of funding a
// Roth — a guaranteed 3.5% is not automatically better than an assumed 7% — and
// it drains a household's only emergency fund into a credit card, because
// liquidity is not a return at all and a rule made of returns cannot see it.
// Tiers 1 and 7 exist because of those two failures respectively.
const (
	tierStarterEF   = 1
	tierMatch       = 2
	tierDebtAbove   = 3
	tierFullEF      = 4
	tierHeadroom    = 5
	tierGoal        = 6
	tierTradeoff    = 7
	tierUnrankedEnd = 99 // sort position only; Option.Tier stays 0
)

// rank applies the waterfall and returns the finished, ordered list.
func rank(in inputs, muted suppression) []Option {
	// TIER 1 SHORT-CIRCUITS THE WHOLE WATERFALL. "Until the household holds one
	// month of trailing fixed costs in liquid savings, that is the ONLY option
	// offered" — so this returns early rather than adding a tier-1 option and
	// letting sorting put it on top. A household with no starter fund and a 24%
	// card must not be shown the card at all: seeing it is how the fund gets
	// drained into it.
	//
	// Suppressed is the one thing that reopens the waterfall. A household that
	// has explicitly muted the emergency-fund option has told the app it does
	// not want to be held here, and continuing to offer nothing would leave them
	// with an advisor that shows an empty panel forever.
	if starter, ok := starterOption(in); ok {
		if !muted.has(starter.Key) {
			return []Option{starter}
		}
	}

	var out []Option
	add := func(o Option, ok bool) {
		if ok && !muted.has(o.Key) {
			out = append(out, o)
		}
	}

	for _, m := range in.matches {
		add(matchOption(in, m))
	}
	for _, d := range in.debts {
		add(debtOption(in, d))
	}
	add(fullEFOption(in))
	for _, h := range in.headroom {
		add(headroomOption(in, h))
	}
	for _, g := range in.goals {
		add(goalOption(in, g))
	}
	add(taxableOption(in))

	sortOptions(out)
	if out == nil {
		return []Option{}
	}
	return out
}

// sortOptions imposes the total order: tier, then unbounded debts first within
// their tier, then larger computed value, then label.
//
// The final label comparison is what makes the order TOTAL. Without it two
// options with equal tier and equal value compare equal, sort.Slice is not
// stable, and the same household gets a different list on two consecutive loads
// of the same page — which is exactly the "the model decided" impression the
// published rule exists to prevent.
func sortOptions(out []Option) {
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]

		ai, bi := sortTier(a), sortTier(b)
		if ai != bi {
			return ai < bi
		}
		// "This debt never gets paid off" outranks any finite saving beside it.
		if a.Unbounded != b.Unbounded {
			return a.Unbounded
		}
		if !a.Value.Equal(b.Value) {
			return a.Value.GreaterThan(b.Value)
		}
		return a.Label < b.Label
	})
}

// sortTier maps an unranked option past every ranked one.
//
// Unranked options sort LAST — not as though their tier were zero, which would
// put them ahead of tier 1, and not as though their rate were zero, which would
// bury them in tier 7 among the below-hurdle debts. A debt whose rate is unknown
// is exactly the debt most likely to be the expensive one, so it gets its own
// position at the end where it is visible and labelled rather than mixed in.
func sortTier(o Option) int {
	if o.Unranked {
		return tierUnrankedEnd
	}
	return o.Tier
}

// --------------------------------------------------------------------------
// Tier 1 & 4 — emergency fund
// --------------------------------------------------------------------------

// starterOption is tier 1: one month of trailing fixed costs, held liquid.
func starterOption(in inputs) (Option, bool) {
	// With no trailing fixed costs there is no target to hold against — a fresh
	// household with no history is not "missing an emergency fund", the app just
	// does not know what a month costs them yet.
	if !in.starterTarget.IsPositive() || !in.liquid.LessThan(in.starterTarget) {
		return Option{}, false
	}
	gap := in.starterTarget.Sub(in.liquid).Round(2)
	return Option{
		Key:       OptionKey(KindEmergencyFund, "starter"),
		Kind:      KindEmergencyFund,
		SubjectID: "starter",
		Tier:      tierStarterEF,
		Label:     "Start an emergency fund",
		Detail: fmt.Sprintf(
			"Holding one month of typical fixed costs (%s) in savings would take %s. You have %s liquid now. Until that is covered this is the only option shown — no return beats not borrowing at card rates for the next emergency.",
			money(in.starterTarget), money(gap), money(in.liquid),
		),
		Amount:    contribution(in.slack.amount, gap),
		Value:     gap,
		ValueKind: ValueGapClosed,
		Basis: []BasisLine{
			{Label: "Typical monthly fixed costs", Value: money(in.starterTarget)},
			{Label: "Liquid savings today", Value: money(in.liquid)},
			{Label: "Gap to one month", Value: money(gap)},
		},
	}, true
}

// fullEFOption is tier 4: top the fund up to the household's target months.
func fullEFOption(in inputs) (Option, bool) {
	if !in.fullTarget.IsPositive() || !in.liquid.LessThan(in.fullTarget) {
		return Option{}, false
	}
	gap := in.fullTarget.Sub(in.liquid).Round(2)
	months := in.liquid.Div(in.slack.fixedCosts).Round(1)
	return Option{
		Key:       OptionKey(KindEmergencyFund, "full"),
		Kind:      KindEmergencyFund,
		SubjectID: "full",
		Tier:      tierFullEF,
		Label:     fmt.Sprintf("Top up the emergency fund to %d months", in.efMonths),
		Detail: fmt.Sprintf(
			"You hold %s, about %s months of typical fixed costs. Reaching %d months would take another %s.",
			money(in.liquid), months.String(), in.efMonths, money(gap),
		),
		Amount:    contribution(in.slack.amount, gap),
		Value:     gap,
		ValueKind: ValueGapClosed,
		Basis: []BasisLine{
			{Label: "Typical monthly fixed costs", Value: money(in.slack.fixedCosts)},
			{Label: "Target", Value: fmt.Sprintf("%d months (%s)", in.efMonths, money(in.fullTarget))},
			{Label: "Liquid savings today", Value: money(in.liquid)},
			{Label: "Gap to target", Value: money(gap)},
		},
	}, true
}

// --------------------------------------------------------------------------
// Tier 2 — unclaimed employer match
// --------------------------------------------------------------------------

// matchOption is tier 2: money the employer has offered and the household is not
// collecting.
//
// THE SINGLE HIGHEST-VALUE THING THIS RANKER CAN SAY. The return is 50–100%
// INSTANTLY — larger than any APR in the household and larger than any assumed
// market return — and unlike every other option it expires.
//
// Under the app's match model the employer pays pct × salary and pays it only
// against what the employee actually defers (networth.annualMatch's clamp), so
// the extra the employee must contribute to collect the unclaimed part equals
// that unclaimed part. The Detail says so rather than leaving the reader to
// wonder why the two figures match.
func matchOption(in inputs, m matchInput) (Option, bool) {
	// Expired: no pay periods left in the tax year means the headroom is gone,
	// not merely late.
	if in.matchPeriodsLeft <= 0 {
		return Option{}, false
	}
	perMonth := m.unclaimed.Div(decimal.NewFromInt(int64(in.matchMonthsLeft))).Round(2)
	return Option{
		Key:       OptionKey(KindEmployerMatch, m.accountID),
		Kind:      KindEmployerMatch,
		SubjectID: m.accountID,
		Tier:      tierMatch,
		Label:     fmt.Sprintf("Capture the full employer match on %s", m.name),
		Detail: fmt.Sprintf(
			"Your employer will match up to %s a year and is paying %s at your current contribution, leaving %s on the table. Contributing about %s more a month for the %d months left in the tax year would collect it. Your plan matches what you defer, so the extra you put in is the extra they pay.",
			money(m.full), money(m.captured), money(m.unclaimed), money(perMonth), in.matchMonthsLeft,
		),
		Amount:    perMonth,
		Value:     m.unclaimed,
		ValueKind: ValueMatchCaptured,
		Basis: []BasisLine{
			{Label: "Match available at a full contribution", Value: money(m.full)},
			{Label: "Match being paid today", Value: money(m.captured)},
			{Label: "Left on the table this year", Value: money(m.unclaimed)},
			{Label: "Pay periods left in the tax year", Value: fmt.Sprintf("%d (%s)", in.matchPeriodsLeft, in.matchPeriodsBasis)},
		},
	}, true
}

// --------------------------------------------------------------------------
// Tiers 3 & 7 — debt
// --------------------------------------------------------------------------

// debtOption places one debt on its side of the hurdle and computes what an
// extra payment would avoid.
//
// APR IS A PERCENTAGE HERE (22.99 for a 22.99% card), matching liabilities.apr,
// account_terms.apr and ComputePayoff's monthlyRate. Comparing it against a
// hurdle held as a fraction is the trap this feature is most likely to fall
// into, so both sides are percentages and resolveHurdle converts once.
func debtOption(in inputs, d debt) (Option, bool) {
	key := OptionKey(KindDebtPaydown, d.id.String())

	// UNKNOWN APR: listed, unranked, labelled. Never defaulted, never silently
	// dropped, and never sorted as though its rate were zero.
	if !d.hasAPR() {
		return Option{
			Key:       key,
			Kind:      KindDebtPaydown,
			SubjectID: d.id.String(),
			Unranked:  true,
			Label:     fmt.Sprintf("Pay down %s", d.name),
			Note:      "APR unknown — add it to compare this",
			Detail: fmt.Sprintf(
				"%s has a balance of %s. Without a rate this cannot be compared against the other options; adding it on the account puts it in the ranking.",
				d.name, money(d.balance),
			),
			ValueKind: ValueInterestAvoided,
			Basis: []BasisLine{
				{Label: "Balance", Value: money(d.balance)},
				{Label: "APR", Value: "not known"},
			},
		}, true
	}

	tier := tierTradeoff
	tradeoff := true
	if d.terms.APR.GreaterThan(in.hurdle) {
		tier, tradeoff = tierDebtAbove, false
	}

	extra := in.slack.amount
	basis := []BasisLine{
		{Label: "Balance", Value: money(d.balance)},
		{Label: "APR", Value: pct(d.terms.APR)},
		{Label: "Hurdle", Value: fmt.Sprintf("%s — %s", pct(in.hurdle), in.hurdleBasis)},
	}

	// PAYMENT UNKNOWN: the rate places it in the waterfall, but there is no
	// schedule to shorten, so no interest figure is invented for it.
	if !d.hasPayment() {
		return Option{
			Key:       key,
			Kind:      KindDebtPaydown,
			SubjectID: d.id.String(),
			Tier:      tier,
			Tradeoff:  tradeoff,
			Label:     fmt.Sprintf("Pay down %s", d.name),
			Note:      "monthly payment unknown — add it to see interest avoided",
			Detail: fmt.Sprintf(
				"%s carries %s on a balance of %s. Paying it down returns that %s guaranteed. Adding the monthly payment on the account would let this show the interest avoided and the months saved.",
				d.name, pct(d.terms.APR), money(d.balance), pct(d.terms.APR),
			),
			Amount:    extra,
			ValueKind: ValueInterestAvoided,
			Basis:     append(basis, BasisLine{Label: "Monthly payment", Value: "not known"}),
		}, true
	}

	base := goals.ComputePayoff(d.balance, d.terms.APR, d.terms.Payment, nil, in.now)
	accel := goals.ComputePayoff(d.balance, d.terms.APR, d.terms.Payment.Add(extra), nil, in.now)
	basis = append(basis,
		BasisLine{Label: "Monthly payment", Value: money(d.terms.Payment)},
		BasisLine{Label: "With this added", Value: money(d.terms.Payment.Add(extra))},
	)

	opt := Option{
		Key:       key,
		Kind:      KindDebtPaydown,
		SubjectID: d.id.String(),
		Tier:      tier,
		Tradeoff:  tradeoff,
		Label:     fmt.Sprintf("Pay down %s", d.name),
		Amount:    extra,
		ValueKind: ValueInterestAvoided,
		Basis:     basis,
	}

	switch {
	case base.NeverPaysOff && accel.NeverPaysOff:
		// Neither clears it. Reporting an interest saving here would be
		// arithmetic on two numbers that do not exist.
		opt.Unbounded = true
		opt.Note = "this balance does not clear at either payment"
		opt.Detail = fmt.Sprintf(
			"%s carries %s and the interest alone is %s a month. Neither the current payment (%s) nor adding %s clears it — the payment has to exceed the monthly interest before a payoff date exists.",
			d.name, pct(d.terms.APR), money(base.MonthlyInterest),
			money(d.terms.Payment), money(extra),
		)
	case base.NeverPaysOff:
		// The strongest thing the app can say about a debt: it goes from never
		// clearing to clearing. Interest avoided is unbounded rather than a
		// difference of two finite totals, so Value stays zero and Unbounded
		// carries the meaning — see sortOptions.
		opt.Unbounded = true
		opt.Detail = fmt.Sprintf(
			"At %s a month this balance never clears — the interest alone is %s a month. Adding %s clears it in %d months, at %s total interest.",
			money(d.terms.Payment), money(base.MonthlyInterest), money(extra),
			accel.Months, money(accel.TotalInterest),
		)
		opt.Basis = append(opt.Basis,
			BasisLine{Label: "Monthly interest at today's balance", Value: money(base.MonthlyInterest)},
			BasisLine{Label: "Payoff with this added", Value: fmt.Sprintf("%d months", accel.Months)},
		)
	default:
		avoided := base.TotalInterest.Sub(accel.TotalInterest).Round(2)
		if avoided.IsNegative() {
			avoided = decimal.Zero
		}
		opt.Value = avoided
		saved := base.Months - accel.Months
		opt.Detail = fmt.Sprintf(
			"%s carries %s. Adding %s a month clears it %d months sooner and avoids %s in interest — a guaranteed %s return, against your assumed %s.",
			d.name, pct(d.terms.APR), money(extra), saved, money(avoided),
			pct(d.terms.APR), pct(in.hurdle),
		)
		opt.Basis = append(opt.Basis,
			BasisLine{Label: "Payoff at the current payment", Value: fmt.Sprintf("%d months, %s interest", base.Months, money(base.TotalInterest))},
			BasisLine{Label: "Payoff with this added", Value: fmt.Sprintf("%d months, %s interest", accel.Months, money(accel.TotalInterest))},
			BasisLine{Label: "Interest avoided", Value: money(avoided)},
		)
	}

	return opt, true
}

// --------------------------------------------------------------------------
// Tier 5 — tax-advantaged headroom
// --------------------------------------------------------------------------

// headroomOption is tier 5: contribution room that expires with the tax year.
//
// A CAP IS NOT PERMISSION. This reports the room the IRS limit leaves against
// what the household plans to contribute; it does not check eligibility, so a
// household over a Roth phase-out could be shown room it may not use. Doc 32
// owns the eligibility table, and the copy here says "room" rather than "you can
// contribute" precisely because this cannot yet tell the difference.
func headroomOption(in inputs, h headroomInput) (Option, bool) {
	perMonth := h.headroom.Div(decimal.NewFromInt(int64(in.matchMonthsLeft))).Round(2)
	return Option{
		Key:       OptionKey(KindTaxAdvantaged, h.group),
		Kind:      KindTaxAdvantaged,
		SubjectID: h.group,
		Tier:      tierHeadroom,
		Label:     fmt.Sprintf("Use this year's %s room", limitGroupLabel(h.group)),
		Detail: fmt.Sprintf(
			"Your planned contributions use %s of the %s %s limit, leaving %s of room that does not carry into next year. That is about %s a month for the %d months left.",
			money(h.planned), money(h.limit), limitGroupLabel(h.group),
			money(h.headroom), money(perMonth), in.matchMonthsLeft,
		),
		Amount:    perMonth,
		Value:     h.headroom,
		ValueKind: ValueHeadroom,
		Basis: []BasisLine{
			{Label: "Annual limit", Value: money(h.limit)},
			{Label: "Planned contributions this year", Value: money(h.planned)},
			{Label: "Room left", Value: money(h.headroom)},
			{Label: "Months left in the tax year", Value: fmt.Sprintf("%d", in.matchMonthsLeft)},
		},
	}, true
}

// limitGroupLabel is the word a shared-limit group is known by in copy.
func limitGroupLabel(group string) string {
	switch group {
	case "401k":
		return "401(k)"
	case "ira":
		return "IRA"
	case "hsa":
		return "HSA"
	default:
		return group
	}
}

// --------------------------------------------------------------------------
// Tier 6 — goal acceleration
// --------------------------------------------------------------------------

// goalOption is tier 6: what an extra monthly contribution does to a goal's
// completion date.
//
// Ranked by SHORTFALL against the target date, per the rule — the goal furthest
// behind its own schedule first, not the largest goal. Shortfall is computed
// against the household's actual trailing surplus (loadGoals), so "behind"
// means the same thing here as it does in the feed.
func goalOption(in inputs, g goalInput) (Option, bool) {
	if !g.feas.Remaining.IsPositive() {
		return Option{}, false
	}
	extra := in.slack.amount

	opt := Option{
		Key:       OptionKey(KindGoalAccel, g.id.String()),
		Kind:      KindGoalAccel,
		SubjectID: g.id.String(),
		Tier:      tierGoal,
		Label:     fmt.Sprintf("Put it toward %s", g.name),
		Amount:    extra,
		ValueKind: ValueMonthsEarlier,
		Basis: []BasisLine{
			{Label: "Still to save", Value: money(g.feas.Remaining)},
			{Label: "Needed each month to hit the date", Value: money(g.feas.RequiredMonthly)},
			{Label: "Shortfall against that", Value: money(g.feas.Shortfall)},
		},
	}

	// An open-ended goal, or one with no required pace, has no baseline to
	// compare against — so this reports when the extra alone would finish it
	// rather than inventing a schedule to beat.
	if !g.feas.RequiredMonthly.IsPositive() {
		alone := monthsToSave(g.feas.Remaining, extra)
		opt.Detail = fmt.Sprintf(
			"%s has %s still to save and no monthly schedule set. Putting %s a month in would complete it in %d months.",
			g.name, money(g.feas.Remaining), money(extra), alone,
		)
		opt.Basis = append(opt.Basis, BasisLine{Label: "Completed on this alone", Value: fmt.Sprintf("%d months", alone)})
		return opt, true
	}

	before := monthsToSave(g.feas.Remaining, g.feas.RequiredMonthly)
	after := monthsToSave(g.feas.Remaining, g.feas.RequiredMonthly.Add(extra))
	earlier := before - after
	opt.Value = decimal.NewFromInt(int64(earlier))
	opt.Detail = fmt.Sprintf(
		"%s needs %s and %s a month to stay on schedule, completing in %d months. Adding %s finishes it in %d — %d months earlier.",
		g.name, money(g.feas.Remaining), money(g.feas.RequiredMonthly), before,
		money(extra), after, earlier,
	)
	opt.Basis = append(opt.Basis,
		BasisLine{Label: "Completed at the current pace", Value: fmt.Sprintf("%d months", before)},
		BasisLine{Label: "Completed with this added", Value: fmt.Sprintf("%d months", after)},
	)
	return opt, true
}

// monthsToSave is how many whole months of `monthly` it takes to reach
// `remaining`, rounded up. A non-positive monthly returns zero rather than
// dividing.
func monthsToSave(remaining, monthly decimal.Decimal) int {
	if !monthly.IsPositive() {
		return 0
	}
	return int(remaining.Div(monthly).Ceil().IntPart())
}

// --------------------------------------------------------------------------
// Tier 7 — taxable investing
// --------------------------------------------------------------------------

// taxableOption is the other half of tier 7, presented BESIDE the below-hurdle
// debts rather than ranked against them.
//
// This is where the feature's whole position is visible: the app has computed
// both sides — a guaranteed APR below the hurdle, and a growth figure at the
// household's own assumed return — and it declines to say which is better,
// because it does not know. Both carry Tradeoff, and the UI renders that tier as
// a comparison rather than a ranking.
func taxableOption(in inputs) (Option, bool) {
	if !in.realReturn.IsPositive() {
		return Option{}, false
	}
	const years = 10
	amount := in.slack.amount
	fv := futureValue(amount, in.realReturn, years*12)
	contributed := amount.Mul(decimal.NewFromInt(years * 12)).Round(2)
	growth := fv.Sub(contributed).Round(2)

	return Option{
		Key:       KindTaxableInvest,
		Kind:      KindTaxableInvest,
		Tier:      tierTradeoff,
		Tradeoff:  true,
		Label:     "Invest it in a taxable account",
		Amount:    amount,
		Value:     growth,
		ValueKind: ValueProjectedGrowth,
		Detail: fmt.Sprintf(
			"Investing %s a month for %d years at your assumed %s real return would come to %s, of which %s is growth. This return is ASSUMED, not guaranteed — that is the difference between it and paying down a debt, and it is why neither is ranked above the other here.",
			money(amount), years, pct(in.hurdleFromReturn()), money(fv), money(growth),
		),
		Basis: []BasisLine{
			{Label: "Monthly amount", Value: money(amount)},
			{Label: "Assumed real return", Value: pct(in.hurdleFromReturn())},
			{Label: "Horizon", Value: fmt.Sprintf("%d years", years)},
			{Label: "Total contributed", Value: money(contributed)},
			{Label: "Projected value", Value: money(fv)},
		},
	}, true
}

// hurdleFromReturn is the household's assumed real return as a PERCENTAGE. It is
// not the hurdle: the hurdle is floored at 6% and this is not, because this
// figure describes what the household actually assumes rather than the bar the
// ranker holds debt to.
func (in inputs) hurdleFromReturn() decimal.Decimal {
	return in.realReturn.Mul(decimal.NewFromInt(100)).Round(2)
}

// futureValue compounds a monthly contribution for n months at an annual rate.
//
// GROWTH APPLIES TO THE BALANCE BEFORE THE MONTH'S CONTRIBUTION, so a deposit is
// not credited with a full month of return it did not earn. That is the same
// convention networth.Project and ProjectRetirement use, and the two must not
// drift: a household comparing this figure against the Retirement page's curve
// is entitled to find them consistent.
func futureValue(monthly, annualRate decimal.Decimal, months int) decimal.Decimal {
	rate := annualRate.Div(decimal.NewFromInt(12))
	balance := decimal.Zero
	for i := 0; i < months; i++ {
		balance = balance.Add(balance.Mul(rate)).Add(monthly)
	}
	return balance.Round(2)
}

// --------------------------------------------------------------------------
// Shared helpers
// --------------------------------------------------------------------------

// contribution is how much of the slack an option that closes a finite gap
// should ask for: the whole gap when the slack covers it, the whole slack
// otherwise. Asking for more than the gap would overshoot the target; asking for
// more than the slack would propose money the household does not have.
func contribution(slack, gap decimal.Decimal) decimal.Decimal {
	if gap.LessThan(slack) {
		return gap
	}
	return slack
}

// money renders a figure the way every other surface does.
//
// moneyfmt.USD, not a local formatter: that package exists precisely because
// two of these had already been written and disagreed about thousands
// separators, so the same amount read "$1234.56" in an insight and "$1,234.56"
// in the monthly recap. The advisor quotes four-figure amounts constantly and
// hands every one of them to the model verbatim.
func money(d decimal.Decimal) string { return moneyfmt.USD(d.Round(2)) }

// pct renders a percentage. Two places: nobody quotes a card at 18.990%.
func pct(d decimal.Decimal) string { return d.Round(2).String() + "%" }
