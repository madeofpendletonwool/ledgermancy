package payroll

import (
	"sort"
	"time"

	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/networth"
)

// Year-to-date roll-ups, contribution headroom and the annual tax summary.
//
// Everything here operates on CONFIRMED stubs only. The caller is responsible
// for that filter — it lives in SQL, so no consumer can forget it — and this
// package assumes it has already happened.

// DatedStub is one confirmed stub plus the header figures a roll-up needs.
type DatedStub struct {
	Stub
	PayDate time.Time
	// YTDGross is the year-to-date gross the employer printed on this stub, when
	// it printed one. See reconcile below for why it is not simply preferred.
	YTDGross decimal.NullDecimal
	YTDNet   decimal.NullDecimal
}

// EmployerYear is one employer's confirmed stubs for one tax year.
//
// Key is an opaque identity for the employer — the handler passes the row's
// UUID as a string. Keeping it a string is what lets every test in this package
// read as "acme" and "globex" rather than as two generated UUIDs.
type EmployerYear struct {
	Key       string
	Name      string
	Frequency PayFrequency
	Stubs     []DatedStub
}

// Year is every employer's stubs for one tax year.
//
// Grouping by employer is not presentational. The IRS elective-deferral limit
// is ONE number across every 401(k) a person holds, so a mid-year job change
// has to pool two employers' deferrals against a single cap — while the YTD
// figures printed on each employer's stubs restart at zero on the new job and
// must never be added to each other as if they continued. Those two facts pull
// in opposite directions, and this shape is what holds both.
type Year struct {
	TaxYear   int
	Employers []EmployerYear
}

// reconcile picks a total for one employer and one measure, given the per-period
// amounts and the last YTD figure the employer printed.
//
// The larger of the two, and the reason is that each is wrong in a different
// direction and neither is trustworthy alone:
//
//   - The sum of period amounts is short whenever the household has not entered
//     every stub — which is the normal state of a feature somebody starts using
//     in June.
//   - The printed YTD is short whenever the employer changed payroll providers
//     mid-year, or the most recent stub on file is not the most recent stub that
//     exists.
//
// Taking the larger means a partial history still reports a credible YTD, and
// entering the missing stubs later can only refine it upward. Understating a
// contribution total is the expensive direction: it reports headroom that is
// not there and invites somebody to over-contribute past a limit.
func reconcile(periodSum decimal.Decimal, latestYTD decimal.NullDecimal) decimal.Decimal {
	if latestYTD.Valid && latestYTD.Decimal.GreaterThan(periodSum) {
		return latestYTD.Decimal
	}
	return periodSum
}

// sortedStubs returns an employer's stubs oldest first, without mutating the
// caller's slice — the roll-ups read "the latest stub" repeatedly and must not
// depend on the order the database happened to hand them over in.
func (e EmployerYear) sortedStubs() []DatedStub {
	out := make([]DatedStub, len(e.Stubs))
	copy(out, e.Stubs)
	sort.SliceStable(out, func(i, j int) bool { return out[i].PayDate.Before(out[j].PayDate) })
	return out
}

// Gross is the employer's year-to-date gross pay.
func (e EmployerYear) Gross() decimal.Decimal {
	sum := decimal.Zero
	latest := decimal.NullDecimal{}
	for _, s := range e.sortedStubs() {
		sum = sum.Add(s.Gross)
		if s.YTDGross.Valid {
			latest = s.YTDGross
		}
	}
	return reconcile(sum, latest)
}

// Net is the employer's year-to-date take-home.
func (e EmployerYear) Net() decimal.Decimal {
	sum := decimal.Zero
	latest := decimal.NullDecimal{}
	for _, s := range e.sortedStubs() {
		sum = sum.Add(s.Net)
		if s.YTDNet.Valid {
			latest = s.YTDNet
		}
	}
	return reconcile(sum, latest)
}

// CategoryTotal is the employer's year-to-date total for one line category.
func (e EmployerYear) CategoryTotal(c Category) decimal.Decimal {
	sum := decimal.Zero
	latest := decimal.NullDecimal{}
	for _, s := range e.sortedStubs() {
		for _, l := range s.Lines {
			if l.Category != c {
				continue
			}
			sum = sum.Add(l.Amount)
			if l.YTDAmount.Valid {
				latest = l.YTDAmount
			}
		}
	}
	return reconcile(sum, latest)
}

// LastPayDate is the most recent pay date on file, and false when the employer
// has no stubs for the year.
func (e EmployerYear) LastPayDate() (time.Time, bool) {
	stubs := e.sortedStubs()
	if len(stubs) == 0 {
		return time.Time{}, false
	}
	return stubs[len(stubs)-1].PayDate, true
}

// --------------------------------------------------------------------------
// Year-level roll-ups
// --------------------------------------------------------------------------

// Gross is total gross pay across every employer.
func (y Year) Gross() decimal.Decimal { return y.sumEmployers(EmployerYear.Gross) }

// Net is total take-home across every employer.
func (y Year) Net() decimal.Decimal { return y.sumEmployers(EmployerYear.Net) }

func (y Year) sumEmployers(f func(EmployerYear) decimal.Decimal) decimal.Decimal {
	total := decimal.Zero
	for _, e := range y.Employers {
		total = total.Add(f(e))
	}
	return total
}

// CategoryTotal is one line category summed across every employer.
func (y Year) CategoryTotal(c Category) decimal.Decimal {
	total := decimal.Zero
	for _, e := range y.Employers {
		total = total.Add(e.CategoryTotal(c))
	}
	return total
}

// TaxTotal is every genuine tax line, across every employer.
func (y Year) TaxTotal() decimal.Decimal {
	total := decimal.Zero
	for _, c := range Categories() {
		if SpecFor(c).IsTax {
			total = total.Add(y.CategoryTotal(c))
		}
	}
	return total
}

// EffectiveTaxRate is TaxTotal / Gross for the year, false on zero gross.
func (y Year) EffectiveTaxRate() (decimal.Decimal, bool) {
	gross := y.Gross()
	if !gross.IsPositive() {
		return decimal.Zero, false
	}
	return y.TaxTotal().Div(gross), true
}

// EmployerTotal is everything every employer added on top of gross.
func (y Year) EmployerTotal() decimal.Decimal {
	total := decimal.Zero
	for _, c := range Categories() {
		if SpecFor(c).EmployerOnly {
			total = total.Add(y.CategoryTotal(c))
		}
	}
	return total
}

// TotalCompensation is gross plus employer contributions, across the year.
func (y Year) TotalCompensation() decimal.Decimal {
	return y.Gross().Add(y.EmployerTotal())
}

// --------------------------------------------------------------------------
// Contribution headroom
// --------------------------------------------------------------------------

// Headroom is how much room is left under one IRS annual cap.
type Headroom struct {
	// Group is the shared-limit group: "401k", "ira" or "hsa". It is the group
	// and not the category because that is the level the limit applies at — a
	// traditional and a Roth 401(k) share one cap, and reporting each against
	// the full cap would double somebody's apparent room.
	Group string
	// Label is the group's display name.
	Label string
	// Contributed is the year's total across every employer and both tax
	// treatments in the group.
	Contributed decimal.Decimal
	// Limit is the cap for the tax year, including any catch-up the age
	// qualifies for.
	Limit decimal.Decimal
	// Remaining is Limit − Contributed, floored at zero. Over-contributing is
	// possible and worth showing, which is what OverBy is for; a negative
	// "remaining" would read as a target.
	Remaining decimal.Decimal
	// OverBy is how far past the cap the year has gone, zero when it has not.
	// Surfaced rather than clamped away: an excess deferral has to be withdrawn
	// before the filing deadline or it is taxed twice, so it is the single most
	// actionable thing this whole calculation can say.
	OverBy decimal.Decimal
	// PeriodsLeft is how many more pay dates fall in the tax year, summed over
	// employers that have a stub on file. Nil when no employer's cadence could
	// be established — the UI then omits the per-period figure rather than
	// dividing by a guess.
	PeriodsLeft *int
	// PerPeriod is Remaining / PeriodsLeft, the amount to defer each remaining
	// paycheck to land exactly on the cap. Nil whenever PeriodsLeft is.
	PerPeriod *decimal.Decimal
}

// headroomGroups is the display order and naming of the shared-limit groups.
var headroomGroups = []struct {
	Group string
	Label string
	// Treatment is the account tax-treatment string networth.AnnualLimitFor
	// keys on. Any member of the group resolves to the same cap, so the
	// traditional variant is used as the representative.
	Treatment string
}{
	{"401k", "401(k) elective deferral", "trad_401k"},
	{"ira", "IRA", "trad_ira"},
	{"hsa", "HSA", "hsa"},
}

// GroupTotal is the year's contributions to one shared-limit group, summed
// across every category in it and every employer.
//
// This is the mid-year-job-change case working correctly: two employers, two
// separate sets of stubs, one combined deferral measured against one cap.
func (y Year) GroupTotal(group string) decimal.Decimal {
	total := decimal.Zero
	for _, c := range Categories() {
		if SpecFor(c).LimitGroup == group {
			total = total.Add(y.CategoryTotal(c))
		}
	}
	return total
}

// ContributionHeadroom reports remaining room under each shared IRS cap.
//
// age is the contributor's age in the tax year and decides catch-up
// eligibility; pass zero when it is unknown, which yields the base limit. That
// is the conservative direction — it understates headroom for somebody over 50
// rather than inventing a catch-up they may not qualify for.
//
// The second result is false when the tax year is not in networth's limits
// table. Callers MUST surface that rather than substituting an adjacent year;
// networth/limits.go states the rule and this is the second consumer of it.
func (y Year) ContributionHeadroom(age int, familyHSA bool) ([]Headroom, bool) {
	limits, ok := networth.Limits(y.TaxYear)
	if !ok {
		return nil, false
	}

	periodsLeft := y.remainingPayPeriods()

	out := make([]Headroom, 0, len(headroomGroups))
	for _, g := range headroomGroups {
		limit, capped := networth.AnnualLimitFor(g.Treatment, age, limits, familyHSA)
		if !capped {
			continue
		}
		contributed := y.GroupTotal(g.Group)

		h := Headroom{
			Group:       g.Group,
			Label:       g.Label,
			Contributed: contributed,
			Limit:       limit,
			Remaining:   decimal.Max(limit.Sub(contributed), decimal.Zero),
			OverBy:      decimal.Max(contributed.Sub(limit), decimal.Zero),
		}
		if periodsLeft != nil && *periodsLeft > 0 {
			h.PeriodsLeft = periodsLeft
			perPeriod := h.Remaining.Div(decimal.NewFromInt(int64(*periodsLeft))).Round(2)
			h.PerPeriod = &perPeriod
		}
		out = append(out, h)
	}
	return out, true
}

// remainingPayPeriods is how many more pay dates fall in the tax year, summed
// across every employer with a stub on file. Nil when no employer's cadence
// could be established.
//
// Summed rather than taken from one employer because the contribution that
// matters is the household member's total: somebody working two jobs can defer
// from both, and a "per remaining paycheck" figure computed from one of them
// would be wrong by the size of the other.
func (y Year) remainingPayPeriods() *int {
	total := 0
	any := false
	for _, e := range y.Employers {
		last, ok := e.LastPayDate()
		if !ok {
			continue
		}
		n, ok := RemainingPayPeriods(last, e.Frequency, y.TaxYear)
		if !ok {
			continue
		}
		total += n
		any = true
	}
	if !any {
		return nil
	}
	return &total
}

// RemainingPayPeriods counts the pay dates after `last` that still fall in the
// tax year.
//
// Derived from the calendar rather than from "periods per year minus stubs on
// file", which would be wrong in the ordinary case: a household that starts
// using this feature in June and enters three stubs has not been paid three
// times this year, and a headroom figure spread over 23 imaginary remaining
// paychecks is worse than no figure at all.
//
// Semimonthly assumes the 15th and the last day of the month, which is the
// overwhelmingly common schedule but IS an assumption — an employer paying on
// the 5th and the 20th gets the same count, since what is being counted is how
// many pay dates remain rather than which days they land on.
func RemainingPayPeriods(last time.Time, f PayFrequency, taxYear int) (int, bool) {
	if _, ok := PeriodsPerYear(f); !ok {
		return 0, false
	}
	last = last.UTC().Truncate(24 * time.Hour)
	yearEnd := time.Date(taxYear, 12, 31, 0, 0, 0, 0, time.UTC)
	if !last.Before(yearEnd) {
		return 0, true
	}

	switch f {
	case Weekly:
		return int(yearEnd.Sub(last).Hours()/24) / 7, true
	case Biweekly:
		return int(yearEnd.Sub(last).Hours()/24) / 14, true
	case Monthly:
		// Whole months between the last pay date and year end. A December pay
		// date leaves none, which is why this is a month difference rather than
		// "12 minus the month number".
		return 12 - int(last.Month()), true
	case Semimonthly:
		count := 0
		for m := last.Month(); m <= time.December; m++ {
			mid := time.Date(taxYear, m, 15, 0, 0, 0, 0, time.UTC)
			// The 0th of next month is the last day of this one, which avoids
			// hard-coding 28/29/30/31 and gets February right in a leap year.
			end := time.Date(taxYear, m+1, 0, 0, 0, 0, 0, time.UTC)
			if mid.After(last) {
				count++
			}
			if end.After(last) {
				count++
			}
		}
		return count, true
	}
	return 0, false
}

// --------------------------------------------------------------------------
// The annual tax summary
// --------------------------------------------------------------------------

// W2Box is one line of the annual summary, named for the W-2 box it corresponds
// to so the figures can be checked against the real form side by side.
type W2Box struct {
	// Box is the W-2 box number, or "12" with a Code for the box 12 entries.
	Box    string
	Code   string
	Label  string
	Amount decimal.Decimal
}

// W2Summary is one employer's annual figures, mapped onto W-2 boxes.
//
// It is emphatically NOT a W-2. It is a summary of what the household's own
// stubs say, useful for checking the real form when it arrives and for handing
// an accountant a packet that is already assembled. Disclaimer travels with the
// data rather than only in the UI, because this will be printed and emailed
// away from the screen that framed it.
type W2Summary struct {
	EmployerKey  string
	EmployerName string
	TaxYear      int
	Boxes        []W2Box
	// Disclaimer is rendered into every export of this summary. See the type
	// comment; it is a field rather than a UI string for exactly that reason.
	Disclaimer string
}

// W2Disclaimer is the sentence attached to every tax summary.
const W2Disclaimer = "This is a summary of the paystubs recorded in Ledgermancy, " +
	"not a tax document. It is not a W-2, it has not been filed with anyone, and " +
	"figures may differ from the W-2 your employer issues. Use it to gather and " +
	"check your own records."

// W2 maps one employer's year onto W-2 boxes.
//
// The wage bases are the interesting part and the reason this is not a simple
// sum. Box 1 is gross less the pre-tax deductions that reduce federal wages;
// boxes 3 and 5 are gross less only the Section 125 items, because a 401(k)
// deferral is exempt from income tax and not from FICA. That difference is
// visible on every real W-2 of anybody who contributes to a 401(k), and getting
// it wrong would make this summary disagree with the form it is meant to help
// check — in a way that looks like the employer made the mistake.
func (e EmployerYear) W2(taxYear int) W2Summary {
	// Aggregate the employer's whole year into a single synthetic stub so the
	// per-stub wage-base logic is reused rather than reimplemented. Roll-ups use
	// the reconciled category totals, so a partial history still lands on the
	// employer's printed YTD figures where it has them.
	annual := Stub{Gross: e.Gross(), Net: e.Net()}
	for _, c := range Categories() {
		total := e.CategoryTotal(c)
		if total.IsZero() {
			continue
		}
		s := SpecFor(c)
		annual.Lines = append(annual.Lines, Line{
			Category:   c,
			Label:      s.Label,
			Amount:     total,
			PreTax:     e.categoryPreTax(c, s.PreTaxByDefault),
			IsEmployer: s.EmployerOnly,
		})
	}

	ficaWages := annual.FICAWages()
	boxes := []W2Box{
		{Box: "1", Label: "Wages, tips, other compensation", Amount: annual.FederalTaxableWages()},
		{Box: "2", Label: "Federal income tax withheld", Amount: e.CategoryTotal(CatFederalIncomeTax)},
		{Box: "3", Label: "Social Security wages", Amount: ficaWages},
		{Box: "4", Label: "Social Security tax withheld", Amount: e.CategoryTotal(CatSocialSecurity)},
		{Box: "5", Label: "Medicare wages and tips", Amount: ficaWages},
		{
			Box:   "6",
			Label: "Medicare tax withheld",
			// The additional Medicare surtax is withheld under the same box as
			// ordinary Medicare on a real W-2, so it is added here rather than
			// reported as a line the form does not have.
			Amount: e.CategoryTotal(CatMedicare).Add(e.CategoryTotal(CatMedicareSurtax)),
		},
		{Box: "16", Label: "State wages", Amount: annual.FederalTaxableWages()},
		{Box: "17", Label: "State income tax", Amount: e.CategoryTotal(CatStateIncomeTax)},
		{Box: "18", Label: "Local wages", Amount: annual.FederalTaxableWages()},
		{Box: "19", Label: "Local income tax", Amount: e.CategoryTotal(CatLocalIncomeTax)},
	}

	// Box 12 entries, in the taxonomy's order so two runs never disagree.
	for _, c := range Categories() {
		s := SpecFor(c)
		if s.W2Box12 == "" {
			continue
		}
		total := e.CategoryTotal(c)
		if total.IsZero() {
			continue
		}
		boxes = append(boxes, W2Box{
			Box: "12", Code: s.W2Box12, Label: s.Label, Amount: total,
		})
	}

	// Boxes with nothing in them are dropped, except the wage boxes: a zero in
	// box 1 is a statement, whereas an absent box 19 for somebody with no local
	// tax is just noise on the page.
	kept := boxes[:0]
	for _, b := range boxes {
		if b.Amount.IsZero() && b.Box != "1" && b.Box != "3" && b.Box != "5" {
			continue
		}
		kept = append(kept, b)
	}

	return W2Summary{
		EmployerKey:  e.Key,
		EmployerName: e.Name,
		TaxYear:      taxYear,
		Boxes:        kept,
		Disclaimer:   W2Disclaimer,
	}
}

// categoryPreTax reports whether an employer's lines in a category were
// recorded pre-tax. The stubs are the authority; the taxonomy default is used
// only when the employer has no line in that category at all, in which case the
// answer does not affect any figure.
//
// Any single pre-tax line makes the category pre-tax for the year. A category
// recorded both ways within one year is a data-entry inconsistency rather than
// a real payroll arrangement, and treating it as pre-tax matches what the
// employer's own YTD figures will have done.
func (e EmployerYear) categoryPreTax(c Category, fallback bool) bool {
	seen := false
	for _, s := range e.Stubs {
		for _, l := range s.Lines {
			if l.Category != c {
				continue
			}
			seen = true
			if l.PreTax {
				return true
			}
		}
	}
	if !seen {
		return fallback
	}
	return false
}
