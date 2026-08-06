package api

import (
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/goals"
)

// Debt terms resolution lives in internal/goals, beside the ComputePayoff that
// consumes it: doc 24's advisor needs the same precedence, and a second copy of
// it is a household being quoted two different APRs for the same card. These
// aliases keep the API layer's existing spelling.
type debtTerms = goals.DebtTerms

const (
	termSourceNone   = goals.TermSourceNone
	termSourceManual = goals.TermSourceManual
	termSourcePlaid  = goals.TermSourcePlaid
)

func mergeDebtTerms(manualAPR, plaidAPR, plaidRate, obligationAmount, manualPayment, plaidPayment decimal.NullDecimal) debtTerms {
	return goals.MergeDebtTerms(manualAPR, plaidAPR, plaidRate, obligationAmount, manualPayment, plaidPayment)
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
