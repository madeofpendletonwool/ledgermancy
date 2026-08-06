package networth

import "github.com/shopspring/decimal"

// The assumption defaults, in ONE place.
//
// A household that has never opened the Retirement page has no
// projection_assumptions row, and every surface that projects has to cope with
// that — the retirement endpoint, the advisor, and doc 32's allocator all read
// the same row and all have to answer without one. Before this existed the
// literals were copied per caller, and the copies are the problem: they are
// three statements of the same assumption that can disagree, and when they do,
// two pages quote different projections from the same inputs and nobody can say
// which is right.
//
// These MATCH THE COLUMN DEFAULTS in 00021_projection_assumptions.sql and
// 00055_allocation_planner.sql. The two must not drift: a household that has
// saved its assumptions reads the row, and one that has not reads this, and a
// user should not be able to tell which by the numbers moving when they first
// press save.

// AssumptionDefaults are the rates used until a household saves its own.
//
// Deliberately NOT a dbgen row: this package is the projection arithmetic and
// has no business knowing the storage layer. Each caller maps these onto
// whatever shape it holds.
type AssumptionDefaults struct {
	// RealReturnRate, InflationRate and WithdrawalRate are FRACTIONS (0.05 = 5%).
	RealReturnRate decimal.Decimal
	InflationRate  decimal.Decimal
	WithdrawalRate decimal.Decimal
	// CollegeInflationRate is a PERCENT (5.50 = 5.5%/yr), matching how the
	// column stores it. College costs have run well above general CPI, which is
	// why it is a separate figure rather than InflationRate.
	CollegeInflationRate decimal.Decimal
}

// DefaultAssumptions returns the defaults. A function rather than a package-level
// var so a caller cannot mutate the shared value out from under everybody else.
func DefaultAssumptions() AssumptionDefaults {
	return AssumptionDefaults{
		RealReturnRate: decimal.RequireFromString("0.05"),
		InflationRate:  decimal.RequireFromString("0.03"),
		// 4% is the convention, not a law, which is exactly why it is editable.
		WithdrawalRate:       decimal.RequireFromString("0.04"),
		CollegeInflationRate: decimal.RequireFromString("5.5"),
	}
}
