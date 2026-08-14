// Package obligations is the bill calendar's domain layer: what is due, when,
// and what that does to a balance.
//
// The division of labour is deliberate and worth stating once.
//
//	Date arithmetic lives in SQL. ListUpcomingObligations expands an
//	(anchor_date, count, unit) cadence into occurrences, because Postgres
//	interval addition clamps month ends the way a person reads them
//	(2025-01-31 + interval '1 month' = 2025-02-28) while Go's time.AddDate
//	normalises the overflow instead (2025-03-03). Nothing in this package
//	derives a due date.
//
//	Money arithmetic lives here, in shopspring/decimal. Balances are projected
//	by exact subtraction, never in the frontend and never through a float.
//
// A projection here is only ever a projection from KNOWN obligations. It does
// not predict discretionary spending and must never be presented as if it did.
package obligations

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// Interval units, matching the interval_unit CHECK on recurring_obligations.
const (
	UnitDay   = "day"
	UnitWeek  = "week"
	UnitMonth = "month"
	UnitYear  = "year"
)

// Units lists every cadence unit the expansion understands, for validation and
// for the entry form.
var Units = []string{UnitDay, UnitWeek, UnitMonth, UnitYear}

// IsValidUnit reports whether u is a cadence unit the expansion understands.
func IsValidUnit(u string) bool {
	switch u {
	case UnitDay, UnitWeek, UnitMonth, UnitYear:
		return true
	default:
		return false
	}
}

// Cadence is "every Count Units" — the whole of an obligation's recurrence rule.
type Cadence struct {
	Count int32
	Unit  string
}

// Label renders a cadence the way the UI says it, so the wording is decided once
// rather than in each surface that shows a bill.
func (c Cadence) Label() string {
	switch {
	case c.Count == 1 && c.Unit == UnitDay:
		return "daily"
	case c.Count == 1 && c.Unit == UnitWeek:
		return "weekly"
	case c.Count == 2 && c.Unit == UnitWeek:
		return "every 2 weeks"
	case c.Count == 1 && c.Unit == UnitMonth:
		return "monthly"
	case c.Count == 3 && c.Unit == UnitMonth:
		return "quarterly"
	case c.Count == 6 && c.Unit == UnitMonth:
		return "twice a year"
	case c.Count == 1 && c.Unit == UnitYear:
		return "yearly"
	case c.Count == 1:
		return "every " + c.Unit
	}
	return fmt.Sprintf("every %d %ss", c.Count, c.Unit)
}

// MaxDays is the longest a single period of this cadence can run, in days. It is
// an upper bound, not an exact length — its only job is to bound the window a
// caller must search to be certain of finding the next occurrence, so months and
// years round up to their longest possible form.
func (c Cadence) MaxDays() int {
	n := int(c.Count)
	switch c.Unit {
	case UnitDay:
		return n
	case UnitWeek:
		return 7 * n
	case UnitMonth:
		return 31 * n
	case UnitYear:
		return 366 * n
	}
	return 0
}

// daysPerMonth normalises any cadence to a comparable monthly figure. 30.4375 is
// the mean Gregorian month (365.25 / 12), which keeps a weekly bill and a yearly
// bill on the same footing.
var daysPerMonth = decimal.RequireFromString("30.4375")

// MonthlyEstimate is what one obligation costs per month on average. This is a
// display and ranking figure — the calendar and the projection always use the
// real occurrences, never this.
func MonthlyEstimate(amount decimal.Decimal, c Cadence) decimal.Decimal {
	if c.Count <= 0 {
		return decimal.Zero
	}
	switch c.Unit {
	case UnitMonth:
		// Exact: n months per period, so amount/n per month.
		return amount.Div(decimal.NewFromInt(int64(c.Count))).Round(2)
	case UnitYear:
		return amount.Div(decimal.NewFromInt(int64(c.Count) * 12)).Round(2)
	}
	// Day and week cadences have an exact length in days, so MaxDays is the
	// real period here, not an upper bound.
	days := decimal.NewFromInt(int64(c.MaxDays()))
	if !days.IsPositive() {
		return decimal.Zero
	}
	return amount.Mul(daysPerMonth).Div(days).Round(2)
}

// AmountRange is what a household said to expect a bill to cost (MAD-120). It is
// a tolerance around an obligation's Amount, not a replacement for it: the
// projection and the monthly estimate still work from Amount, and a bill with no
// range stated is the ordinary case.
//
// Both bounds are always present — the column CHECK enforces both-or-neither —
// so a non-nil *AmountRange is always a complete, usable range.
type AmountRange struct {
	Min decimal.Decimal
	Max decimal.Decimal
}

// Contains reports whether a charge is the amount the household expected.
func (r AmountRange) Contains(d decimal.Decimal) bool {
	return !d.LessThan(r.Min) && !d.GreaterThan(r.Max)
}

// Label renders a range the way every surface says it, so the wording is decided
// once rather than in each page that shows a bill.
func (r AmountRange) Label() string {
	return fmt.Sprintf("%s–%s", r.Min.StringFixed(2), r.Max.StringFixed(2))
}

// rangeFrom rebuilds an AmountRange from the nullable column pair, returning nil
// when no range was stated. A half-populated pair cannot reach here (the table's
// CHECK rejects it), so a single bound present is treated as no range rather
// than guessed at.
func rangeFrom(min, max decimal.NullDecimal) *AmountRange {
	if !min.Valid || !max.Valid {
		return nil
	}
	return &AmountRange{Min: min.Decimal, Max: max.Decimal}
}

// Occurrence is one obligation falling due on one date. Several occurrences can
// share an ObligationID — that is the point of expansion.
type Occurrence struct {
	ObligationID uuid.UUID
	Label        string
	Amount       decimal.Decimal
	// Range is the stated expected range, nil when the member stated none.
	Range       *AmountRange
	CategoryID  *uuid.UUID
	AccountID   *uuid.UUID
	Source      string
	MerchantKey *string
	Cadence     Cadence
	DueDate     time.Time
}

// ListUpcoming expands every obligation visible to userID into its occurrences
// inside [from, to] inclusive. Pass uuid.Nil for userID to get the
// household-shared view (what safe-to-spend and the insight feed use).
//
// This is the single expansion entry point. The calendar, the list, the
// projection, the safe-to-spend split and the upcoming-bill insight all come
// through here so they cannot disagree about when a bill is due.
func ListUpcoming(
	ctx context.Context,
	q *dbgen.Queries,
	householdID, userID uuid.UUID,
	from, to time.Time,
) ([]Occurrence, error) {
	rows, err := q.ListUpcomingObligations(ctx, dbgen.ListUpcomingObligationsParams{
		HouseholdID: householdID,
		UserID:      &userID,
		Column3:     dateOnly(from),
		Column4:     dateOnly(to),
	})
	if err != nil {
		return nil, fmt.Errorf("expand obligations: %w", err)
	}

	out := make([]Occurrence, 0, len(rows))
	for _, r := range rows {
		out = append(out, Occurrence{
			ObligationID: r.ObligationID,
			Label:        r.Label,
			Amount:       r.Amount,
			Range:        rangeFrom(r.AmountMin, r.AmountMax),
			CategoryID:   r.CategoryID,
			AccountID:    r.AccountID,
			Source:       r.Source,
			MerchantKey:  r.MerchantKey,
			Cadence:      Cadence{Count: r.IntervalCount, Unit: r.IntervalUnit},
			DueDate:      r.DueDate.UTC(),
		})
	}
	return out, nil
}

// NextDue returns, for each obligation in rows, the first occurrence on or after
// `from` — nil when there is none (an ended or expired obligation).
//
// It deliberately does NOT compute dates itself. It asks ListUpcoming for a
// window wide enough that the answer must lie inside it, and takes the first hit
// per obligation. The window is one full period past `from` (the next occurrence
// of a live obligation is always within one period) extended to cover any
// obligation whose anchor is still in the future, so a biennial renewal anchored
// three years out is found exactly as reliably as a monthly bill.
func NextDue(
	ctx context.Context,
	q *dbgen.Queries,
	householdID, userID uuid.UUID,
	rows []dbgen.RecurringObligation,
	from time.Time,
) (map[uuid.UUID]time.Time, error) {
	from = dateOnly(from)
	to := from
	for _, o := range rows {
		if !o.IsActive {
			continue
		}
		c := Cadence{Count: o.IntervalCount, Unit: o.IntervalUnit}
		if end := from.AddDate(0, 0, c.MaxDays()+1); end.After(to) {
			to = end
		}
		if anchor := dateOnly(o.AnchorDate); anchor.After(to) {
			to = anchor
		}
	}
	if !to.After(from) {
		return map[uuid.UUID]time.Time{}, nil
	}

	occurrences, err := ListUpcoming(ctx, q, householdID, userID, from, to)
	if err != nil {
		return nil, err
	}
	// ListUpcoming returns due_date ascending, so the first sighting of an
	// obligation is its next occurrence.
	next := make(map[uuid.UUID]time.Time, len(rows))
	for _, o := range occurrences {
		if _, seen := next[o.ObligationID]; !seen {
			next[o.ObligationID] = o.DueDate
		}
	}
	return next, nil
}

// dateOnly strips a timestamp to its UTC calendar date, matching how Postgres
// DATE columns round-trip through this codebase.
func dateOnly(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
