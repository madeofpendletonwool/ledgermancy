package networth

import (
	"fmt"
	"time"

	"github.com/shopspring/decimal"
)

// Valuation of directly-held bonds — the ones Plaid cannot see.
//
// A bond bought through a brokerage is already handled: Plaid returns it as a
// security and it flows into holdings. This file is for the other case, the one
// that has no aggregator at all — Series I and EE savings bonds and marketable
// Treasuries bought at TreasuryDirect. Until now those sat in manual_assets as
// a frozen purchase price that never accrued.
//
// The whole point is that this is NOT an estimate. A savings bond's value is
// arithmetic over rates the Treasury publishes, and the same inputs give the
// same answer to the cent every time. Everything here is a pure function over
// decimal.Decimal taking `now` as a parameter, the way ProjectRetirement does;
// nothing reads the clock.
//
// # The algorithm, and where it comes from
//
// 31 CFR 359.39 gives the redemption value of a savings bond on an accrual date
// (the first of each month; the value does not change between them):
//
//	FV = PV × [1 + (CR ÷ 2)]^(m ÷ 6)
//
// where PV is the redemption value at the start of the semiannual rate period,
// CR is the composite rate as a decimal, and m is the number of full calendar
// months elapsed in that period. FV is rounded to the nearest cent AT EACH
// ACCRUAL DATE — the rounding is part of the instrument, not a display concern,
// and dropping it puts the answer cents away from the Treasury's own.
//
// For Series I the composite rate (31 CFR 359.17) is
//
//	composite = fixed + 2×inflation + (fixed × inflation)
//
// floored at zero. The floor is a property of the bond, not a safety check: an
// I bond never loses nominal value, which is why a deflationary period reads as
// zero return rather than a loss.
//
// # The $25 unit
//
// Treasury tabulates redemption values for a $25 bond and scales: "the
// redemption value of a $50 bond is twice the amount shown". That matters
// because the per-period rounding happens on the UNIT, so rounding at the
// wrong base lands cents away on a large holding. A $1,000 I bond is 40 units,
// not one $1,000 bond.
//
// The unit's ISSUE PRICE is not always $25. Paper EE bonds were sold at half
// face, so a $25-face EE unit was bought for $12.50 — which is also why a paper
// EE entered at its face value reads as twice what it cost.
//
// Book-entry bonds are defined slightly differently (31 CFR 359.55 prorates a
// $100 bond rather than a $25 one) and can land a few cents from this on a
// large holding. The $25 basis is used here because it is the one Treasury's
// own public calculator applies, and therefore the only one a user can check
// without logging in to TreasuryDirect. Every figure this file produces was
// verified against that calculator — see bonds_test.go.
//
// # What is deliberately NOT modelled
//
// Marketable Treasuries are held at par plus accrued coupon, and say so. Marking
// one to market needs a live price this app has no source for, and a Treasury
// held to maturity is worth par — which is what a household holding one
// intends. Yield-to-maturity, duration and credit ratings are out for the same
// reason.

// Bond series. These match asset_details.bond_series.
const (
	SeriesISavings  = "i_savings"
	SeriesEESavings = "ee_savings"
	SeriesTreasury  = "treasury"
	SeriesOther     = "other"
)

const (
	// unitFace is the denomination Treasury tabulates redemption values for.
	unitFace = 25

	// savingsBondTermMonths is final maturity. A savings bond stops earning at
	// 30 years; it does not keep compounding in a drawer.
	savingsBondTermMonths = 360

	// eeDoublingMonths is original maturity for a Series EE bond, at which
	// Treasury guarantees it is worth at least twice its issue price.
	eeDoublingMonths = 240

	// penaltyMonths is the interest forfeited on redemption inside
	// penaltyFreeMonths: the earning period is shortened by three months.
	penaltyMonths     = 3
	penaltyFreeMonths = 60
)

// eeFixedRateEra is the first issue month whose EE bonds earn a fixed rate for
// life. Earlier EE bonds earned variable market-based rates (90% of five-year
// Treasury averages, reset semiannually, under guarantee periods that changed
// several times). No rate table values those, so they are refused rather than
// run through a fixed-rate model that does not describe the instrument.
var eeFixedRateEra = time.Date(2005, time.May, 1, 0, 0, 0, 0, time.UTC)

// powPrecision is the working precision for the fractional-exponent step. Far
// more than the cent the result is rounded to, so the rounding is decided by
// the arithmetic rather than by the precision.
const powPrecision = 28

// BondRate is one published rate period from savings_bond_rates. Rates are
// PERCENTAGES (3.40 means 3.40%), matching liabilities.apr.
type BondRate struct {
	Series    string
	Start     time.Time
	Fixed     decimal.Decimal
	Inflation decimal.NullDecimal // Series I only; NULL for EE
	SourceURL string
}

// BondRates is a lookup over published rate periods, ordered per series.
type BondRates struct {
	bySeries map[string][]BondRate
}

// NewBondRates indexes rate rows for lookup. Rows may arrive in any order.
func NewBondRates(rows []BondRate) *BondRates {
	r := &BondRates{bySeries: make(map[string][]BondRate, 2)}
	for _, row := range rows {
		row.Start = monthFloor(row.Start)
		s := r.bySeries[row.Series]
		// Insertion sort by Start. The table is ~60 rows per series and only
		// grows twice a year, so this stays cheaper than pulling in sort.
		i := len(s)
		s = append(s, row)
		for i > 0 && s[i-1].Start.After(row.Start) {
			s[i] = s[i-1]
			i--
		}
		s[i] = row
		r.bySeries[row.Series] = s
	}
	return r
}

// At returns the rate in effect for a period beginning on `on`.
//
// "In effect" is the greatest period_start <= on, NOT arithmetic on a May/Nov
// grid. A bond's semiannual periods start on its own issue-month anniversary,
// so a bond issued in August takes the May announcement for its first six
// months. The very first I bond period also starts on 1 September 1998, off the
// grid entirely.
//
// A row's coverage ENDS, and that is the load-bearing part. Each announcement
// covers until the next one; the most recent covers six months and no further.
// Without that bound "greatest period_start <= on" quietly answers for dates
// years past the end of the table — which is precisely the silent extrapolation
// this feature exists to refuse. A rate that has not been announced yet is not
// knowable, and the honest answer is ok=false.
func (r *BondRates) At(series string, on time.Time) (BondRate, bool) {
	on = monthFloor(on)
	rows := r.bySeries[series]

	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].Start.After(on) {
			continue
		}
		// Coverage runs to the next announcement, or six months for the last.
		end := rows[i].Start.AddDate(0, 6, 0)
		if i+1 < len(rows) {
			end = rows[i+1].Start
		}
		if !on.Before(end) {
			return BondRate{}, false
		}
		return rows[i], true
	}
	return BondRate{}, false
}

// Covered reports the last period start published for a series, for the "we
// have rates through <date>" message.
func (r *BondRates) Covered(series string) (time.Time, bool) {
	s := r.bySeries[series]
	if len(s) == 0 {
		return time.Time{}, false
	}
	return s[len(s)-1].Start, true
}

// Bond is what the app knows about one directly-held bond, read from
// asset_details.
type Bond struct {
	Series string

	// IssueDate is the issue MONTH; savings bonds accrue by month, and the day
	// within it never matters.
	IssueDate time.Time

	// PurchasePrice is what was paid. FaceValue is the denomination printed on
	// the bond. They are equal for every electronic savings bond and for I
	// bonds generally; a paper EE was sold at half face.
	PurchasePrice decimal.Decimal
	FaceValue     decimal.Decimal

	// Marketable Treasuries only.
	CouponRate   decimal.NullDecimal // percent
	MaturityDate *time.Time
}

// AppliedRate is one semiannual period that actually went into a value, kept so
// the UI can show which published rates produced the figure and the user can
// walk it back to treasurydirect.gov.
type AppliedRate struct {
	// PeriodStart is when this six-month period of the BOND began.
	PeriodStart time.Time
	// Announced is the period_start of the rate row used, which is the May or
	// November announcement that covers PeriodStart.
	Announced time.Time
	Fixed     decimal.Decimal
	Inflation decimal.NullDecimal
	Composite decimal.Decimal // annualised percent, floored at zero
	// Months is how much of this period counted: 6 for a completed period, 1–5
	// for the partial one at the end.
	Months int
}

// BondValue is a valuation. OK=false is the same shape AnnualLimitFor uses for
// an unconfigured tax year: a refusal the caller must surface, never a number
// to fall back on.
//
// When OK is false, Redemption and Accrued hold the value as far as the rate
// table reached, so the UI can render "valued through <date>" beside real
// figures — but NO CALLER MAY WRITE A VALUATION FROM A !OK RESULT. Carrying the
// last known rate forward invents a return, and writing a stale-but-plausible
// number is exactly the quiet dishonesty the rest of this app refuses.
type BondValue struct {
	OK     bool
	Reason string

	// Redemption is what the bond could be turned into on AsOf, and the only
	// figure that belongs in net worth. Accrued is what it has earned; the two
	// differ inside the first five years, when redemption forfeits the last
	// three months' interest.
	Redemption decimal.Decimal
	Accrued    decimal.Decimal

	PenaltyApplied  bool
	DoublingApplied bool

	// AsOf is the accrual date valued for — the first of the month, because the
	// value does not change between accrual dates.
	AsOf time.Time
	// ValuedThrough is the last accrual date the rate table could reach. Equal
	// to AsOf on a complete valuation.
	ValuedThrough time.Time
	FinalMaturity time.Time
	Matured       bool

	// Basis is a plain-language statement of how the figure was produced,
	// shown in the UI. A number the user cannot interrogate is
	// indistinguishable from a guess.
	Basis string
	Rates []AppliedRate
}

// ValueBond values a directly-held bond as of `now`.
//
// Deterministic: same bond, same rates, same `now` gives a byte-identical
// result. `now` is a parameter precisely so no test reads the clock.
func ValueBond(b Bond, rates *BondRates, now time.Time) BondValue {
	asOf := monthFloor(now)
	issue := monthFloor(b.IssueDate)

	v := BondValue{AsOf: asOf, ValuedThrough: asOf}

	if issue.IsZero() {
		v.Reason = "no issue date recorded; a bond cannot be valued without one"
		return v
	}
	if issue.After(asOf) {
		v.Reason = fmt.Sprintf("issued %s, which is in the future", issue.Format("January 2006"))
		return v
	}
	if !b.PurchasePrice.IsPositive() {
		v.Reason = "no purchase price recorded; a bond cannot be valued without one"
		return v
	}

	switch b.Series {
	case SeriesISavings, SeriesEESavings:
		return valueSavingsBond(b, rates, issue, asOf)
	case SeriesTreasury:
		return valueTreasury(b, issue, asOf)
	default:
		v.Reason = "only Series I, Series EE and marketable Treasuries can be valued from published data"
		return v
	}
}

// valueSavingsBond walks the bond's own six-month periods from issue forward.
func valueSavingsBond(b Bond, rates *BondRates, issue, asOf time.Time) BondValue {
	v := BondValue{AsOf: asOf, ValuedThrough: asOf}
	v.FinalMaturity = issue.AddDate(0, savingsBondTermMonths, 0)

	if b.Series == SeriesEESavings && issue.Before(eeFixedRateEra) {
		v.Reason = "EE bonds issued before May 2005 earned variable market-based rates, " +
			"which this app has no published table for. Enter its value by hand."
		return v
	}
	if rates == nil {
		v.Reason = "no savings bond rates are loaded"
		return v
	}

	// The fixed rate is set at issue and lasts the life of the bond. For an EE
	// bond it is the whole rate; for an I bond it is the half that never moves.
	issueRate, ok := rates.At(b.Series, issue)
	if !ok {
		v.Reason = fmt.Sprintf("no published rate covers %s, when this bond was issued",
			issue.Format("January 2006"))
		return v
	}

	// A savings bond stops earning at 30 years. Past that, value it as of final
	// maturity rather than compounding it forever in a drawer.
	earnUntil := asOf
	if !v.FinalMaturity.After(asOf) {
		earnUntil = v.FinalMaturity
		v.Matured = true
	}

	face := b.FaceValue
	if !face.IsPositive() {
		// Electronic savings bonds are bought at face, so an absent face value
		// means "same as what was paid" rather than an error.
		face = b.PurchasePrice
	}
	units := face.Div(decimal.NewFromInt(unitFace))
	if !units.IsPositive() {
		v.Reason = "face value must be positive"
		return v
	}
	unitPrice := b.PurchasePrice.Div(units)

	// The doubling guarantee applies to Series EE only, and it is a cliff
	// rather than a curve: a 0.10% EE bond compounds to barely more than it
	// cost over twenty years and then jumps to double on the anniversary. It
	// is applied INSIDE the accrual walk, not bolted on at the end, because the
	// last ten years of the bond's life compound from the doubled value — a
	// bond lifted to double at year 20 and then grown for a decade is worth
	// materially more than one grown for thirty years and doubled at the end.
	var floorAt *unitFloor
	if b.Series == SeriesEESavings {
		floorAt = &unitFloor{
			afterMonths: eeDoublingMonths,
			value:       unitPrice.Mul(decimal.NewFromInt(2)),
		}
	}

	// Accrue to the requested date, and separately to three months earlier —
	// the redemption value inside the first five years is the value the bond
	// had three months ago (31 CFR 351.35: "we reduce the overall earning
	// period from the issue date by three months").
	accrued, through, applied, reason := accrue(unitPrice, issue, earnUntil, b.Series, issueRate, rates, floorAt)
	v.Rates = applied
	v.ValuedThrough = through
	v.DoublingApplied = floorAt != nil && floorAt.applied

	monthsHeld := monthsBetween(issue, earnUntil)
	redeemUntil := earnUntil
	if monthsHeld < penaltyFreeMonths {
		redeemUntil = earnUntil.AddDate(0, -penaltyMonths, 0)
		if redeemUntil.Before(issue) {
			redeemUntil = issue
		}
		v.PenaltyApplied = true
	}

	redeemUnit := accrued
	if v.PenaltyApplied {
		// A bond young enough to carry the penalty is decades from the
		// doubling, so the redemption walk needs no floor of its own.
		redeemUnit, _, _, _ = accrue(unitPrice, issue, redeemUntil, b.Series, issueRate, rates, nil)
	}

	// "The redemption value of a bond subject to the 3-month interest penalty
	// shall not be reduced below the issue price" (31 CFR 351.35). A savings
	// bond never redeems for less than it cost.
	if redeemUnit.LessThan(unitPrice) {
		redeemUnit = unitPrice
	}

	v.Accrued = units.Mul(accrued).Round(2)
	v.Redemption = units.Mul(redeemUnit).Round(2)

	if reason != "" {
		v.Reason = reason
		return v
	}
	v.OK = true
	v.Basis = savingsBondBasis(b.Series, v, monthsHeld)
	return v
}

// unitFloor is a guaranteed minimum a unit reaches at a given age — Treasury's
// promise that an EE bond is worth at least twice its issue price at 20 years.
// Modelled as a floor applied during the walk so that later periods compound
// from it.
type unitFloor struct {
	afterMonths int
	value       decimal.Decimal
	applied     bool
}

// apply lifts a unit value to the floor once the bond is old enough. It is a
// max, so calling it at every period boundary past the anniversary is a no-op
// after the first time it bites.
func (f *unitFloor) apply(value decimal.Decimal, monthsElapsed int) decimal.Decimal {
	if f == nil || monthsElapsed < f.afterMonths || !f.value.GreaterThan(value) {
		return value
	}
	f.applied = true
	return f.value
}

// accrue walks whole six-month periods and then the partial one, rounding to
// the cent at every accrual date exactly as 31 CFR 359.39 specifies.
//
// It returns the last accrual date it could reach. A gap in the rate table
// stops the walk and returns a non-empty reason rather than carrying the last
// known rate forward.
func accrue(
	unitPrice decimal.Decimal,
	issue, until time.Time,
	series string,
	issueRate BondRate,
	rates *BondRates,
	floor *unitFloor,
) (value decimal.Decimal, through time.Time, applied []AppliedRate, reason string) {
	months := monthsBetween(issue, until)
	value = unitPrice
	through = issue

	for start := 0; start < months; start += 6 {
		value = floor.apply(value, start)
		periodStart := issue.AddDate(0, start, 0)

		// Only Series I consults the table period by period, because only its
		// inflation half moves. A Series EE bond earns the fixed rate set at
		// issue for its whole life, so the table is read once and never again —
		// which is also why a gap in later periods cannot make an old EE bond
		// unvaluable, and why an EE bond can be valued years ahead.
		row := issueRate
		if series == SeriesISavings {
			var ok bool
			row, ok = rates.At(series, periodStart)
			if !ok {
				covered, has := rates.Covered(series)
				msg := fmt.Sprintf("valued through %s; no published rate covers the six months from %s",
					through.Format("January 2006"), periodStart.Format("January 2006"))
				if has {
					msg += fmt.Sprintf(" (rates are published through %s)", covered.Format("January 2006"))
				}
				return value, through, applied, msg
			}
		}

		composite := compositeRate(series, issueRate.Fixed, row)

		// Full period, or however much of the last one has elapsed.
		m := months - start
		if m > 6 {
			m = 6
		}

		value = growUnit(value, composite, m)
		through = periodStart.AddDate(0, m, 0)
		applied = append(applied, AppliedRate{
			PeriodStart: periodStart,
			Announced:   row.Start,
			Fixed:       issueRate.Fixed,
			Inflation:   row.Inflation,
			Composite:   composite,
			Months:      m,
		})
	}
	// The anniversary can fall exactly at the end of the walk, in which case no
	// further period runs to apply it.
	value = floor.apply(value, months)
	return value, through, applied, ""
}

// compositeRate is the annualised rate for one six-month period, as a
// percentage.
//
// For Series EE that is just the fixed rate set at issue. For Series I it is
// the composite of the bond's own fixed rate and the inflation rate announced
// for the period — floored at zero, because an I bond never loses nominal
// value.
func compositeRate(series string, fixed decimal.Decimal, row BondRate) decimal.Decimal {
	if series != SeriesISavings || !row.Inflation.Valid {
		return fixed
	}
	infl := row.Inflation.Decimal
	// fixed + 2×inflation + (fixed × inflation), with the cross term divided by
	// 100 because both inputs are percentages rather than fractions.
	composite := fixed.
		Add(infl.Mul(decimal.NewFromInt(2))).
		Add(fixed.Mul(infl).Div(decimal.NewFromInt(100)))
	if composite.IsNegative() {
		return decimal.Zero
	}
	return composite
}

// growUnit applies FV = PV × [1 + (CR ÷ 2)]^(m ÷ 6) and rounds to the cent.
func growUnit(pv, compositePercent decimal.Decimal, months int) decimal.Decimal {
	if months <= 0 {
		return pv
	}
	// composite is a percentage; /200 is "half of it, as a fraction".
	base := decimal.NewFromInt(1).Add(compositePercent.Div(decimal.NewFromInt(200)))
	if months == 6 {
		return pv.Mul(base).Round(2)
	}
	exp := decimal.NewFromInt(int64(months)).DivRound(decimal.NewFromInt(6), 20)
	factor, err := base.PowWithPrecision(exp, powPrecision)
	if err != nil {
		// base is positive for any composite >= 0, which compositeRate
		// guarantees, so this is unreachable rather than a fallback worth
		// pretending is a value.
		return pv
	}
	return pv.Mul(factor).Round(2)
}

// savingsBondBasis states in words how the figure was produced.
func savingsBondBasis(series string, v BondValue, monthsHeld int) string {
	name := "Series I"
	if series == SeriesEESavings {
		name = "Series EE"
	}
	basis := fmt.Sprintf("%s savings bond, valued as of %s from %d published rate period(s).",
		name, v.AsOf.Format("January 2006"), len(v.Rates))
	switch {
	case v.Matured:
		basis += fmt.Sprintf(" It reached final maturity in %s and has stopped earning interest.",
			v.FinalMaturity.Format("January 2006"))
	case v.PenaltyApplied:
		basis += fmt.Sprintf(" Held %d months, so cashing it forfeits the last three months' interest —"+
			" the redemption value is what it could be turned into today, and the accrued value is what it has earned.",
			monthsHeld)
	}
	if v.DoublingApplied {
		basis += " Treasury's guarantee that an EE bond is worth at least twice its issue price at 20 years applies."
	}
	return basis
}

// valueTreasury holds a marketable Treasury at par plus accrued coupon, and
// says so.
//
// This is a statement of basis, not a market value. Marking one to market needs
// a live price the app has no source for, and inventing one would be a
// fabricated number in a load-bearing line. A Treasury held to maturity is
// worth par, which is what most households holding one intend.
func valueTreasury(b Bond, issue, asOf time.Time) BondValue {
	v := BondValue{AsOf: asOf, ValuedThrough: asOf}

	face := b.FaceValue
	if !face.IsPositive() {
		face = b.PurchasePrice
	}
	if b.MaturityDate == nil {
		v.Reason = "no maturity date recorded; a Treasury cannot be held at par without one"
		return v
	}
	v.FinalMaturity = monthFloor(*b.MaturityDate)

	if !v.FinalMaturity.After(asOf) {
		v.Matured = true
		v.OK = true
		v.Accrued = face.Round(2)
		v.Redemption = v.Accrued
		v.Basis = fmt.Sprintf("Matured %s; held at face value. A matured Treasury earns nothing further.",
			v.FinalMaturity.Format("January 2006"))
		return v
	}

	v.Accrued = face.Round(2)
	v.Basis = "Held at par. This is not a market value — marking a Treasury to market needs a live price " +
		"this app has no source for, and a Treasury held to maturity is worth its face value."

	// Accrued coupon since the last payment date. Coupons run back in six-month
	// steps from maturity, which is how a Treasury's schedule is actually
	// defined. Interest already PAID has landed in a bank account and is counted
	// there, so only the unpaid stub belongs on the asset.
	if b.CouponRate.Valid && b.CouponRate.Decimal.IsPositive() {
		last := v.FinalMaturity
		for last.After(asOf) {
			last = last.AddDate(0, -6, 0)
		}
		if last.Before(issue) {
			last = issue
		}
		months := monthsBetween(last, asOf)
		if months > 0 {
			// face × rate/100 × months/12
			coupon := face.
				Mul(b.CouponRate.Decimal).Div(decimal.NewFromInt(100)).
				Mul(decimal.NewFromInt(int64(months))).Div(decimal.NewFromInt(12))
			v.Accrued = face.Add(coupon).Round(2)
			v.Basis += fmt.Sprintf(" Includes %d month(s) of coupon interest accrued at %s%% since the last payment.",
				months, b.CouponRate.Decimal.String())
		}
	}

	v.Redemption = v.Accrued
	v.OK = true
	return v
}

// monthFloor normalises a date to the first of its month in UTC. Savings bonds
// accrue on the first of the month and the value does not change between
// accrual dates, so the day is noise that would otherwise make results depend
// on when a job happened to run.
func monthFloor(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// monthsBetween counts whole calendar months from a to b. Negative if b is
// before a.
func monthsBetween(a, b time.Time) int {
	a, b = monthFloor(a), monthFloor(b)
	return int(b.Year()-a.Year())*12 + int(b.Month()) - int(a.Month())
}

// MonthsToDoubling counts how far a Series EE bond is from its 20-year
// guarantee. Zero once the anniversary has passed.
//
// It is surfaced because it is the single most useful thing to know about an EE
// bond: the value is nearly flat for two decades and then steps, so "18 months
// away" is a materially different position from "eight years away" even though
// the current figures look alike.
func MonthsToDoubling(issue, asOf time.Time) int {
	remaining := eeDoublingMonths - monthsBetween(issue, asOf)
	if remaining < 0 {
		return 0
	}
	return remaining
}
