package api

import (
	"github.com/shopspring/decimal"
)

// Where a debt term came from.
//
// termSourceNone is a real answer and not a zero: "nobody knows this figure" and
// "this figure is zero" mean different things to a payoff schedule, and the UI
// has to render them differently — one asks the user to fill it in, the other
// reports that the debt never pays off. Collapsing them is how a mortgage came
// to display a confident $0.00 APR.
const (
	termSourceNone   = ""
	termSourceManual = "manual"
	termSourcePlaid  = "plaid"
)

// debtTerms is what a debt costs to carry: what the household typed, falling
// back to what the institution reported, decided PER FIELD.
//
// Manual wins because it is the only figure a human asserted. Plaid's is a
// machine reading that the human has now explicitly contradicted, and a sync
// silently restoring it would be the same class of failure as the bug this
// package's account_terms table was added to fix — data changing under the user
// with nothing said about it. Manual values survive every sync by construction:
// they live in account_terms, which the Plaid sync path never writes to.
//
// Per field rather than per row: a bank that reports a minimum payment but no
// rate should keep contributing that payment after the household types a rate.
// Taking the whole row from one source would throw away half of what is known.
type debtTerms struct {
	APR       decimal.Decimal
	APRSource string
	// Payment is the monthly payment the schedule assumes. Zero with a
	// termSourceNone source means unknown, and ComputePayoff will report the
	// debt as never paying off — which is why the source, not the value, is what
	// callers must branch on.
	Payment       decimal.Decimal
	PaymentSource string
}

// mergeDebtTerms resolves the household's terms against the institution's.
//
// It takes loose columns rather than a row type because sqlc generates a
// distinct struct per query, and both ListVisibleLiabilities and
// GetGoalDebtTerms need this. A shared helper is the only thing keeping the
// Goals page and the Net Worth page from disagreeing about what a debt costs.
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
func mergeDebtTerms(manualAPR, plaidAPR, plaidRate, obligationAmount, manualPayment, plaidPayment decimal.NullDecimal) debtTerms {
	var t debtTerms

	switch {
	case manualAPR.Valid:
		t.APR, t.APRSource = manualAPR.Decimal, termSourceManual
	case plaidAPR.Valid:
		t.APR, t.APRSource = plaidAPR.Decimal, termSourcePlaid
	case plaidRate.Valid:
		t.APR, t.APRSource = plaidRate.Decimal, termSourcePlaid
	default:
		t.APRSource = termSourceNone
	}

	switch {
	case obligationAmount.Valid:
		t.Payment, t.PaymentSource = obligationAmount.Decimal, termSourceManual
	case manualPayment.Valid:
		t.Payment, t.PaymentSource = manualPayment.Decimal, termSourceManual
	case plaidPayment.Valid:
		t.Payment, t.PaymentSource = plaidPayment.Decimal, termSourcePlaid
	default:
		t.PaymentSource = termSourceNone
	}

	return t
}

// isDebtAccountType is the single definition of "this account is money owed".
//
// It mirrors ComputeNetWorth's type split (internal/db/queries/networth.sql) and
// frontend/src/lib/money.ts isLiability(). All three must stay in step: the bug
// this replaced was a fourth, different definition — "Plaid served a liabilities
// row for it" — which answered a question nobody was asking and left a household
// with three debts unable to create a single payoff goal.
func isDebtAccountType(accountType string) bool {
	return accountType == "credit" || accountType == "loan"
}

// debtKindLabel is the word a debt is known by in the UI.
//
// Derived from the account rather than from liabilities.kind, for two reasons:
// that column does not exist for most debts (no liabilities row), and its CHECK
// permits only credit/student/mortgage, so it cannot say "auto" or "line of
// credit" at all. Plaid's subtype can, and is what the Accounts page already
// displays, so the two now agree.
// aprDecimals is how many places a rate should be rendered to for an account of
// this type. Amortizing loans get three because note rates are quoted in
// eighths — 6.775, 5.875 — and rounding one to two places prints a number the
// household cannot find on their statement. Cards get two: nobody quotes a card
// at 18.990%.
func aprDecimals(accountType string) int32 {
	if accountType == "loan" {
		return 3
	}
	return 2
}

func debtKindLabel(accountType string, subtype *string) string {
	if subtype != nil && *subtype != "" {
		return *subtype
	}
	return accountType
}
