package reporting

import (
	"sort"
	"time"

	"github.com/shopspring/decimal"
)

// Return calculations.
//
// This file is the one place in the app where a number could quietly lie at
// scale. A wrong spending total is visibly wrong; a wrong annualised return
// looks exactly like a right one. Three rules follow from that:
//
//  1. Every figure is computed in shopspring/decimal, never float64. The one
//     exception is the exponentiation inside the IRR solve, which is documented
//     where it happens and bounded so it cannot smuggle error into a reported
//     value.
//  2. When an input cannot support a figure, the figure is NOT computed. A
//     "not computable" is a legitimate, honest answer and every result type
//     here can express it. Returning a plausible-looking number from
//     insufficient data is the failure mode this file exists to avoid.
//  3. External cash flows — money crossing the account boundary — are
//     classified in exactly one place (externalFlowSubtypes) so the two return
//     measures cannot disagree about what a deposit is.

// externalFlowSubtypes are the Plaid investment-transaction subtypes that move
// money across the account boundary.
//
// This distinction is the whole ballgame. A buy moves cash into a security
// *inside* the account and changes nothing about performance; a contribution
// moves new money *in* and must not be credited to the market. Getting this
// backwards inflates every return figure by the household's savings rate, which
// is precisely the flattering error a finance app must not make.
//
// Dividends, interest and fees are deliberately absent: they are earned or lost
// inside the account, so they belong in the return, not beside it.
var externalFlowSubtypes = map[string]bool{
	"contribution": true,
	"deposit":      true,
	"withdrawal":   true,
	"distribution": true,
	"transfer":     true,
	"send":         true,
	"request":      true,
}

// IsExternalFlow reports whether an investment transaction moved money across
// the account boundary.
//
// Both the type and the subtype are consulted: Plaid's 'cash' and 'transfer'
// types are where boundary movements live, and a subtype like "transfer" under
// a 'buy' type would be an in-account reallocation, not new money.
func IsExternalFlow(txnType, subtype string) bool {
	switch txnType {
	case "cash", "transfer":
		return externalFlowSubtypes[subtype]
	default:
		return false
	}
}

// Valuation is the portfolio's worth on one day.
type Valuation struct {
	Date  time.Time
	Value decimal.Decimal
}

// Flow is one external cash movement.
//
// Amount follows the *investor's* perspective, not Plaid's: positive means
// money went INTO the portfolio. Plaid's raw sign is the opposite for a deposit
// (cash credited is negative), so the flip happens once, in FlowsFromRows, and
// nowhere else.
type Flow struct {
	Date   time.Time
	Amount decimal.Decimal
}

// PerformanceInput is everything the return measures need.
type PerformanceInput struct {
	// Valuations must be sorted ascending by date and contain at least two
	// points for any return to be computable.
	Valuations []Valuation
	Flows      []Flow
}

// Performance is a period's results. Every field is optional because every
// figure has inputs that can fail to support it, and a nil is the honest answer.
type Performance struct {
	// Start and End bracket what was ACTUALLY measured, which may be a shorter
	// span than the caller asked for. The UI must caveat with these, not with
	// the requested period.
	Start time.Time
	End   time.Time
	Days  int

	StartValue decimal.Decimal
	EndValue   decimal.Decimal
	// NetFlows is contributions minus withdrawals over the measured span.
	NetFlows decimal.Decimal
	// Gain is EndValue − StartValue − NetFlows: the money the market made (or
	// lost), with the user's own deposits removed.
	Gain decimal.Decimal

	// TWR is the time-weighted return over the whole span, as a fraction
	// (0.0734 = 7.34%). Neutral to when money was added — this measures the
	// portfolio, not the investor.
	TWR *decimal.Decimal
	// MWR is the money-weighted (internal rate of) return, ANNUALISED, as a
	// fraction. This measures the investor's actual outcome. Nil when the solve
	// did not converge or the flows do not bracket a root.
	MWR *decimal.Decimal
	// MWRNote explains a nil MWR in words the UI can show, so "not computable"
	// never renders as a silent blank.
	MWRNote string
	// Annualised is TWR expressed as an annual rate. Nil for spans under a year
	// ON PURPOSE: annualising six weeks of history produces a number that is
	// arithmetically defensible and practically a lie.
	Annualised *decimal.Decimal
}

// minAnnualiseDays is the shortest span that may be annualised. A full year, so
// that scaling up a partial year's return — the classic way a young portfolio
// gets to claim a 400% annual return — cannot happen.
const minAnnualiseDays = 365

// daysPerYear is the denominator for time-value calculations. 365 (not 365.25)
// matches the XIRR convention every spreadsheet uses, so a user checking this
// against Excel gets the same answer.
const daysPerYear = 365

// ComputePerformance produces the period's return figures.
//
// The valuation series is chain-linked day-over-day, which is exact when
// valuations are available at every flow date — and they are, because the
// snapshot job writes every account every day. Sub-periods whose opening value
// is not positive are skipped rather than treated as a return: a return on a
// zero base is undefined, and inventing one (or dividing by zero) is how a
// freshly-funded account ends up reporting an infinite gain.
func ComputePerformance(in PerformanceInput) (Performance, bool) {
	vals := in.Valuations
	if len(vals) < 2 {
		return Performance{}, false
	}

	first, last := vals[0], vals[len(vals)-1]
	perf := Performance{
		Start:      first.Date,
		End:        last.Date,
		Days:       int(last.Date.Sub(first.Date).Hours() / 24),
		StartValue: first.Value,
		EndValue:   last.Value,
	}

	// Only flows strictly after the opening valuation and up to the closing one
	// belong to the measured span. A flow on the opening day is already inside
	// the opening value.
	flows := make([]Flow, 0, len(in.Flows))
	for _, f := range in.Flows {
		if f.Date.After(first.Date) && !f.Date.After(last.Date) {
			flows = append(flows, f)
		}
	}
	sort.Slice(flows, func(i, j int) bool { return flows[i].Date.Before(flows[j].Date) })

	for _, f := range flows {
		perf.NetFlows = perf.NetFlows.Add(f.Amount)
	}
	perf.Gain = perf.EndValue.Sub(perf.StartValue).Sub(perf.NetFlows)

	if twr, ok := timeWeightedReturn(vals, flows); ok {
		perf.TWR = &twr
		if perf.Days >= minAnnualiseDays {
			if ann, ok := annualise(twr, perf.Days); ok {
				perf.Annualised = &ann
			}
		}
	}

	mwr, note := moneyWeightedReturn(perf.StartValue, perf.EndValue, first.Date, last.Date, flows)
	perf.MWR, perf.MWRNote = mwr, note

	return perf, true
}

// timeWeightedReturn chain-links the sub-period returns between consecutive
// valuations.
//
// For each sub-period the flows that landed inside it are removed from the
// closing value before the ratio is taken, so a deposit never reads as growth:
//
//	r = (V_end − flows_in_period) / V_start − 1
//
// This assumes a flow arrives at the END of its sub-period. With daily
// valuations that assumption is worth at most one day of market movement on the
// flow amount — immaterial, and the alternative (assuming the start) would
// overstate returns on deposits, which is the direction that flatters.
func timeWeightedReturn(vals []Valuation, flows []Flow) (decimal.Decimal, bool) {
	one := decimal.NewFromInt(1)
	product := one
	linked := 0

	fi := 0
	for i := 1; i < len(vals); i++ {
		prev, cur := vals[i-1], vals[i]

		var periodFlows decimal.Decimal
		for fi < len(flows) && !flows[fi].Date.After(cur.Date) {
			if flows[fi].Date.After(prev.Date) {
				periodFlows = periodFlows.Add(flows[fi].Amount)
			}
			fi++
		}

		// A sub-period opening at zero or below cannot express a return. Skipping
		// it keeps the rest of the chain honest instead of poisoning the whole
		// product with a division by zero.
		if !prev.Value.IsPositive() {
			continue
		}

		growth := cur.Value.Sub(periodFlows).Div(prev.Value)
		product = product.Mul(growth)
		linked++
	}

	if linked == 0 {
		return decimal.Zero, false
	}
	return product.Sub(one), true
}

// annualise converts a total return over `days` into an annual rate:
// (1+r)^(365/days) − 1.
//
// decimal.Pow handles the fractional exponent in decimal (Ln then ExpTaylor),
// so no float64 enters the calculation. A base at or below zero means a total
// return of −100% or worse, which is either a wiped-out portfolio or a bug
// upstream; either way it returns not-computable rather than a number.
func annualise(totalReturn decimal.Decimal, days int) (decimal.Decimal, bool) {
	if days <= 0 {
		return decimal.Zero, false
	}
	base := decimal.NewFromInt(1).Add(totalReturn)
	if !base.IsPositive() {
		return decimal.Zero, false
	}
	exp := decimal.NewFromInt(daysPerYear).Div(decimal.NewFromInt(int64(days)))
	return base.Pow(exp).Sub(decimal.NewFromInt(1)), true
}

// IRR solver bounds. The bracket is wide enough for any real portfolio
// (−99.99% to +1000% annualised) while staying inside the range where the
// discount factors remain well-conditioned.
var (
	irrLowerBound = decimal.RequireFromString("-0.9999")
	irrUpperBound = decimal.NewFromInt(10)
)

const (
	// irrMaxIterations is a backstop, not the expected exit. Bisection halves an
	// ~11-wide bracket each step, so it reaches irrPrecision in about 27 —
	// reaching 60 would mean something is wrong, not that more work is needed.
	// The bound matters because each iteration discounts every cash flow with
	// decimal Pow, which is exact but not cheap.
	irrMaxIterations = 60
	// irrPrecision is the bracket width at which the rate is settled: 1e-7,
	// four orders finer than the two decimal places a percentage is shown to.
	irrPrecision = "0.0000001"
	// irrTolerance is how close to zero the NPV must get to call it solved
	// early, in currency units.
	irrTolerance = "0.000001"
)

// moneyWeightedReturn solves for the annualised internal rate of return.
//
// The cash-flow series is built from the investor's perspective: the opening
// value is money they had in (negative, an outflow from them), each
// contribution is negative, each withdrawal positive, and the closing value is
// what they get back (positive).
//
// Solved by bisection rather than Newton. Newton is faster but can diverge on
// the irregular, sign-changing series real portfolios produce, and a diverged
// Newton returns a *number* — the exact failure this function must not have.
// Bisection either brackets a root or it does not, and "does not" is reported.
//
// The second return value is a human-readable reason, non-empty only when the
// rate is nil.
func moneyWeightedReturn(
	startValue, endValue decimal.Decimal,
	start, end time.Time,
	flows []Flow,
) (*decimal.Decimal, string) {
	days := int(end.Sub(start).Hours() / 24)
	if days <= 0 {
		return nil, "the period is too short to measure a rate of return"
	}

	type cashFlow struct {
		day    int
		amount decimal.Decimal
	}
	series := []cashFlow{{day: 0, amount: startValue.Neg()}}
	for _, f := range flows {
		d := int(f.Date.Sub(start).Hours() / 24)
		series = append(series, cashFlow{day: d, amount: f.Amount.Neg()})
	}
	series = append(series, cashFlow{day: days, amount: endValue})

	// A series that never changes sign has no root, and forcing one out of it is
	// how nonsense rates get published. This is the "non-convergent" case the UI
	// shows as "not computable".
	var hasPositive, hasNegative bool
	for _, cf := range series {
		if cf.amount.IsPositive() {
			hasPositive = true
		}
		if cf.amount.IsNegative() {
			hasNegative = true
		}
	}
	if !hasPositive || !hasNegative {
		return nil, "the cash flows never change sign, so no rate of return exists"
	}

	npv := func(rate decimal.Decimal) decimal.Decimal {
		base := decimal.NewFromInt(1).Add(rate)
		total := decimal.Zero
		for _, cf := range series {
			exp := decimal.NewFromInt(int64(cf.day)).Div(decimal.NewFromInt(daysPerYear))
			// base is > 0 by construction: the bracket starts above −1.
			total = total.Add(cf.amount.Div(base.Pow(exp)))
		}
		return total
	}

	lo, hi := irrLowerBound, irrUpperBound
	fLo, fHi := npv(lo), npv(hi)
	// No sign change across the bracket means the root (if any) is outside a
	// range that describes any real investment. Refuse rather than extrapolate.
	if fLo.Sign() == fHi.Sign() {
		return nil, "no rate of return between -99.99% and 1000% fits these cash flows"
	}

	tolerance := decimal.RequireFromString(irrTolerance)
	precision := decimal.RequireFromString(irrPrecision)
	two := decimal.NewFromInt(2)

	mid := lo.Add(hi).Div(two)
	for i := 0; i < irrMaxIterations; i++ {
		mid = lo.Add(hi).Div(two)
		fMid := npv(mid)
		// Two exits: the NPV is close enough to zero, or the bracket has closed
		// past the precision anyone can see. The second is what normally fires —
		// on a large portfolio an NPV of a few cents is already exact enough.
		if fMid.Abs().LessThan(tolerance) || hi.Sub(lo).LessThan(precision) {
			break
		}
		if fMid.Sign() == fLo.Sign() {
			lo, fLo = mid, fMid
		} else {
			hi = mid
		}
	}

	rate := mid.Round(6)
	return &rate, ""
}
