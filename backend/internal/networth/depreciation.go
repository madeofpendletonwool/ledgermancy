package networth

import (
	"fmt"
	"time"

	"github.com/shopspring/decimal"
)

// Vehicle depreciation.
//
// # This produces a PROPOSAL, never a write
//
// Read the type: SuggestVehicleValue returns a Suggestion, and nothing in this
// package writes one. That is the whole design constraint, and it is not
// squeamishness. Silently depreciating a car means net worth moves with no user
// action and no explanation, in an app whose entire pitch is that the numbers
// are honest and checkable. The user is shown the figure, the curve that
// produced it, and the inputs — and then decides.
//
// Bonds are the deliberate exception (see bonds.go) and the reason is
// DETERMINISM, not convenience: a savings bond's value is arithmetic over
// published rates, identical every time anyone computes it. A depreciation
// curve is a generalisation about the used-car market. The moment a valuation
// depends on a judgement it goes back to being a proposal.
//
// # The curve, and where it comes from
//
// The widely published rule of thumb for US vehicles: roughly 20% of value is
// lost in the first year, then roughly 15% of the remaining value each year
// after, leaving a car at about 40% of its original value at five years.
// CARFAX's depreciation guide (https://www.carfax.com/buying/car-depreciation)
// states this shape, and it is consistent across the major used-car guides.
//
//	retained(t) = 0.80 × 0.85^(t−1)     for t ≥ 1
//	retained(t) = 1 − 0.20t             for 0 ≤ t < 1
//
// At five years that gives 0.80 × 0.85⁴ = 41.8%, matching the "about 40%"
// figure. It is a generalisation and the UI says so: a curve the user cannot
// interrogate is indistinguishable from a guess.
//
// # What it is applied to
//
// The last value the USER confirmed, scaled by the ratio of the curve at the
// two ages. Not an original purchase price — the app does not reliably have
// one, and a car entered second-hand never had one here at all. Depreciating
// forward from the last agreed figure also means a user who overrides the
// suggestion has their number respected as the new anchor rather than being
// argued with every month.

const (
	// firstYearLoss and laterYearLoss are the published curve. Named rather
	// than inlined so the UI copy and the arithmetic cannot drift apart.
	firstYearLoss = "0.20"
	laterYearLoss = "0.15"

	// defaultAnnualMileage is the round figure commonly used for US average
	// annual mileage. Only a baseline for the mileage tilt, and the basis text
	// says when it was assumed rather than supplied.
	defaultAnnualMileage = 13500

	// mileageSensitivity is how much of a proportional mileage deviation is
	// carried into value, and mileageCap bounds it. Both are judgements, which
	// is exactly why this whole file produces a proposal: a car driven double
	// the average is worth less, but not half as much.
	mileageSensitivity = "0.20"
	mileageCap         = "0.20"

	// salvageFloor stops the curve from asymptotically claiming a car is worth
	// nothing. An old running vehicle has a floor the exponential does not.
	salvageFloor = "0.05"
)

// VehicleValuation is the input to a depreciation proposal: what the user last
// agreed the car was worth, and what is known about the car.
type VehicleValuation struct {
	// LastValue is the most recent confirmed value, and LastValueAsOf is when
	// it was true.
	LastValue     decimal.Decimal
	LastValueAsOf time.Time

	// ModelYear is the vehicle's model year — the only age anchor the app
	// reliably has.
	ModelYear int

	// Mileage is an odometer reading and MileageAsOf when it was taken. Zero
	// mileage means "not recorded"; the proposal is then curve-only.
	Mileage       int
	MileageAsOf   time.Time
	AnnualMileage int
}

// Suggestion is a proposed revaluation. OK=false is the same refusal shape the
// rest of this package uses — a condition to surface, never a number to fall
// back on.
type Suggestion struct {
	OK     bool
	Reason string

	// Value is the proposed new value. It is a PROPOSAL: writing it requires
	// the user to accept it.
	Value decimal.Decimal
	// Change is Value − LastValue, for "we think this dropped $1,400".
	Change decimal.Decimal

	// Basis explains the figure in full, including that it is an estimate.
	Basis string
}

// SuggestVehicleValue proposes a new value for a vehicle by ageing the last
// confirmed one along the published depreciation curve.
//
// `now` is a parameter so nothing here reads the clock and the result is
// reproducible.
func SuggestVehicleValue(in VehicleValuation, now time.Time) Suggestion {
	now = monthFloor(now)
	var s Suggestion

	if !in.LastValue.IsPositive() {
		s.Reason = "no current value to depreciate from"
		return s
	}
	if in.ModelYear <= 0 {
		s.Reason = "no model year recorded, so the car's age is unknown"
		return s
	}

	// Age from the start of the model year. A model year is not a purchase
	// date, and pretending otherwise would put false precision on the front of
	// the curve where it is steepest.
	born := time.Date(in.ModelYear, time.January, 1, 0, 0, 0, 0, time.UTC)
	ageNow := yearsBetween(born, now)
	ageThen := yearsBetween(born, monthFloor(in.LastValueAsOf))

	if ageNow.LessThanOrEqual(ageThen) {
		s.Reason = "the current value is already up to date"
		return s
	}

	retainedNow := retainedFraction(ageNow)
	retainedThen := retainedFraction(ageThen)
	if !retainedThen.IsPositive() {
		s.Reason = "the car is old enough that the curve no longer says anything useful"
		return s
	}

	value := in.LastValue.Mul(retainedNow).Div(retainedThen)

	basis := fmt.Sprintf(
		"Estimated from a published depreciation curve — about %s%% of value lost in the first year "+
			"and %s%% of the remainder each year after (source: CARFAX's depreciation guide). "+
			"A %d model year is %s years old; the last confirmed value of %s was recorded at %s years old.",
		pct(firstYearLoss), pct(laterYearLoss), in.ModelYear,
		ageNow.StringFixed(1), in.LastValue.StringFixed(2), ageThen.StringFixed(1))

	// Mileage tilt: only meaningful if the odometer reading is at least as
	// recent as the value it is adjusting, otherwise it is already baked in.
	if adj, note, ok := mileageAdjustment(in, now); ok {
		value = value.Mul(adj)
		basis += " " + note
	} else {
		basis += " Mileage was not used: " + note
	}

	// Floor, so the curve cannot claim a running car is worth nothing.
	floor := in.LastValue.Mul(decimal.RequireFromString(salvageFloor))
	if value.LessThan(floor) {
		value = floor
		basis += fmt.Sprintf(" Held at a %s%% floor — an old but running vehicle is worth more than the curve alone suggests.",
			pct(salvageFloor))
	}

	s.OK = true
	s.Value = value.Round(2)
	s.Change = s.Value.Sub(in.LastValue).Round(2)
	s.Basis = basis + " This is an estimate, not a valuation: accept it, or enter your own figure."
	return s
}

// mileageAdjustment tilts the curve for a car driven harder or lighter than
// average. Returns a multiplier, a sentence for the basis, and whether it
// applied at all.
func mileageAdjustment(in VehicleValuation, now time.Time) (decimal.Decimal, string, bool) {
	one := decimal.NewFromInt(1)

	if in.Mileage <= 0 || in.MileageAsOf.IsZero() {
		return one, "no odometer reading is recorded.", false
	}
	if monthFloor(in.MileageAsOf).Before(monthFloor(in.LastValueAsOf)) {
		return one, "the odometer reading predates the last confirmed value, so it is already reflected in it.", false
	}

	annual := in.AnnualMileage
	assumed := false
	if annual <= 0 {
		annual, assumed = defaultAnnualMileage, true
	}

	// Expected mileage for a car of this age, against what it actually reads.
	born := time.Date(in.ModelYear, time.January, 1, 0, 0, 0, 0, time.UTC)
	age := yearsBetween(born, monthFloor(in.MileageAsOf))
	expected := age.Mul(decimal.NewFromInt(int64(annual)))
	if !expected.IsPositive() {
		return one, "the car is too new for an expected-mileage comparison.", false
	}

	actual := decimal.NewFromInt(int64(in.Mileage))
	deviation := actual.Sub(expected).Div(expected)

	cap := decimal.RequireFromString(mileageCap)
	tilt := deviation.Mul(decimal.RequireFromString(mileageSensitivity)).Neg()
	if tilt.GreaterThan(cap) {
		tilt = cap
	}
	if tilt.LessThan(cap.Neg()) {
		tilt = cap.Neg()
	}

	direction := "above"
	if deviation.IsNegative() {
		direction = "below"
	}
	note := fmt.Sprintf(
		"Adjusted %s%% for mileage: %s miles reads %s%% %s the %s expected for its age at %s miles a year%s.",
		tilt.Mul(decimal.NewFromInt(100)).StringFixed(1),
		actual.StringFixed(0),
		deviation.Abs().Mul(decimal.NewFromInt(100)).StringFixed(0), direction,
		expected.StringFixed(0), decimal.NewFromInt(int64(annual)).StringFixed(0),
		map[bool]string{true: " (assumed, no annual mileage recorded)", false: ""}[assumed])

	return one.Add(tilt), note, true
}

// retainedFraction is the published curve: a steep first year, then a declining
// annual rate on the remainder.
func retainedFraction(years decimal.Decimal) decimal.Decimal {
	one := decimal.NewFromInt(1)
	if !years.IsPositive() {
		return one
	}

	first := decimal.RequireFromString(firstYearLoss)
	if years.LessThan(one) {
		// Linear through the first year rather than exponential, so a
		// three-month-old car is not implied to have lost the full first-year
		// drop already.
		return one.Sub(first.Mul(years))
	}

	base := one.Sub(decimal.RequireFromString(laterYearLoss))
	exp := years.Sub(one)
	factor, err := base.PowWithPrecision(exp, powPrecision)
	if err != nil {
		// base is 0.85 and exp is non-negative here, so this cannot happen;
		// returning the first-year figure is a refusal to invent, not a value.
		return one.Sub(first)
	}
	return one.Sub(first).Mul(factor)
}

// yearsBetween is a fractional year count, by whole months so the result does
// not depend on the day a job happened to run.
func yearsBetween(from, to time.Time) decimal.Decimal {
	months := monthsBetween(from, to)
	if months <= 0 {
		return decimal.Zero
	}
	return decimal.NewFromInt(int64(months)).DivRound(decimal.NewFromInt(12), 6)
}

// pct renders a stored fraction ("0.20") as a percentage ("20") for prose.
func pct(fraction string) string {
	return decimal.RequireFromString(fraction).
		Mul(decimal.NewFromInt(100)).StringFixed(0)
}
