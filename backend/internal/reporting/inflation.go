package reporting

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// Inflation adjustment: turning nominal dollars into real ones (doc 27).
//
// Every long-horizon comparison this app makes is between dollars from
// different years, and until this file existed it made them as if a 2019 dollar
// and a 2026 dollar were the same thing. They are not, by about 30%. "Net worth
// up 8% this year" in a 6% inflation year is 2% real growth, and the app had no
// way to say so.
//
// Three rules run through everything here, and they are the same rules the rest
// of the app's honesty work follows.
//
//  1. Deflation is a VIEW, applied at read time, always. Nothing stored is ever
//     rewritten into real terms. A stored figure is what it was, in the dollars
//     of the day it was recorded.
//  2. A missing index is an ERROR, never a pass-through. A "real" number that
//     is quietly still nominal is the exact dishonesty this file exists to
//     remove, and it is undetectable downstream — it looks like a real number.
//  3. The base period is stated wherever a real figure renders. "In June 2026
//     dollars" is not a footnote; it is what makes the number mean anything.

// CPISeries is the published CPI-U index, in memory.
//
// Kept as a sorted slice plus a lookup map rather than a bare map, because two
// separate things are asked of it: an exact index for one month (deflation,
// which must be exact or fail) and "what is the newest month you have"
// (labelling and staleness, which must be answerable without scanning).
type CPISeries struct {
	byPeriod map[time.Time]decimal.Decimal
	periods  []time.Time // ascending, normalised to first-of-month UTC
}

// CPIPoint is one published month.
type CPIPoint struct {
	Period time.Time
	Index  decimal.Decimal
}

// NewCPISeries builds a series from published points. Periods are normalised to
// the first of their month, so a caller cannot miss a lookup because it passed
// the 15th.
//
// A duplicate period keeps the LAST value given, matching the upsert semantics
// of the table it is loaded from: a BLS revision supersedes what it revises.
func NewCPISeries(points []CPIPoint) *CPISeries {
	s := &CPISeries{byPeriod: make(map[time.Time]decimal.Decimal, len(points))}
	for _, p := range points {
		if !p.Index.IsPositive() {
			// A zero or negative index is not a price level. Dropping it here
			// means Real() reports a missing index rather than dividing by zero.
			continue
		}
		s.byPeriod[MonthStart(p.Period)] = p.Index
	}
	s.periods = make([]time.Time, 0, len(s.byPeriod))
	for period := range s.byPeriod {
		s.periods = append(s.periods, period)
	}
	sort.Slice(s.periods, func(i, j int) bool { return s.periods[i].Before(s.periods[j]) })
	return s
}

// LoadCPISeries reads the whole published series.
//
// Whole rather than paged: ~200 rows growing by twelve a year, and deflating a
// multi-year chart needs most of them anyway.
func LoadCPISeries(ctx context.Context, q *dbgen.Queries) (*CPISeries, error) {
	rows, err := q.ListCPISeries(ctx)
	if err != nil {
		return nil, fmt.Errorf("list cpi series: %w", err)
	}
	points := make([]CPIPoint, 0, len(rows))
	for _, r := range rows {
		points = append(points, CPIPoint{Period: r.Period, Index: r.IndexValue})
	}
	return NewCPISeries(points), nil
}

// MonthStart normalises any instant to the first of its month, in UTC.
//
// Every period key in this file goes through it, so a lookup can never miss
// because of a time-of-day component or a caller's local zone.
func MonthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// Empty reports whether the series carries nothing at all. A deployment whose
// seed failed to load looks like this, and callers offer no real view rather
// than erroring on every point.
func (s *CPISeries) Empty() bool { return s == nil || len(s.periods) == 0 }

// Index returns one month's published index. ok is false when that month was
// never published — which is a real state, not just an out-of-range one: BLS
// never published October 2025 (the 2025 lapse in appropriations), so the
// series has a hole in the middle and not only at its ends.
func (s *CPISeries) Index(period time.Time) (decimal.Decimal, bool) {
	if s.Empty() {
		return decimal.Zero, false
	}
	v, ok := s.byPeriod[MonthStart(period)]
	return v, ok
}

// Earliest and Latest are the span the series actually covers. ok is false on
// an empty series.
func (s *CPISeries) Earliest() (time.Time, bool) {
	if s.Empty() {
		return time.Time{}, false
	}
	return s.periods[0], true
}

func (s *CPISeries) Latest() (time.Time, bool) {
	if s.Empty() {
		return time.Time{}, false
	}
	return s.periods[len(s.periods)-1], true
}

// BasePeriod is the month real figures are expressed in: "today's dollars",
// except that today's index does not exist yet.
//
// BLS publishes a month around the middle of the FOLLOWING month, so the
// current month never has an index and the previous one usually does not
// either. Falling back to the newest published month is what the doc asks for,
// and the fallback is safe precisely because the result is labelled: the UI
// says "in June 2026 dollars", not "in today's dollars", so nobody is told the
// figure is fresher than the series is.
//
// ok is false on an empty series, or when the series begins after `now` — a
// deployment whose clock predates the seed has no honest base to offer.
func (s *CPISeries) BasePeriod(now time.Time) (time.Time, bool) {
	if s.Empty() {
		return time.Time{}, false
	}
	current := MonthStart(now)
	if _, ok := s.byPeriod[current]; ok {
		return current, true
	}
	// Newest published month at or before now.
	for i := len(s.periods) - 1; i >= 0; i-- {
		if !s.periods[i].After(current) {
			return s.periods[i], true
		}
	}
	return time.Time{}, false
}

// StaleAfterMonths is how far behind the newest published month may fall before
// the series is called stale.
//
// Two, not one. BLS publishes mid-following-month, so being one month behind is
// the NORMAL state of a perfectly healthy series and flagging it would train
// users to ignore the flag. Two months behind means a release was missed.
const StaleAfterMonths = 2

// Stale reports whether the newest published month is far enough behind `now`
// to be worth saying so. The UI says the series is stale rather than silently
// drawing a shorter chart — a chart that quietly stops is indistinguishable
// from a household that quietly stopped.
func (s *CPISeries) Stale(now time.Time) bool {
	latest, ok := s.Latest()
	if !ok {
		return true
	}
	return monthsBetween(latest, MonthStart(now)) > StaleAfterMonths
}

// ErrNoIndex is returned when a period has no published index.
//
// A sentinel rather than a bare error so a caller can tell "this month cannot
// be deflated" (normal: the series does not reach that far back, or it is the
// October 2025 hole) apart from a real failure, and degrade that one point
// rather than the whole chart.
type ErrNoIndex struct{ Period time.Time }

func (e ErrNoIndex) Error() string {
	return fmt.Sprintf("no published CPI index for %s", e.Period.Format("January 2006"))
}

// realScale is the working precision of the index ratio.
//
// The indices themselves carry four decimals and the ratio is order-1, so eight
// places is far more than the inputs justify; it is here so the rounding that
// matters happens exactly once, at the cent, rather than accumulating through a
// chart's worth of points.
const realScale = 8

// Real converts a nominal amount dated `from` into `base`-dated dollars.
//
//	real = nominal × index[base] / index[from]
//
// Exact decimal throughout, rounded to the cent once at the end.
//
// It returns ErrNoIndex for a missing index on either side rather than passing
// the nominal figure through. That refusal is the whole point of the function:
// a real figure that is quietly nominal cannot be spotted downstream, and every
// caller here would rather render a gap and say why.
//
// A same-period conversion is the identity, exactly — index[base] and
// index[from] are the same value, so the ratio is 1 and no rounding error can
// creep in.
func Real(nominal decimal.Decimal, from, base time.Time, series *CPISeries) (decimal.Decimal, error) {
	fromPeriod, basePeriod := MonthStart(from), MonthStart(base)

	fromIndex, ok := series.Index(fromPeriod)
	if !ok {
		return decimal.Zero, ErrNoIndex{Period: fromPeriod}
	}
	if fromPeriod.Equal(basePeriod) {
		return nominal, nil
	}
	baseIndex, ok := series.Index(basePeriod)
	if !ok {
		return decimal.Zero, ErrNoIndex{Period: basePeriod}
	}

	return nominal.Mul(baseIndex).DivRound(fromIndex, realScale).Round(2), nil
}

// Change is the price-level change between two months, as a rate: 0.062 means
// prices rose 6.2%.
//
// Used for the "inflation was X% over this window" line beside a real chart,
// and to deflate a RETURN rather than a level — see RealRate.
func (s *CPISeries) Change(from, to time.Time) (decimal.Decimal, error) {
	fromPeriod, toPeriod := MonthStart(from), MonthStart(to)
	fromIndex, ok := s.Index(fromPeriod)
	if !ok {
		return decimal.Zero, ErrNoIndex{Period: fromPeriod}
	}
	toIndex, ok := s.Index(toPeriod)
	if !ok {
		return decimal.Zero, ErrNoIndex{Period: toPeriod}
	}
	return toIndex.DivRound(fromIndex, realScale).Sub(decimal.NewFromInt(1)), nil
}

// AnnualisedChange is the compound annual price change between two months —
// "inflation averaged X% a year over this window".
//
// Compound, not the arithmetic mean of the annual rates, for the same reason
// annualise exists at all: prices compound, and averaging the rates understates
// a volatile decade.
//
// ok is false when either month is missing, when the window is under a year
// (annualising a shorter one produces a defensible-looking lie, exactly as it
// does for a portfolio return), or when the ratio is not annualisable.
func (s *CPISeries) AnnualisedChange(from, to time.Time) (decimal.Decimal, bool) {
	fromPeriod, toPeriod := MonthStart(from), MonthStart(to)
	change, err := s.Change(fromPeriod, toPeriod)
	if err != nil {
		return decimal.Zero, false
	}
	days := int(toPeriod.Sub(fromPeriod).Hours() / 24)
	if days < minAnnualiseDays {
		return decimal.Zero, false
	}
	return annualise(change, days)
}

// RealRate deflates a nominal RETURN by a price-level change.
//
//	real = (1 + nominal) / (1 + inflation) − 1
//
// Not `nominal − inflation`, which is the approximation everybody reaches for
// and which is wrong by the product of the two: at 20% nominal and 6%
// inflation, subtraction says 14% and the truth is 13.2%. Over a decade that
// gap compounds into a materially different retirement.
func RealRate(nominal, inflation decimal.Decimal) decimal.Decimal {
	one := decimal.NewFromInt(1)
	denominator := one.Add(inflation)
	if denominator.IsZero() {
		// Total deflation to zero prices is not a state any published series
		// can be in; the guard exists so this can never panic on bad data.
		return nominal
	}
	return one.Add(nominal).DivRound(denominator, realScale).Sub(one)
}

// YearToDate is inflation from last December to the base period, the figure the
// Dashboard strip quotes as "inflation YTD".
//
// Anchored on December rather than on "twelve months ago" because that is what
// "year to date" means everywhere else in this app, and because a household
// comparing it against its own YTD income growth needs the two windows to be
// the same window.
func (s *CPISeries) YearToDate(now time.Time) (rate decimal.Decimal, from, to time.Time, ok bool) {
	base, ok := s.BasePeriod(now)
	if !ok {
		return decimal.Zero, time.Time{}, time.Time{}, false
	}
	start := time.Date(base.Year()-1, time.December, 1, 0, 0, 0, 0, time.UTC)
	if start.Equal(base) {
		return decimal.Zero, time.Time{}, time.Time{}, false
	}
	change, err := s.Change(start, base)
	if err != nil {
		return decimal.Zero, time.Time{}, time.Time{}, false
	}
	return change, start, base, true
}

// Gaps lists the months inside the series' own span that carry no index.
//
// These exist and are not a data-quality bug: BLS never published October 2025,
// because the 2025 lapse in appropriations stopped collection, and the agency
// has said it will not be estimated after the fact. Surfacing the list is what
// lets the UI explain a hole in a chart instead of drawing a line straight
// through it.
func (s *CPISeries) Gaps() []time.Time {
	if s.Empty() {
		return nil
	}
	var out []time.Time
	for month := s.periods[0]; month.Before(s.periods[len(s.periods)-1]); month = month.AddDate(0, 1, 0) {
		if _, ok := s.byPeriod[month]; !ok {
			out = append(out, month)
		}
	}
	return out
}

// RealReturns is the inflation-adjusted half of a Performance.
//
// Only RETURN figures appear here, never dollar figures, and the omission is
// deliberate. Deflating StartValue or NetFlows correctly would need every cash
// flow deflated on its own date; deflating them from the period's endpoints
// would produce a number that looks precise and is not. Returns are exactly
// what the ratio of two index values can honestly convert.
type RealReturns struct {
	// Inflation is the price-level change across the same span the return
	// covers, as a fraction.
	Inflation decimal.Decimal
	// AnnualInflation is that same change expressed as a compound annual rate.
	// It exists because an ALREADY-annualised return (MWR) must be deflated by
	// an annualised price change; deflating it by the total would understate the
	// real figure by the whole compounding of the period. Nil for spans under a
	// year, where annualising either side is not defensible.
	AnnualInflation *decimal.Decimal

	// TWR, Annualised and MWR mirror Performance's, deflated. Nil wherever the
	// nominal figure was nil — a real return over a span too short to annualise
	// is no more annualisable than the nominal one.
	TWR        *decimal.Decimal
	Annualised *decimal.Decimal
	MWR        *decimal.Decimal
}

// Deflate converts a Performance's returns into real terms.
//
// ok is false when either endpoint's month has no published index, or when
// there was no nominal TWR to deflate. The Investments page then shows the
// nominal figures alone and says why, rather than showing a real column that is
// quietly nominal.
func (s *CPISeries) Deflate(p Performance) (RealReturns, bool) {
	if p.TWR == nil {
		return RealReturns{}, false
	}
	inflation, err := s.Change(p.Start, p.End)
	if err != nil {
		return RealReturns{}, false
	}

	out := RealReturns{Inflation: inflation}
	twr := RealRate(*p.TWR, inflation)
	out.TWR = &twr

	// Annualised only where the nominal one was: the span rule is about how much
	// history exists, and deflating does not add any.
	if p.Annualised != nil {
		if ann, ok := annualise(twr, p.Days); ok {
			out.Annualised = &ann
		}
	}

	// MWR arrives already annualised, so it is deflated by the annualised price
	// change rather than the total. Using the total here would understate the
	// real return by the entire compounding of the span — a five-year window at
	// 3% inflation would be deflated by 16% instead of 3%.
	if annualInflation, ok := s.AnnualisedChange(p.Start, p.End); ok {
		out.AnnualInflation = &annualInflation
		if p.MWR != nil {
			realMWR := RealRate(*p.MWR, annualInflation)
			out.MWR = &realMWR
		}
	}
	return out, true
}

// monthsBetween counts whole months from a to b. Negative when b precedes a.
func monthsBetween(a, b time.Time) int {
	a, b = MonthStart(a), MonthStart(b)
	return (b.Year()-a.Year())*12 + int(b.Month()-a.Month())
}
