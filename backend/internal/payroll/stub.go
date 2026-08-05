package payroll

import (
	"github.com/shopspring/decimal"
)

// Line is one deduction or employer contribution on a stub, free of any
// database or HTTP type so the arithmetic below can be tested against
// hand-computed fixtures without a Postgres.
type Line struct {
	Category Category
	Label    string
	// Amount is always the POSITIVE magnitude. Direction comes from Category
	// and IsEmployer, never from a sign — see the column comment in the
	// migration for why signing deductions negative was rejected.
	Amount decimal.Decimal
	// YTDAmount is the year-to-date figure the employer printed beside the
	// line, when the stub carried one. Zero-valued and NotValid when it did not.
	YTDAmount decimal.NullDecimal
	PreTax    bool
	// IsEmployer is money added on top of gross rather than taken out of it.
	IsEmployer bool
}

// Stub is one pay period's figures.
type Stub struct {
	Gross decimal.Decimal
	Net   decimal.Decimal
	Lines []Line
}

// --------------------------------------------------------------------------
// The balance check
// --------------------------------------------------------------------------

// Residual is gross − employee deductions − net: what the stub fails to account
// for. Zero means the stub reconciles.
//
// Employer lines are excluded, and that is the whole reason paystub_lines has
// an is_employer column. A 401(k) match is money the employer paid on top of
// gross; including it here would make every stub with a match appear short by
// the size of the match, and doc 23's own verification rule — "gross − Σ(
// deduction lines) = net must hold for every confirmed paystub" — would fail on
// the most common stub in America.
func (s Stub) Residual() decimal.Decimal {
	out := s.Gross.Sub(s.Net)
	for _, l := range s.Lines {
		if l.IsEmployer {
			continue
		}
		out = out.Sub(l.Amount)
	}
	return out
}

// BalanceTolerance is how far off a stub may be and still be accepted.
//
// One cent, not zero. Payroll systems round each deduction to the cent
// independently, so a stub with a dozen lines can legitimately be a penny out
// without anything being wrong, and refusing those would mean refusing real
// paystubs. Anything larger is a mis-entry: a missed line, a transposed digit,
// a YTD figure typed into a period column. Storing one of those silently is
// what poisons every downstream figure, which is why this is a hard gate on
// confirmation rather than a warning.
var BalanceTolerance = decimal.NewFromFloat(0.01)

// Balances reports whether the stub reconciles within BalanceTolerance.
func (s Stub) Balances() bool {
	return s.Residual().Abs().LessThanOrEqual(BalanceTolerance)
}

// --------------------------------------------------------------------------
// Derived figures
// --------------------------------------------------------------------------

// TaxTotal is the sum of the genuine tax lines: federal, state and local income
// tax plus the FICA pair and the additional Medicare surtax.
//
// Premiums and retirement deferrals are excluded even though both leave the
// paycheck, because the number this feeds is compared against published tax
// rates. Folding a health premium into it would produce an "effective tax rate"
// that is not one, and that reads as far worse news than it is.
func (s Stub) TaxTotal() decimal.Decimal {
	total := decimal.Zero
	for _, l := range s.Lines {
		if l.IsEmployer {
			continue
		}
		if SpecFor(l.Category).IsTax {
			total = total.Add(l.Amount)
		}
	}
	return total
}

// EffectiveTaxRate is TaxTotal / Gross as a fraction, and false when gross is
// zero.
//
// The false is not a formality. A zero-gross stub is a real thing — a
// correction run, or an unpaid leave period with benefits still deducted — and
// dividing by it would produce either a panic or an infinity that renders as
// something ridiculous. Callers say "not enough information" instead.
func (s Stub) EffectiveTaxRate() (decimal.Decimal, bool) {
	if !s.Gross.IsPositive() {
		return decimal.Zero, false
	}
	return s.TaxTotal().Div(s.Gross), true
}

// EmployerTotal is everything the employer added on top of gross: the 401(k)
// match, employer-paid premiums, tuition assistance.
func (s Stub) EmployerTotal() decimal.Decimal {
	total := decimal.Zero
	for _, l := range s.Lines {
		if l.IsEmployer {
			total = total.Add(l.Amount)
		}
	}
	return total
}

// TotalCompensation is gross plus everything the employer paid on top of it.
// The number most people have never seen, and usually 10–25% above the salary
// they would quote.
func (s Stub) TotalCompensation() decimal.Decimal {
	return s.Gross.Add(s.EmployerTotal())
}

// FederalTaxableWages is gross less the pre-tax deductions that reduce federal
// taxable wages — the basis for box 1 of a W-2.
//
// Both conditions matter and neither is redundant: the line must be marked
// pre-tax on the stub AND its category must be one that actually reduces the
// federal base. A Roth 401(k) marked pre-tax by a mis-import still reduces
// nothing, because ReducesNothing is a fact about Roth money rather than a
// default somebody can override.
func (s Stub) FederalTaxableWages() decimal.Decimal {
	return s.grossLessPreTax(func(b WageBasis) bool {
		return b == ReducesFederalOnly || b == ReducesFederalAndFICA
	})
}

// FICAWages is gross less the Section 125 pre-tax deductions only — the basis
// for boxes 3 and 5 of a W-2.
//
// This is where a 401(k) deferral does NOT come out, and the reason a real W-2
// shows a bigger number in box 3 than in box 1 for anybody who contributes.
func (s Stub) FICAWages() decimal.Decimal {
	return s.grossLessPreTax(func(b WageBasis) bool {
		return b == ReducesFederalAndFICA
	})
}

func (s Stub) grossLessPreTax(reduces func(WageBasis) bool) decimal.Decimal {
	out := s.Gross
	for _, l := range s.Lines {
		if l.IsEmployer || !l.PreTax {
			continue
		}
		if reduces(SpecFor(l.Category).Basis) {
			out = out.Sub(l.Amount)
		}
	}
	// A stub whose pre-tax deductions exceed gross is not something to report a
	// negative wage base for. It happens on a zero- or near-zero-gross
	// correction run where benefits were still taken, and a negative box 1 is
	// meaningless — clamp rather than propagate nonsense into a summary
	// somebody hands to an accountant.
	if out.IsNegative() {
		return decimal.Zero
	}
	return out
}

// GroupTotal sums the employee deductions in one display group ("tax",
// "retirement", "health", "insurance", "other"), which is what the paycheck
// breakdown chart is drawn from.
func (s Stub) GroupTotal(group string) decimal.Decimal {
	total := decimal.Zero
	for _, l := range s.Lines {
		if l.IsEmployer {
			continue
		}
		if SpecFor(l.Category).Group == group {
			total = total.Add(l.Amount)
		}
	}
	return total
}

// BreakdownGroups is the order the paycheck breakdown is drawn in: gross flows
// left to right through each band and what survives is net.
var BreakdownGroups = []string{"tax", "retirement", "health", "insurance", "other"}

// Band is one segment of the "where your paycheck went" breakdown.
type Band struct {
	Group  string
	Amount decimal.Decimal
}

// Breakdown returns the bands plus the take-home remainder, in draw order.
//
// The bands are guaranteed to sum with net back to gross whenever the stub
// balances, which is what lets the chart be drawn without a reconciling fudge
// segment. An unbalanced stub cannot reach here: confirmation rejects it, and
// only confirmed stubs are ever charted.
func (s Stub) Breakdown() ([]Band, decimal.Decimal) {
	bands := make([]Band, 0, len(BreakdownGroups))
	for _, g := range BreakdownGroups {
		total := s.GroupTotal(g)
		if total.IsZero() {
			continue
		}
		bands = append(bands, Band{Group: g, Amount: total})
	}
	return bands, s.Net
}
