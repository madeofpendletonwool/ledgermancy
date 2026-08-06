package goals

import (
	"github.com/shopspring/decimal"
)

// Where a debt term came from.
//
// TermSourceNone is a real answer and not a zero: "nobody knows this figure" and
// "this figure is zero" mean different things to a payoff schedule, and the UI
// has to render them differently — one asks the user to fill it in, the other
// reports that the debt never pays off. Collapsing them is how a mortgage came
// to display a confident $0.00 APR.
const (
	TermSourceNone   = ""
	TermSourceManual = "manual"
	TermSourcePlaid  = "plaid"
)

// DebtTerms is what a debt costs to carry: what the household typed, falling
// back to what the institution reported, decided PER FIELD.
//
// Manual wins because it is the only figure a human asserted. Plaid's is a
// machine reading that the human has now explicitly contradicted, and a sync
// silently restoring it would be the same class of failure as the bug the
// account_terms table was added to fix — data changing under the user with
// nothing said about it. Manual values survive every sync by construction: they
// live in account_terms, which the Plaid sync path never writes to.
//
// Per field rather than per row: a bank that reports a minimum payment but no
// rate should keep contributing that payment after the household types a rate.
// Taking the whole row from one source would throw away half of what is known.
//
// This lives beside ComputePayoff rather than in the API layer because it is the
// input that function takes, and it now has three consumers: the Net Worth
// page, the Goals page, and the advisor's debt options. A second implementation
// anywhere is a household being told two different things about the same card.
type DebtTerms struct {
	APR       decimal.Decimal
	APRSource string
	// Payment is the monthly payment the schedule assumes. Zero with a
	// TermSourceNone source means unknown, and ComputePayoff will report the
	// debt as never paying off — which is why the source, not the value, is what
	// callers must branch on.
	Payment       decimal.Decimal
	PaymentSource string
}

// MergeDebtTerms resolves the household's terms against the institution's.
//
// It takes loose columns rather than a row type because sqlc generates a
// distinct struct per query, and ListVisibleLiabilities, GetGoalDebtTerms and
// the advisor all need this. A shared helper is the only thing keeping the
// Goals page, the Net Worth page and the advisor from disagreeing about what a
// debt costs.
//
// plaidRate is liabilities.interest_rate_percentage: student loans and mortgages
// report that instead of an apr, which is the fallback the liabilities endpoint
// has always applied.
//
// obligationAmount is the linked bill's amount, and it outranks the typed
// minimum payment. Both are the household's own figure, so neither is more
// authoritative — but only one of them can be edited from two places. Letting
// the bill win means the payoff projection and the bill calendar can never quote
// different numbers for the same payment.
//
// APR IS A PERCENTAGE HERE, NOT A FRACTION: liabilities.apr and
// account_terms.apr both store 22.99 for a 22.99% card. ComputePayoff's
// monthlyRate divides by 1200 on that basis. Passing a fraction through this is
// the trap doc 24 names by name.
func MergeDebtTerms(manualAPR, plaidAPR, plaidRate, obligationAmount, manualPayment, plaidPayment decimal.NullDecimal) DebtTerms {
	var t DebtTerms

	switch {
	case manualAPR.Valid:
		t.APR, t.APRSource = manualAPR.Decimal, TermSourceManual
	case plaidAPR.Valid:
		t.APR, t.APRSource = plaidAPR.Decimal, TermSourcePlaid
	case plaidRate.Valid:
		t.APR, t.APRSource = plaidRate.Decimal, TermSourcePlaid
	default:
		t.APRSource = TermSourceNone
	}

	switch {
	case obligationAmount.Valid:
		t.Payment, t.PaymentSource = obligationAmount.Decimal, TermSourceManual
	case manualPayment.Valid:
		t.Payment, t.PaymentSource = manualPayment.Decimal, TermSourceManual
	case plaidPayment.Valid:
		t.Payment, t.PaymentSource = plaidPayment.Decimal, TermSourcePlaid
	default:
		t.PaymentSource = TermSourceNone
	}

	return t
}
