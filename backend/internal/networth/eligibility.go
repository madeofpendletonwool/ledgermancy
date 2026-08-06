package networth

import (
	"strconv"

	"github.com/shopspring/decimal"
)

// Roth IRA eligibility, and why it is a separate file from limits.go.
//
// AnnualLimitFor answers exactly one question: "what is the annual cap for this
// account type at this age". It has no idea whether the household may
// contribute AT ALL. A Roth IRA has a MAGI phase-out, and a household above it
// that is shown $7,500 of headroom is being handed a plan that is not merely
// optimistic but not legal to execute — presented with the same confidence as
// everything else on the page. A CAP IS NOT PERMISSION.
//
// This file follows limits.go's shape for limits.go's reasons: a versioned Go
// map reviewed when the app is upgraded, and an unconfigured year that returns
// ok=false rather than quietly using an adjacent year's thresholds. The
// flattering assumption is the one that gets shipped by accident, so the
// default here is never "eligible".

// Filing statuses, matching the CHECK on households.filing_status (00054).
// Married-separate is its own case and a brutal one: the phase-out runs from $0
// to $10,000 and has never been indexed to inflation, so almost every household
// filing separately is ineligible for a direct Roth contribution. Folding it
// into "single" would be the single most wrong shortcut available here.
const (
	FilingSingle          = "single"
	FilingMarriedJoint    = "married_joint"
	FilingMarriedSeparate = "married_separate"
	FilingHeadOfHousehold = "hoh"
)

// Eligibility statuses. These strings travel to the client and into chat tool
// results, so they are a wire format.
const (
	// EligibilityEligible: below the phase-out, the full cap applies.
	EligibilityEligible = "eligible"
	// EligibilityPhasedOut: inside the range, with a REDUCED limit computed from
	// the documented linear formula. The phase-out is a ramp, not a cliff.
	EligibilityPhasedOut = "phased_out"
	// EligibilityIneligible: above the range. The true statement is "ineligible
	// for a DIRECT Roth contribution" — see the backdoor note below.
	EligibilityIneligible = "ineligible"
	// EligibilityUnknown: no MAGI on file, no filing status, or an unconfigured
	// tax year. Never a synonym for eligible.
	EligibilityUnknown = "unknown"
)

// PhaseOutRange is one (tax year, filing status) Roth MAGI phase-out band.
//
// Below Start the full contribution is allowed; at or above End none is. Inside
// the band the allowed contribution falls linearly, rounded UP to the nearest
// $10 and floored at $200 while any contribution is allowed at all — both of
// those are the IRS's own rounding rules for this calculation, not a
// convenience, and dropping them produces a figure a few dollars away from what
// the household may actually contribute.
type PhaseOutRange struct {
	Start decimal.Decimal
	End   decimal.Decimal
}

// rothPhaseOuts is the versioned table, keyed by tax year then filing status.
//
// Same rule as taxYearLimits: add a year when the IRS publishes it, and do not
// edit a past year in place. An eligibility check run against history should
// still see what was true then.
//
// Head of household uses the single band; the IRS publishes them together
// ("single, head of household, or married filing separately AND you did not
// live with your spouse"). Married-separate below is the "lived with your
// spouse" band, which is the case that actually applies to a household filing
// separately in the app's sense.
var rothPhaseOuts = map[int]map[string]PhaseOutRange{
	2025: {
		FilingSingle:          {Start: decimal.NewFromInt(150000), End: decimal.NewFromInt(165000)},
		FilingHeadOfHousehold: {Start: decimal.NewFromInt(150000), End: decimal.NewFromInt(165000)},
		FilingMarriedJoint:    {Start: decimal.NewFromInt(236000), End: decimal.NewFromInt(246000)},
		FilingMarriedSeparate: {Start: decimal.Zero, End: decimal.NewFromInt(10000)},
	},
	2026: {
		FilingSingle:          {Start: decimal.NewFromInt(153000), End: decimal.NewFromInt(168000)},
		FilingHeadOfHousehold: {Start: decimal.NewFromInt(153000), End: decimal.NewFromInt(168000)},
		FilingMarriedJoint:    {Start: decimal.NewFromInt(242000), End: decimal.NewFromInt(252000)},
		FilingMarriedSeparate: {Start: decimal.Zero, End: decimal.NewFromInt(10000)},
	},
}

// LatestEligibilityYear is the most recent year in the phase-out table, for the
// message shown when the running year is missing.
func LatestEligibilityYear() int {
	latest := 0
	for y := range rothPhaseOuts {
		if y > latest {
			latest = y
		}
	}
	return latest
}

// Eligibility is the answer for one account: a status, and — when the household
// is inside the phase-out — the reduced limit that actually applies.
type Eligibility struct {
	// Status is one of the Eligibility* constants above.
	Status string
	// Limit is the contribution actually allowed. It equals the full cap when
	// Status is eligible, the linearly reduced figure when phased_out, and zero
	// when ineligible. It is the full cap when unknown — paired with Note, which
	// says the check could not be made.
	Limit decimal.Decimal
	// Note is rendered verbatim beside the figure. It always says what was
	// assumed, because every one of these statuses is a claim about what the
	// household may legally do.
	Note string
}

// The IRS's rounding for a phased-out Roth limit: round the computed amount UP
// to the next $10, and if the result is above zero but under $200, it is $200.
var (
	rothRoundTo   = decimal.NewFromInt(10)
	rothMinimum   = decimal.NewFromInt(200)
	oneHundredPct = decimal.NewFromInt(100)
)

// EligibilityFor reports whether a household may contribute to an account of a
// given tax treatment, and how much.
//
// The parameters that can be absent are absent-able on purpose:
//
//   - filingStatus == "" — doc 31's column is nullable and a household that has
//     not said cannot be assumed into a band.
//   - magi is a NullDecimal — the app cannot compute a MAGI, so it is typed in
//     or it is missing.
//
// Either one missing returns `unknown` WITH the full limit, so the projection
// still runs, plus a Note the caller must render. That combination is
// deliberate: refusing to project would make the planner useless for the
// households that have not filled in an optional field, and projecting silently
// would be a claim nobody checked.
//
// fullLimit is the cap AnnualLimitFor already computed for this account and age.
// This function never re-derives it — two implementations of the IRA cap is
// exactly the drift limits.go exists to prevent.
func EligibilityFor(
	treatment string, year int, filingStatus string, magi decimal.NullDecimal, fullLimit decimal.Decimal,
) (Eligibility, bool) {
	switch treatment {
	case "roth_ira":
		// The case this file exists for; handled below.
	case "hsa":
		// An HSA contribution requires HDHP coverage for the month, which the
		// app cannot see: there is no insurance data anywhere in the schema and
		// no honest way to infer it from transactions. So an HSA is ALWAYS
		// unknown, and says so, rather than being reported as eligible because
		// nothing contradicted it.
		return Eligibility{
			Status: EligibilityUnknown,
			Limit:  fullLimit,
			Note: "An HSA contribution requires HDHP coverage for the month. " +
				"Ledgermancy has no insurance data, so this is not checked — the cap below assumes you are covered.",
		}, true
	default:
		// A traditional IRA has a DEDUCTIBILITY phase-out, not a contribution
		// one: the contribution is always allowed, only the tax deduction is
		// limited. Reporting it as an eligibility limit would be wrong in the
		// unflattering direction, which is still wrong. 401(k)s, taxable
		// accounts, 529s and the custodial treatments have no MAGI test at all.
		return Eligibility{Status: EligibilityEligible, Limit: fullLimit}, true
	}

	bands, ok := rothPhaseOuts[year]
	if !ok {
		return Eligibility{
			Status: EligibilityUnknown,
			Limit:  fullLimit,
			Note: "Roth income limits for " + strconv.Itoa(year) + " aren't configured yet (we have them through " +
				strconv.Itoa(LatestEligibilityYear()) + "), so eligibility could not be checked.",
		}, false
	}
	band, ok := bands[filingStatus]
	if !ok {
		return Eligibility{
			Status: EligibilityUnknown,
			Limit:  fullLimit,
			Note: "Set your filing status on the household profile to check Roth eligibility. " +
				"Until then this assumes you're eligible to contribute.",
		}, true
	}
	if !magi.Valid {
		return Eligibility{
			Status: EligibilityUnknown,
			Limit:  fullLimit,
			Note: "Enter your modified AGI on the household profile to check Roth eligibility. " +
				"Until then this assumes you're eligible to contribute.",
		}, true
	}

	income := magi.Decimal
	switch {
	case income.LessThan(band.Start):
		return Eligibility{Status: EligibilityEligible, Limit: fullLimit}, true

	case income.GreaterThanOrEqual(band.End):
		// "Ineligible for a DIRECT Roth contribution" is the true statement, and
		// it is the one worth writing. A backdoor Roth — a non-deductible
		// traditional contribution converted straight away — is legal, common
		// above this line, and NOT modelled here. Saying "you cannot put money
		// here" would be false advice about a route many households above the
		// phase-out actually take.
		return Eligibility{
			Status: EligibilityIneligible,
			Limit:  decimal.Zero,
			Note: "Above the " + strconv.Itoa(year) + " Roth MAGI limit for this filing status, so a DIRECT Roth " +
				"contribution is not allowed. A backdoor Roth (a non-deductible traditional IRA contribution, " +
				"converted) is a separate route this planner does not model.",
		}, true
	}

	// Inside the band. The reduction is linear across the range and then
	// rounded the IRS's way.
	span := band.End.Sub(band.Start)
	remaining := band.End.Sub(income) // > 0 here
	allowed := fullLimit.Mul(remaining).Div(span)
	allowed = allowed.Div(rothRoundTo).Ceil().Mul(rothRoundTo)
	if allowed.IsPositive() && allowed.LessThan(rothMinimum) {
		allowed = rothMinimum
	}
	if allowed.GreaterThan(fullLimit) {
		allowed = fullLimit
	}

	pct := remaining.Div(span).Mul(oneHundredPct).Round(0)
	return Eligibility{
		Status: EligibilityPhasedOut,
		Limit:  allowed,
		Note: "Inside the " + strconv.Itoa(year) + " Roth MAGI phase-out for this filing status: about " +
			pct.String() + "% of the full contribution is allowed, so the cap here is $" +
			allowed.StringFixed(2) + " rather than $" + fullLimit.StringFixed(2) + ".",
	}, true
}
