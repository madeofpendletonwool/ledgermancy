package goals

import (
	"time"

	"github.com/shopspring/decimal"
)

// payoffHorizonMonths bounds the amortization simulation at 100 years.
//
// The loop cannot run away once the non-amortizing guard has passed — the
// balance strictly decreases every month — but "strictly decreases" and
// "decreases usefully" are not the same thing. A $500,000 balance at 5% accrues
// $2,083.33 a month; pay $2,083.34 and it does amortize, in roughly four
// million months. Nothing past this horizon is a payoff plan, so it is reported
// as NeverPaysOff rather than as a number nobody should read.
const payoffHorizonMonths = 1200

// PayoffFeasibility is the computed standing of one debt-payoff goal. It is the
// debt-side counterpart to Feasibility: same "money is exact decimal, the model
// only phrases the result" contract, different arithmetic.
type PayoffFeasibility struct {
	// Balance is what is still owed, and MonthlyPayment what is being put
	// against it — echoed back so a caller can render the sentence the figures
	// came from without re-reading the liability.
	Balance        decimal.Decimal
	MonthlyPayment decimal.Decimal
	// MonthlyInterest is this month's interest at the current balance. It is the
	// number that makes NeverPaysOff make sense to a human: "at $50/month this
	// card is never paid off — the interest alone is $61/month".
	MonthlyInterest decimal.Decimal

	// NeverPaysOff is true when the payment cannot retire the debt: it is at or
	// below the monthly interest, or non-positive, or payoff lies beyond
	// payoffHorizonMonths. When set, Months, TotalInterest and PayoffDate are
	// zero — there is no schedule to report, and reporting a large number
	// instead of "never" would be the wrong answer dressed as an answer.
	NeverPaysOff bool
	// Months is how many payments it takes, counting the final partial one.
	Months int
	// TotalInterest is the interest paid across the whole schedule.
	TotalInterest decimal.Decimal
	// PayoffDate is now plus Months months; nil when the debt never pays off or
	// is already clear.
	PayoffDate *time.Time

	// OpenEnded is true when the goal has no target date: there is a payoff date
	// but no deadline to miss, so RequiredMonthly is zero and OnTrack tracks
	// only whether the debt amortizes at all.
	OpenEnded bool
	// MonthsLeft is whole months from now to the target date, floored at 1 — the
	// same convention Compute uses.
	MonthsLeft int
	// RequiredMonthly is the smallest whole-cent payment that clears the balance
	// within MonthsLeft. Zero when open-ended, achieved, or unreachable.
	RequiredMonthly decimal.Decimal
	// TargetReachable is false when no payment inside the search bound hits the
	// target date. With the bound set to "clear it in one month" this is
	// unreachable in practice; the flag exists so a caller never renders an
	// unconverged figure as though it were a plan.
	TargetReachable bool
	// OnTrack reports whether the current payment clears the debt by the target
	// date (or, open-ended, whether it clears it at all).
	OnTrack bool
	// Achieved is true once the balance is at or below zero — the debt is paid.
	Achieved bool
}

// ComputePayoff derives a debt-payoff goal's standing by amortizing the balance
// forward one month at a time.
//
// NOTE ON GOAL SEMANTICS. A payoff goal reuses the goals table, whose
// target_amount is documented as "a positive target". For a payoff goal that
// target is *the balance to eliminate* — captured from the liability when the
// goal is created — not zero. Progress is therefore
// `target_amount − current balance`, which rises from 0 toward the target as the
// debt is paid, and the positive-target convention and the progress bar both
// still hold. A reader arriving from Compute will otherwise assume the savings
// reading, in which "current" is money accumulated rather than debt retired.
//
// apr is a PERCENTAGE, not a fraction: liabilities.apr stores 6.25 for 6.25%
// (see 00004_investments_liabilities.sql). The /100 below is mandatory —
// dropping it inflates every rate by 100× and reports almost every debt as
// never paying off.
//
//   - monthlyRate = apr / 100 / 12
//   - each month:  interest = round(balance × monthlyRate, 2)
//     balance  = balance + interest − payment
//   - payment ≤ interest → the balance never falls: NeverPaysOff
//   - zero or absent APR degrades to balance / payment, correct for an
//     interest-free debt
func ComputePayoff(balance, apr, monthlyPayment decimal.Decimal, targetDate *time.Time, now time.Time) PayoffFeasibility {
	rate := monthlyRate(apr)

	f := PayoffFeasibility{
		Balance:        balance,
		MonthlyPayment: monthlyPayment,
		OpenEnded:      targetDate == nil,
	}

	if !balance.IsPositive() {
		// Nothing owed: paid off, and no schedule to describe.
		f.Achieved = true
		f.OnTrack = true
		return f
	}

	f.MonthlyInterest = balance.Mul(rate).Round(2)

	months, interest, ok := simulatePayoff(balance, rate, monthlyPayment)
	if !ok {
		f.NeverPaysOff = true
	} else {
		f.Months = months
		f.TotalInterest = interest
		payoff := now.AddDate(0, months, 0)
		f.PayoffDate = &payoff
	}

	if targetDate == nil {
		// No deadline: amortizing at all is the whole of being on track.
		f.OnTrack = ok
		return f
	}

	f.MonthsLeft = wholeMonthsBetween(now, *targetDate)
	f.OnTrack = ok && months <= f.MonthsLeft

	required, reachable := requiredPayment(balance, rate, f.MonthsLeft)
	f.TargetReachable = reachable
	if reachable {
		f.RequiredMonthly = required
	}
	return f
}

// monthlyRate converts a stored APR percentage into a monthly fraction. A
// negative or absent APR is treated as interest-free rather than as a discount.
func monthlyRate(apr decimal.Decimal) decimal.Decimal {
	if !apr.IsPositive() {
		return decimal.Zero
	}
	return apr.Div(decimal.NewFromInt(100)).Div(decimal.NewFromInt(12))
}

// simulatePayoff amortizes balance forward at the given payment, returning the
// number of payments and the interest paid across them. ok is false when the
// debt never retires — the caller turns that into NeverPaysOff rather than a
// month count.
//
// Interest is rounded to the cent each month because that is what a lender
// posts: a statement charges whole cents, and compounding an unrounded figure
// would drift from the balance the user can see on their card.
func simulatePayoff(balance, rate, payment decimal.Decimal) (months int, totalInterest decimal.Decimal, ok bool) {
	totalInterest = decimal.Zero
	if !payment.IsPositive() {
		return 0, totalInterest, false
	}
	// The non-amortizing case, checked once up front: a payment at or below the
	// interest accrual leaves the balance flat or growing forever. Rounded, so
	// the comparison matches the interest actually charged.
	if payment.LessThanOrEqual(balance.Mul(rate).Round(2)) {
		return 0, totalInterest, false
	}

	for months = 1; months <= payoffHorizonMonths; months++ {
		interest := balance.Mul(rate).Round(2)
		totalInterest = totalInterest.Add(interest)
		balance = balance.Add(interest).Sub(payment)
		if !balance.IsPositive() {
			// The final payment covers whatever is left; the overshoot is not
			// money paid, so only the interest accrued is counted.
			return months, totalInterest, true
		}
	}
	return 0, decimal.Zero, false
}

// requiredPayment bisects for the smallest whole-cent monthly payment that
// clears the balance within months. Bisection over the same simulation the rest
// of the file uses is deliberate: a closed-form annuity would be a second,
// separately-wrong model of the debt, whereas this is verifiable by feeding the
// answer back through simulatePayoff.
//
// The upper bound is one month's interest plus the whole balance, which by
// construction retires the debt in a single payment.
func requiredPayment(balance, rate decimal.Decimal, months int) (decimal.Decimal, bool) {
	if months < 1 {
		return decimal.Zero, false
	}

	hi := ceilCents(balance.Add(balance.Mul(rate)))
	if m, _, ok := simulatePayoff(balance, rate, hi); !ok || m > months {
		return decimal.Zero, false
	}

	// lo is always known-insufficient, hi always known-sufficient, and both stay
	// on cent boundaries, so the gap halves until it is exactly one cent.
	lo := decimal.Zero
	cent := decimal.New(1, -2)
	two := decimal.NewFromInt(2)
	for hi.Sub(lo).GreaterThan(cent) {
		mid := ceilCents(lo.Add(hi).Div(two))
		if m, _, ok := simulatePayoff(balance, rate, mid); ok && m <= months {
			hi = mid
		} else {
			lo = mid
		}
	}
	return hi, true
}

// ceilCents rounds up to the next whole cent. A required payment must never be
// rounded down: a cent short is a payment that misses the date.
func ceilCents(d decimal.Decimal) decimal.Decimal {
	return d.Shift(2).Ceil().Shift(-2)
}
