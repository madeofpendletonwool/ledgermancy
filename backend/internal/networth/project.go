package networth

import (
	"time"

	"github.com/shopspring/decimal"
)

// Assumptions are the inputs to a projection. Every one is visible and
// adjustable in the UI: a forecast whose workings are hidden is not something
// anyone should plan around.
type Assumptions struct {
	// MonthlySurplus is what is expected to be saved each month, normally the
	// household's trailing average leftover.
	MonthlySurplus decimal.Decimal
	// AnnualReturnRate is the assumed nominal return on invested assets, as a
	// fraction (0.06 = 6%). Applied only to the investment portion.
	AnnualReturnRate decimal.Decimal
	// AnnualDebtPaydown is how much principal is retired per year.
	AnnualDebtPaydown decimal.Decimal
	// Months to project forward.
	Months int
}

// ProjectionPoint is one month of a projection.
type ProjectionPoint struct {
	Month       string          `json:"month"`
	NetWorth    decimal.Decimal `json:"net_worth"`
	Assets      decimal.Decimal `json:"assets"`
	Liabilities decimal.Decimal `json:"liabilities"`
	// Contributed is cumulative money added, kept separate from growth so it
	// is obvious how much of the projection is saving versus assumed return.
	Contributed decimal.Decimal `json:"contributed"`
	Growth      decimal.Decimal `json:"growth"`
}

// Project rolls a starting position forward under a set of assumptions.
//
// This is deliberately a plain compounding model, not a simulation and not an
// AI guess: the user can see every input, and the arithmetic is one line per
// month. It is an illustration of "if things continue like this", which is what
// a projection can honestly claim — nothing here predicts markets.
func Project(start Result, a Assumptions) []ProjectionPoint {
	if a.Months <= 0 {
		return nil
	}

	// A nominal annual rate divided by twelve. Not a compounded monthly
	// equivalent — the difference is immaterial at these horizons and the
	// simpler form is the one a user can check by hand.
	monthlyRate := decimal.Zero
	if a.AnnualReturnRate.IsPositive() {
		monthlyRate = a.AnnualReturnRate.Div(decimal.NewFromInt(12))
	}
	monthlyPaydown := a.AnnualDebtPaydown.Div(decimal.NewFromInt(12))

	// Only invested assets are assumed to grow; cash is not.
	invested := start.Breakdown.Investments
	other := start.AssetsTotal.Sub(invested)
	liabilities := start.LiabilitiesTotal

	contributed := decimal.Zero
	growth := decimal.Zero
	now := time.Now()

	points := make([]ProjectionPoint, 0, a.Months)
	for m := 1; m <= a.Months; m++ {
		// Growth applies to the invested balance before this month's
		// contribution, so a deposit is not credited with a full month of
		// return it did not earn.
		monthGrowth := invested.Mul(monthlyRate)
		invested = invested.Add(monthGrowth)
		growth = growth.Add(monthGrowth)

		if a.MonthlySurplus.IsPositive() {
			invested = invested.Add(a.MonthlySurplus)
			contributed = contributed.Add(a.MonthlySurplus)
		} else {
			// A negative surplus draws down other assets rather than
			// implying shares are sold in some particular order.
			other = other.Add(a.MonthlySurplus)
			contributed = contributed.Add(a.MonthlySurplus)
		}

		// Debt cannot go below zero.
		liabilities = liabilities.Sub(monthlyPaydown)
		if liabilities.IsNegative() {
			liabilities = decimal.Zero
		}

		assets := invested.Add(other)
		month := now.AddDate(0, m, 0)

		points = append(points, ProjectionPoint{
			Month:       month.Format("2006-01"),
			Assets:      assets.Round(2),
			Liabilities: liabilities.Round(2),
			NetWorth:    assets.Sub(liabilities).Round(2),
			Contributed: contributed.Round(2),
			Growth:      growth.Round(2),
		})
	}

	return points
}
