package likelihood

import (
	"fmt"
	"time"

	"github.com/shopspring/decimal"
)

// Plan tracking: what the plan said would go in, what actually did, and what the
// difference costs at the horizon.
//
// THE POINT OF THE WHOLE FEATURE is that an accepted plan stops being a
// projection and becomes a relationship. A plan nobody reconciles is a decision
// made once and never revisited, which is exactly the failure a real advisor
// exists to prevent.
//
// ACTUALS ARE NEVER STORED. They are read live from investment_transactions,
// account_balance_history and transactions every time this runs, so editing a
// contribution recorded three months ago corrects the drift immediately and
// without a backfill. What IS stored is the plan's EXPECTED position, because
// recomputing that means replaying inputs against assumptions the household can
// edit — and a drift figure whose expected side silently moved is worse than no
// drift figure.

// ReconcileInput is one reconciliation. It is pure data: this package reads no
// database, matching allocation's split between AssembleBaseline and Run.
type ReconcileInput struct {
	// Since is when the plan was accepted — the start of the tracking window.
	Since time.Time
	// AsOf is the snapshot date, normally today.
	AsOf time.Time

	// Lump is the plan's one-off amount, expected to have landed at Since.
	Lump decimal.Decimal
	// Buckets is the plan's per-account expectation and what actually arrived.
	Buckets []BucketActual

	// RealReturnRate is the household's assumed real return, used to compound
	// the monthly shortfall forward. A FRACTION.
	RealReturnRate decimal.Decimal
	// HorizonMonths is the plan's full horizon from Since, so the remaining term
	// is what the re-projection compounds over.
	HorizonMonths int
	// DriftThreshold is the monthly gap past which the plan is reported as
	// behind rather than on track. Money.
	DriftThreshold decimal.Decimal
}

// BucketActual pairs one bucket's expectation with what really happened.
type BucketActual struct {
	AccountID string
	Name      string
	// ExpectedLump and ExpectedMonthly are what the plan directed here.
	ExpectedLump    decimal.Decimal
	ExpectedMonthly decimal.Decimal
	// Actual is the money that actually arrived in this bucket across the whole
	// window, lump included.
	Actual decimal.Decimal
	// Tracked is false when this bucket has no usable trail — a Plaid-linked
	// cash or debt account with no balance history and no investment
	// transactions. It is reported as UNTRACKED, never as zero: "we cannot see
	// what you paid in" and "you paid in nothing" are opposite findings, and
	// collapsing them would tell a household it had missed contributions it
	// actually made.
	Tracked bool
}

// BucketDrift is one bucket's plan-vs-actual answer.
type BucketDrift struct {
	AccountID string `json:"account_id"`
	Name      string `json:"name"`

	ExpectedToDate decimal.Decimal `json:"expected_to_date"`
	ActualToDate   decimal.Decimal `json:"actual_to_date"`
	// Drift is actual − expected: NEGATIVE means behind, which is the direction
	// that matters. Signed rather than absolute so "ahead" is expressible.
	Drift decimal.Decimal `json:"drift"`
	// MonthlyDrift is the same gap expressed per month, which is the figure a
	// household can act on: "$180/mo behind" is a decision, "$2,160 behind" is
	// a number.
	MonthlyDrift decimal.Decimal `json:"monthly_drift"`

	Tracked bool   `json:"tracked"`
	Note    string `json:"note,omitempty"`
}

// Tracking is the whole drift report.
type Tracking struct {
	AsOf  string `json:"as_of"`
	Since string `json:"since"`
	// Months is whole months elapsed in the window.
	Months int `json:"months"`

	ExpectedLump   decimal.Decimal `json:"expected_lump"`
	ExpectedToDate decimal.Decimal `json:"expected_to_date"`
	ActualToDate   decimal.Decimal `json:"actual_to_date"`
	Drift          decimal.Decimal `json:"drift"`
	MonthlyDrift   decimal.Decimal `json:"monthly_drift"`

	// OnTrack is false when the monthly shortfall exceeds the threshold. A plan
	// that is AHEAD is on track — this is a floor, not a band.
	OnTrack bool `json:"on_track"`

	// ProjectedShortfall is what the current monthly gap compounds to by the
	// horizon, if nothing changes. Zero when the plan is on or ahead of track.
	ProjectedShortfall decimal.Decimal `json:"projected_shortfall"`
	RemainingMonths    int             `json:"remaining_months"`

	Buckets []BucketDrift `json:"buckets"`

	// Untracked names buckets with no usable trail, so a partial answer says so
	// rather than presenting itself as complete.
	Untracked []string `json:"untracked"`

	// Summary is the one line the insight and the UI both render.
	Summary  string `json:"summary"`
	Basis    string `json:"basis"`
	Estimate bool   `json:"estimate"`
}

const trackingBasis = "Expected contributions are replayed from the plan's own inputs; actuals are read live from " +
	"your recorded transactions and balance history every time this is opened, so correcting a past contribution " +
	"corrects the drift. The projected shortfall compounds the CURRENT monthly gap forward at your assumed real " +
	"return for the rest of the horizon — it assumes nothing changes, which is the one thing you can change."

// Reconcile computes plan-vs-actual drift and what it costs at the horizon.
//
// A pure function of its input: no clock, no I/O. `AsOf` is injected so a
// reconciliation is reproducible and so tests are not at the mercy of the
// calendar.
func Reconcile(in ReconcileInput) Tracking {
	months := wholeMonthsBetween(in.Since, in.AsOf)
	out := Tracking{
		AsOf:      in.AsOf.Format(time.DateOnly),
		Since:     in.Since.Format(time.DateOnly),
		Months:    months,
		Buckets:   make([]BucketDrift, 0, len(in.Buckets)),
		Untracked: []string{},
		Basis:     trackingBasis,
		Estimate:  true,
	}
	out.ExpectedLump = in.Lump.Round(2)

	remaining := in.HorizonMonths - months
	if remaining < 0 {
		remaining = 0
	}
	out.RemainingMonths = remaining

	monthsDec := decimal.NewFromInt(int64(months))
	for _, b := range in.Buckets {
		// The lump is expected to have landed at the start of the window, so it
		// counts from month zero. The monthly is expected once per WHOLE month
		// elapsed — a plan two weeks old expects no monthly contribution yet,
		// and reporting one as missed would put every new plan behind on the
		// day it was made.
		expected := b.ExpectedLump.Add(b.ExpectedMonthly.Mul(monthsDec)).Round(2)

		d := BucketDrift{
			AccountID:      b.AccountID,
			Name:           b.Name,
			ExpectedToDate: expected,
			ActualToDate:   b.Actual.Round(2),
			Tracked:        b.Tracked,
		}
		if !b.Tracked {
			// Nothing is claimed about an untracked bucket: no drift, no
			// contribution to the totals, and a note saying why.
			d.ActualToDate = decimal.Zero
			d.Note = "No contribution trail for this account, so nothing is claimed about it. " +
				"Recording contributions or balances against it will bring it into the comparison."
			out.Untracked = append(out.Untracked, b.Name)
			out.Buckets = append(out.Buckets, d)
			continue
		}

		d.Drift = d.ActualToDate.Sub(expected).Round(2)
		if months > 0 {
			d.MonthlyDrift = d.Drift.Div(monthsDec).Round(2)
		}
		out.ExpectedToDate = out.ExpectedToDate.Add(expected)
		out.ActualToDate = out.ActualToDate.Add(d.ActualToDate)
		out.Buckets = append(out.Buckets, d)
	}

	out.ExpectedToDate = out.ExpectedToDate.Round(2)
	out.ActualToDate = out.ActualToDate.Round(2)
	out.Drift = out.ActualToDate.Sub(out.ExpectedToDate).Round(2)
	if months > 0 {
		out.MonthlyDrift = out.Drift.Div(monthsDec).Round(2)
	}

	threshold := in.DriftThreshold
	if !threshold.IsPositive() {
		threshold = decimal.Zero
	}
	// Behind by more than the threshold. Ahead is on track: this is a floor, not
	// a band, because nobody needs an alert telling them they saved too much.
	out.OnTrack = out.MonthlyDrift.Neg().LessThanOrEqual(threshold)

	if out.MonthlyDrift.IsNegative() && remaining > 0 {
		out.ProjectedShortfall = compoundMonthly(
			out.MonthlyDrift.Neg(), in.RealReturnRate, remaining).Round(2)
	}

	out.Summary = summarise(out)
	return out
}

// compoundMonthly is the future value of a level monthly contribution, using the
// SAME convention as the projection engine: a real annual rate divided by
// twelve, growth applied to the balance before each month's deposit.
//
// Deliberately the same arithmetic as ProjectRetirement's month loop rather than
// a closed-form annuity factor. The closed form differs by the timing of the
// first deposit, and a shortfall figure that disagreed with the projection it
// sits beside is exactly the two-surfaces-disagreeing failure doc 24 names.
func compoundMonthly(monthly, annualRate decimal.Decimal, months int) decimal.Decimal {
	rate := decimal.Zero
	if annualRate.IsPositive() {
		rate = annualRate.Div(decimal.NewFromInt(12))
	}
	balance := decimal.Zero
	for m := 0; m < months; m++ {
		balance = balance.Add(balance.Mul(rate))
		balance = balance.Add(monthly)
		// Rounded to the cent each month. Left unrounded the running balance
		// grows a few digits of phantom precision per step and reaches several
		// thousand by a long horizon, which is slow and no more accurate.
		balance = balance.Round(2)
	}
	return balance
}

// wholeMonthsBetween counts completed months, never partial ones. A plan made on
// the 3rd is one month old on the 3rd of next month and not before.
func wholeMonthsBetween(from, to time.Time) int {
	if !to.After(from) {
		return 0
	}
	months := int(to.Year()-from.Year())*12 + int(to.Month()) - int(from.Month())
	if to.Day() < from.Day() {
		months--
	}
	if months < 0 {
		months = 0
	}
	return months
}

// summarise writes the line the insight feed and the tracker panel both render.
func summarise(t Tracking) string {
	if t.Months == 0 {
		return "This plan is less than a month old, so there is nothing to compare yet."
	}
	if len(t.Buckets) == len(t.Untracked) {
		return "None of this plan's buckets has a contribution trail yet, so there is nothing to compare. " +
			"Record contributions or balances against them to start tracking."
	}
	var s string
	switch {
	case t.MonthlyDrift.IsPositive():
		s = fmt.Sprintf(
			"On track — you're putting in $%s a month MORE than this plan called for, $%s ahead over %d months.",
			t.MonthlyDrift.StringFixed(2), t.Drift.StringFixed(2), t.Months)

	case t.MonthlyDrift.IsZero():
		s = fmt.Sprintf("On track — contributions match this plan over %d months.", t.Months)

	default:
		behind := t.MonthlyDrift.Neg()
		s = fmt.Sprintf("You're $%s a month behind this plan, $%s short over %d months.",
			behind.StringFixed(2), t.Drift.Neg().StringFixed(2), t.Months)
		if t.ProjectedShortfall.IsPositive() {
			s += fmt.Sprintf(" At that rate you land about $%s short at the horizon.",
				t.ProjectedShortfall.StringFixed(2))
		}
	}

	// THE DISCLOSURE ATTACHES TO EVERY OUTCOME, not only to the bad one. A
	// household reading "on track" over a comparison that silently omitted two
	// of its five buckets has been told something the figures do not support,
	// and "on track" is the reading where an unnoticed gap does the most damage.
	if len(t.Untracked) > 0 {
		s += fmt.Sprintf(" Note: %d bucket(s) have no contribution trail and are left out of this comparison"+
			" entirely — %v.", len(t.Untracked), t.Untracked)
	}
	return s
}
