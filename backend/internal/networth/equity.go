package networth

import "github.com/shopspring/decimal"

// Equity in an asset with a loan secured against it.
//
// # Equity is a DISPLAY figure and must never enter the net-worth sum
//
// ComputeNetWorth already counts the house as an asset and the mortgage as a
// liability, and their difference is therefore already in net worth. Adding
// equity on top would count the asset twice. This function exists so that "I
// own both cars outright" and "I have $30k of equity in my car" are both
// first-class things the UI can say, where today the asset and the loan are
// unrelated rows that never meet.
//
// The invariant is asserted in the tests rather than left as a comment: net
// worth for a household with a linked asset and loan equals net worth with them
// unlinked, to the cent.

// Equity is what is actually owned of an asset, and how far along the loan is.
type Equity struct {
	// Value is the asset's current value and Balance what is still owed
	// against it.
	Value   decimal.Decimal
	Balance decimal.Decimal

	// Owned is Value − Balance. It can be negative — a car loan routinely
	// exceeds the car early on, and hiding that would be the opposite of the
	// point.
	Owned decimal.Decimal

	// PaidFraction is the share of the asset's value that is owned outright,
	// clamped to 0–1 for a progress bar. Zero when the asset is worthless or
	// the loan exceeds it.
	PaidFraction decimal.Decimal

	// Underwater reports owing more than the thing is worth.
	Underwater bool
}

// ComputeEquity derives the equity figures for one asset and its linked loan.
//
// Callers must treat the result as presentation only. Nothing here is summed
// into assets, liabilities or net worth.
func ComputeEquity(value, loanBalance decimal.Decimal) Equity {
	e := Equity{Value: value, Balance: loanBalance}
	e.Owned = value.Sub(loanBalance)
	e.Underwater = e.Owned.IsNegative()

	if value.IsPositive() && e.Owned.IsPositive() {
		e.PaidFraction = e.Owned.Div(value).Round(4)
		if e.PaidFraction.GreaterThan(decimal.NewFromInt(1)) {
			e.PaidFraction = decimal.NewFromInt(1)
		}
	}
	return e
}
