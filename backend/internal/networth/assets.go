package networth

import (
	"context"
	"time"

	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// Glue between the stored asset rows and the pure valuation functions.
//
// Everything below converts nullable database columns into the value types
// bonds.go and depreciation.go take. It deliberately does no arithmetic: the
// figures are produced by the pure functions, which is what makes them
// reproducible and testable without a database.

// LoadBondRates reads the published rate table.
//
// The table is ~100 rows and grows twice a year, so it is read whole. A bond
// valuation needs every period it spans, and paging that would only add round
// trips.
func LoadBondRates(ctx context.Context, q *dbgen.Queries) (*BondRates, error) {
	rows, err := q.ListSavingsBondRates(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]BondRate, 0, len(rows))
	for _, r := range rows {
		out = append(out, BondRate{
			Series:    r.Series,
			Start:     r.PeriodStart,
			Fixed:     r.FixedRate,
			Inflation: r.InflationRate,
			SourceURL: r.SourceUrl,
		})
	}
	return NewBondRates(out), nil
}

// BondFrom builds a Bond from the nullable detail columns.
//
// ok=false means the row does not describe a bond at all — no series, no issue
// date, or nothing paid for it. That is a different statement from "this bond
// could not be valued", which is ValueBond's job to say and to explain.
func BondFrom(
	series *string,
	issueDate *time.Time,
	purchasePrice, faceValue, couponRate decimal.NullDecimal,
	maturityDate *time.Time,
) (Bond, bool) {
	if series == nil || *series == "" || issueDate == nil || !purchasePrice.Valid {
		return Bond{}, false
	}
	b := Bond{
		Series:        *series,
		IssueDate:     *issueDate,
		PurchasePrice: purchasePrice.Decimal,
		CouponRate:    couponRate,
		MaturityDate:  maturityDate,
	}
	if faceValue.Valid {
		b.FaceValue = faceValue.Decimal
	}
	return b, true
}

// BondFromAssetRow is the ListBondAssets flavour, used by the revaluation job.
func BondFromAssetRow(r dbgen.ListBondAssetsRow) (Bond, bool) {
	return BondFrom(r.BondSeries, r.IssueDate, r.PurchasePrice, r.FaceValue, r.CouponRate, r.MaturityDate)
}

// BondFromDetail is the asset_details flavour, used by the API.
func BondFromDetail(d dbgen.AssetDetail) (Bond, bool) {
	return BondFrom(d.BondSeries, d.IssueDate, d.PurchasePrice, d.FaceValue, d.CouponRate, d.MaturityDate)
}

// VehicleFromDetail builds a depreciation input from an asset and its detail
// row. ok=false means there is no model year, without which the car has no age
// and the curve says nothing.
//
// The odometer reading is dated to the detail row's last update rather than
// carrying its own timestamp: it is entered by hand, so "when the user last
// touched this" is the honest answer for when it was true.
func VehicleFromDetail(value decimal.Decimal, valueAsOf time.Time, d dbgen.AssetDetail) (VehicleValuation, bool) {
	if d.Year == nil || *d.Year <= 0 {
		return VehicleValuation{}, false
	}
	v := VehicleValuation{
		LastValue:     value,
		LastValueAsOf: valueAsOf,
		ModelYear:     int(*d.Year),
	}
	if d.Mileage != nil && *d.Mileage > 0 {
		v.Mileage = int(*d.Mileage)
		v.MileageAsOf = d.UpdatedAt
	}
	if d.AnnualMileage != nil && *d.AnnualMileage > 0 {
		v.AnnualMileage = int(*d.AnnualMileage)
	}
	return v, true
}

// StalenessThreshold is how old a recorded value gets before the app offers to
// update it.
//
// Twelve months for both real estate and vehicles. Shorter would nag about
// noise — neither moves enough in a quarter to be worth a push — and longer
// lets a year-and-a-half-old figure sit in net worth unchallenged, which is the
// drift this whole feature exists to stop.
const StalenessThreshold = 12 * 30 * 24 * time.Hour

// StaleBefore is the cutoff date passed to ListStaleAssets.
func StaleBefore(now time.Time) time.Time {
	return monthFloor(now).AddDate(-1, 0, 0)
}
