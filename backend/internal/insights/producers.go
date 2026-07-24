package insights

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
)

// sharedUser is the "representative user" passed to the reporting queries.
//
// The report queries scope visibility as `(i.user_id = $2 OR i.is_shared)`.
// Insights are HOUSEHOLD-scoped and shown to every member, so detection must
// run with household-wide-but-shared-only visibility — never one member's
// private items, which would leak a partner's private spending into a feed the
// whole household reads. Passing uuid.Nil (no real item's user_id) collapses
// the OR to `i.is_shared`, giving exactly the shared-only visibility the
// aggregate alerts use. Items default to is_shared = TRUE, so a normal
// household still sees everything.
var sharedUser = uuid.Nil

// money formats a finished decimal for a narrative sentence. The number is
// never computed here — only decorated.
func money(d decimal.Decimal) string { return "$" + d.StringFixed(2) }

func monthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func monthEnd(start time.Time) time.Time { return start.AddDate(0, 1, -1) }

// --------------------------------------------------------------------------
// spending_spike — this month's category spend materially exceeds its trailing
// average. Reuses GetSpendingByCategory (this month) + GetCategoryAverages
// (trailing full months).
// --------------------------------------------------------------------------

const (
	spikeMultiplier     = 1.5 // this-month must be at least this × the average
	spikeFloorDollars   = 50  // …and above a dollar floor, so small categories stay quiet
	spikeTrailingMonths = 6
)

type spendingSpikeProducer struct{}

func (spendingSpikeProducer) Kind() string { return "spending_spike" }

func (spendingSpikeProducer) Detect(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time) ([]Candidate, error) {
	mStart := monthStart(now)

	thisMonth, err := q.GetSpendingByCategory(ctx, dbgen.GetSpendingByCategoryParams{
		HouseholdID: householdID, UserID: sharedUser, Date: mStart, Date_2: monthEnd(mStart),
	})
	if err != nil {
		return nil, err
	}

	// Average over prior FULL months only, so the current partial month never
	// dilutes its own baseline.
	averages, err := q.GetCategoryAverages(ctx, dbgen.GetCategoryAveragesParams{
		HouseholdID: householdID, UserID: sharedUser,
		Date: mStart.AddDate(0, -spikeTrailingMonths, 0), Date_2: mStart.AddDate(0, 0, -1),
	})
	if err != nil {
		return nil, err
	}
	avgBySlug := make(map[string]decimal.Decimal, len(averages))
	for _, a := range averages {
		avgBySlug[a.CategorySlug] = a.MonthlyAverage
	}

	floor := decimal.NewFromInt(spikeFloorDollars)
	mult := decimal.NewFromFloat(spikeMultiplier)

	var out []Candidate
	for _, c := range thisMonth {
		avg, ok := avgBySlug[c.CategorySlug]
		if !ok || !avg.IsPositive() {
			continue
		}
		if c.Total.LessThan(floor) || c.Total.LessThan(avg.Mul(mult)) {
			continue
		}

		over := c.Total.Sub(avg)
		ratio := c.Total.Div(avg)
		period := mStart
		out = append(out, Candidate{
			Kind:     "spending_spike",
			Priority: clampPriority(int(ratio.InexactFloat64())+1, 2, 5),
			Title:    fmt.Sprintf("%s spending is up this month", c.CategoryName),
			Body: fmt.Sprintf(
				"You've spent %s on %s so far this month, versus a typical %s — about %s more than usual.",
				money(c.Total), c.CategoryName, money(avg), money(over)),
			Data: map[string]any{
				"category":      c.CategoryName,
				"category_slug": c.CategorySlug,
				"this_month":    c.Total.StringFixed(2),
				"average":       avg.StringFixed(2),
				"over":          over.StringFixed(2),
			},
			Period:    &period,
			DedupeKey: fmt.Sprintf("spending_spike:%s:%s", c.CategorySlug, mStart.Format("2006-01")),
		})
	}
	return out, nil
}

// --------------------------------------------------------------------------
// new_recurring — a currently-active recurring merchant. Reuses
// GetRecurringMerchants; deduped on merchant_key so each subscription surfaces
// once. Only merchants seen recently qualify, so a cancelled subscription does
// not keep resurfacing.
// --------------------------------------------------------------------------

const (
	recurringLookbackMonths = 12
	recurringActiveDays     = 45
)

// daysPerMonth normalises a cadence to a per-month figure, matching the
// recurring report endpoint exactly.
var daysPerMonth = decimal.NewFromFloat(30.4368)

type newRecurringProducer struct{}

func (newRecurringProducer) Kind() string { return "new_recurring" }

func (newRecurringProducer) Detect(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time) ([]Candidate, error) {
	rows, err := q.GetRecurringMerchants(ctx, dbgen.GetRecurringMerchantsParams{
		HouseholdID: householdID, UserID: sharedUser,
		Date: now.AddDate(0, -recurringLookbackMonths, 0),
	})
	if err != nil {
		return nil, err
	}

	activeCutoff := now.AddDate(0, 0, -recurringActiveDays)

	var out []Candidate
	for _, m := range rows {
		if m.MerchantKey == nil || m.LastSeen.Before(activeCutoff) {
			continue
		}

		var monthly decimal.Decimal
		if m.AvgGapDays.IsPositive() {
			monthly = m.AverageAmount.Mul(daysPerMonth).Div(m.AvgGapDays).Round(2)
		}
		avg := m.AverageAmount.Round(2)
		out = append(out, Candidate{
			Kind:     "new_recurring",
			Priority: 1,
			Title:    fmt.Sprintf("Recurring charge from %s", m.Merchant),
			Body: fmt.Sprintf(
				"%s looks like a recurring charge — about %s each time, roughly %s per month.",
				m.Merchant, money(avg), money(monthly)),
			Data: map[string]any{
				"merchant":         m.Merchant,
				"merchant_key":     *m.MerchantKey,
				"average_amount":   avg.StringFixed(2),
				"monthly_estimate": monthly.StringFixed(2),
				"cadence":          cadenceLabel(m.AvgGapDays),
				"occurrences":      m.Occurrences,
				"last_seen":        m.LastSeen.Format(time.DateOnly),
			},
			DedupeKey: "new_recurring:" + *m.MerchantKey,
		})
	}
	return out, nil
}

// cadenceLabel mirrors the recurring report's buckets; the detection query only
// returns gaps in the 6–40 day band.
func cadenceLabel(avgGap decimal.Decimal) string {
	switch days := avgGap.InexactFloat64(); {
	case days < 10:
		return "weekly"
	case days < 20:
		return "every 2 weeks"
	default:
		return "monthly"
	}
}

// --------------------------------------------------------------------------
// budget_pace — a budget on track to blow this month. Reuses GetBudgetProgress;
// compares spend to budget × the elapsed fraction of the month, and only fires
// when the projected end-of-month spend would exceed the budget. Zero
// candidates when the household has no budgets.
// --------------------------------------------------------------------------

const budgetPaceMargin = 1.15 // spend must run at least this far ahead of pace

type budgetPaceProducer struct{}

func (budgetPaceProducer) Kind() string { return "budget_pace" }

func (budgetPaceProducer) Detect(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time) ([]Candidate, error) {
	now = now.UTC()
	mStart := monthStart(now)
	mEnd := monthEnd(mStart)

	rows, err := q.GetBudgetProgress(ctx, dbgen.GetBudgetProgressParams{
		HouseholdID: householdID, UserID: sharedUser,
		WindowStart: mStart, WindowEnd: mEnd, Ref: now,
	})
	if err != nil {
		return nil, err
	}

	// Elapsed fraction of the month, e.g. day 15 of 30 = 0.5. Guard the first of
	// the month so nothing divides by zero.
	fraction := decimal.NewFromInt(int64(now.Day())).Div(decimal.NewFromInt(int64(mEnd.Day())))
	if !fraction.IsPositive() {
		return nil, nil
	}
	margin := decimal.NewFromFloat(budgetPaceMargin)

	var out []Candidate
	for _, b := range rows {
		// This is a month-pace signal; a weekly or yearly budget's spend is over a
		// different window, so the month fraction would misjudge it.
		if b.Period != "monthly" {
			continue
		}
		if !b.Budgeted.IsPositive() || !b.Spent.IsPositive() {
			continue
		}
		if b.Spent.LessThan(b.Budgeted.Mul(fraction).Mul(margin)) {
			continue
		}
		projected := b.Spent.Div(fraction).Round(2)
		if projected.LessThanOrEqual(b.Budgeted) {
			continue
		}

		over := projected.Sub(b.Budgeted)
		period := mStart
		out = append(out, Candidate{
			Kind:     "budget_pace",
			Priority: 3,
			Title:    fmt.Sprintf("%s budget is running hot", b.CategoryName),
			Body: fmt.Sprintf(
				"You've spent %s of your %s %s budget with part of the month left. At this pace you're on track for about %s — roughly %s over.",
				money(b.Spent), money(b.Budgeted), b.CategoryName, money(projected), money(over)),
			Data: map[string]any{
				"category":      b.CategoryName,
				"category_slug": b.CategorySlug,
				"budgeted":      b.Budgeted.StringFixed(2),
				"spent":         b.Spent.StringFixed(2),
				"projected":     projected.StringFixed(2),
				"over":          over.StringFixed(2),
			},
			Period:    &period,
			DedupeKey: fmt.Sprintf("budget_pace:%s:%s", b.CategoryID, mStart.Format("2006-01")),
		})
	}
	return out, nil
}

// --------------------------------------------------------------------------
// low_leftover — this month's leftover (income − spending) is low or negative.
// Reuses GetSpendingSummary. Priority scales with how negative it is.
// --------------------------------------------------------------------------

// lowLeftoverFraction is the share of income below which the month's leftover
// is worth flagging.
var lowLeftoverFraction = decimal.NewFromFloat(0.1)

type lowLeftoverProducer struct{}

func (lowLeftoverProducer) Kind() string { return "low_leftover" }

func (lowLeftoverProducer) Detect(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time) ([]Candidate, error) {
	mStart := monthStart(now)

	s, err := q.GetSpendingSummary(ctx, dbgen.GetSpendingSummaryParams{
		HouseholdID: householdID, UserID: sharedUser, Date: mStart, Date_2: monthEnd(mStart),
	})
	if err != nil {
		return nil, err
	}

	// Only meaningful once income has landed for the month; otherwise a fresh
	// month always reads as "negative leftover".
	if !s.Income.IsPositive() {
		return nil, nil
	}
	leftover := s.Income.Sub(s.Spending)
	if leftover.GreaterThanOrEqual(s.Income.Mul(lowLeftoverFraction)) {
		return nil, nil
	}

	priority := 3
	var body string
	if leftover.IsNegative() {
		priority = 5
		body = fmt.Sprintf(
			"You've spent %s against %s of income this month — %s more than came in.",
			money(s.Spending), money(s.Income), money(leftover.Neg()))
	} else {
		body = fmt.Sprintf(
			"You've spent %s against %s of income this month, leaving just %s.",
			money(s.Spending), money(s.Income), money(leftover))
	}

	period := mStart
	return []Candidate{{
		Kind:     "low_leftover",
		Priority: priority,
		Title:    "Not much left this month",
		Body:     body,
		Data: map[string]any{
			"income":   s.Income.StringFixed(2),
			"spending": s.Spending.StringFixed(2),
			"leftover": leftover.StringFixed(2),
		},
		Period:    &period,
		DedupeKey: "low_leftover:" + mStart.Format("2006-01"),
	}}, nil
}

// clampPriority keeps a computed priority within a sane band.
func clampPriority(p, lo, hi int) int {
	if p < lo {
		return lo
	}
	if p > hi {
		return hi
	}
	return p
}
