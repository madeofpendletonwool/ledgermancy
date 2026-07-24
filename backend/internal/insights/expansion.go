package insights

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
)

// This file holds the "expansion" producers added on top of the original feed:
// a forward projection of the month's cash flow, an outsized-transaction flag,
// income-change detection, and savings-rate milestones. Each is deterministic —
// every number is finished in SQL / decimal and stashed in Data as a
// StringFixed(2) string — and reuses the existing report queries wherever
// possible. Goal-progress nudges are already covered by goalProducer (goal.go),
// so no separate producer is added for them.

// --------------------------------------------------------------------------
// month_end_projection — a forward look at the current month's cash flow. Where
// low_leftover reports realized leftover after income has landed, this projects
// the spending run-rate to month-end against typical income, so a squeeze can be
// flagged mid-month while there is still time to act.
// --------------------------------------------------------------------------

const (
	// The projection is only meaningful once enough of the month has elapsed to
	// extrapolate, and it hands off to the realized low_leftover flag late in the
	// month, so it stays quiet outside this window.
	projectionMinDay = 7
	projectionMaxDay = 24
	// Prior full months averaged for "typical income".
	projectionIncomeMonths = 6
)

// projectionSqueezeFraction is the share of typical income below which a
// projected leftover is worth flagging.
var projectionSqueezeFraction = decimal.NewFromFloat(0.1)

type monthEndProjectionProducer struct{}

func (monthEndProjectionProducer) Kind() string { return "month_end_projection" }

func (monthEndProjectionProducer) Detect(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time) ([]Candidate, error) {
	mStart := monthStart(now)
	mEnd := monthEnd(mStart)
	day := now.UTC().Day()
	daysInMonth := mEnd.Day()
	if day < projectionMinDay || day > projectionMaxDay {
		return nil, nil
	}

	// Spend so far this month (up to and including today).
	s, err := q.GetSpendingSummary(ctx, dbgen.GetSpendingSummaryParams{
		HouseholdID: householdID, UserID: sharedUser, Date: mStart, Date_2: now,
	})
	if err != nil {
		return nil, err
	}
	if !s.Spending.IsPositive() {
		return nil, nil
	}

	// Typical income: average of prior full months' income. Lumpy paychecks make
	// this-month income unreliable, so the projection leans on history.
	trend, err := q.GetMonthlyTrend(ctx, dbgen.GetMonthlyTrendParams{
		HouseholdID: householdID, UserID: sharedUser,
		Date: mStart.AddDate(0, -projectionIncomeMonths, 0), Date_2: mStart.AddDate(0, 0, -1),
	})
	if err != nil {
		return nil, err
	}
	sumIncome := decimal.Zero
	n := 0
	for _, m := range trend {
		if m.Income.IsPositive() {
			sumIncome = sumIncome.Add(m.Income)
			n++
		}
	}
	if n == 0 {
		return nil, nil
	}
	expectedIncome := sumIncome.Div(decimal.NewFromInt(int64(n)))

	// Straight-line projection: spend-so-far scaled to the full month.
	projected := s.Spending.
		Mul(decimal.NewFromInt(int64(daysInMonth))).
		Div(decimal.NewFromInt(int64(day)))
	projectedLeftover := expectedIncome.Sub(projected)

	// Only a projected squeeze is worth a heads-up.
	if projectedLeftover.GreaterThanOrEqual(expectedIncome.Mul(projectionSqueezeFraction)) {
		return nil, nil
	}

	priority := 4
	var body string
	if projectedLeftover.IsNegative() {
		priority = 5
		body = fmt.Sprintf(
			"At this pace you're on track to spend about %s this month against a typical %s of income — roughly %s more than comes in. There's still time to ease off.",
			money(projected.Round(2)), money(expectedIncome.Round(2)), money(projectedLeftover.Neg().Round(2)))
	} else {
		body = fmt.Sprintf(
			"At this pace you're on track to spend about %s this month against a typical %s of income, leaving only about %s.",
			money(projected.Round(2)), money(expectedIncome.Round(2)), money(projectedLeftover.Round(2)))
	}

	period := mStart
	return []Candidate{{
		Kind:     "month_end_projection",
		Priority: priority,
		Title:    "On track to overspend this month",
		Body:     body,
		Data: map[string]any{
			"spent_so_far":       s.Spending.StringFixed(2),
			"projected_spending": projected.Round(2).StringFixed(2),
			"expected_income":    expectedIncome.Round(2).StringFixed(2),
			"projected_leftover": projectedLeftover.Round(2).StringFixed(2),
			"as_of_day":          day,
			"days_in_month":      daysInMonth,
		},
		Period:    &period,
		DedupeKey: "month_end_projection:" + mStart.Format("2006-01"),
	}}, nil
}

// --------------------------------------------------------------------------
// large_transaction — a single purchase far above the household's typical one.
// Reuses GetLargestTransactions for the recent window and compares each charge
// to GetAverageSpendingTransaction in Go; both figures are authoritative from
// SQL, the producer only picks a threshold.
// --------------------------------------------------------------------------

const (
	largeTxnBaselineMonths = 6
	largeTxnRecentDays     = 14
	largeTxnMultiple       = 4   // a charge this many × the typical purchase…
	largeTxnFloorDollars   = 150 // …and at least this many dollars, so tiny baselines stay quiet
	largeTxnLimit          = 5
)

type largeTransactionProducer struct{}

func (largeTransactionProducer) Kind() string { return "large_transaction" }

func (largeTransactionProducer) Detect(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time) ([]Candidate, error) {
	base, err := q.GetAverageSpendingTransaction(ctx, dbgen.GetAverageSpendingTransactionParams{
		HouseholdID: householdID, UserID: sharedUser,
		Date: now.AddDate(0, -largeTxnBaselineMonths, 0),
	})
	if err != nil {
		return nil, err
	}
	avg := base.AvgAmount

	// Threshold: the larger of "N× typical" and the absolute floor.
	threshold := avg.Mul(decimal.NewFromInt(largeTxnMultiple))
	floor := decimal.NewFromInt(largeTxnFloorDollars)
	if threshold.LessThan(floor) {
		threshold = floor
	}

	txns, err := q.GetLargestTransactions(ctx, dbgen.GetLargestTransactionsParams{
		HouseholdID: householdID, UserID: sharedUser,
		Date: now.AddDate(0, 0, -largeTxnRecentDays), Date_2: now, Limit: largeTxnLimit,
	})
	if err != nil {
		return nil, err
	}

	var out []Candidate
	for _, t := range txns {
		// GetLargestTransactions is amount-descending, so the first under-threshold
		// row means the rest are too.
		if t.Amount.LessThan(threshold) {
			break
		}
		dateStr := t.Date.Format(time.DateOnly)
		body := fmt.Sprintf(
			"A %s charge at %s on %s is well above your typical purchase of about %s.",
			money(t.Amount.Round(2)), t.Merchant, t.Date.Format("Jan 2"), money(avg.Round(2)))
		out = append(out, Candidate{
			Kind:     "large_transaction",
			Priority: 4,
			Title:    fmt.Sprintf("Large purchase at %s", t.Merchant),
			Body:     body,
			Data: map[string]any{
				"merchant": t.Merchant,
				"amount":   t.Amount.StringFixed(2),
				"date":     dateStr,
				"category": t.CategoryName,
				"typical":  avg.Round(2).StringFixed(2),
			},
			// No transaction id in the projection, so key on the charge's identity;
			// the same charge always yields the same key and is never re-raised.
			DedupeKey: fmt.Sprintf("large_transaction:%s:%s:%s", t.Merchant, dateStr, t.Amount.StringFixed(2)),
		})
	}
	return out, nil
}

// --------------------------------------------------------------------------
// income_change — the most recent completed month's income departs materially
// from the household's recent average. Reuses GetMonthlyTrend (income is already
// SQL-computed per month). Guarded against sync gaps: both sides must have real,
// positive income before a change is flagged.
// --------------------------------------------------------------------------

const (
	incomeChangeBaselineMonths = 4
	incomeChangeFloorDollars   = 500
	incomeChangeMinBaseline    = 2 // need at least this many prior months for a stable average
)

var incomeChangeFraction = decimal.NewFromFloat(0.25)

type incomeChangeProducer struct{}

func (incomeChangeProducer) Kind() string { return "income_change" }

func (incomeChangeProducer) Detect(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time) ([]Candidate, error) {
	lastStart := monthStart(now).AddDate(0, -1, 0) // most recent completed month
	trend, err := q.GetMonthlyTrend(ctx, dbgen.GetMonthlyTrendParams{
		HouseholdID: householdID, UserID: sharedUser,
		Date: lastStart.AddDate(0, -incomeChangeBaselineMonths, 0), Date_2: monthEnd(lastStart),
	})
	if err != nil {
		return nil, err
	}

	byMonth := make(map[string]decimal.Decimal, len(trend))
	for _, m := range trend {
		byMonth[m.Month.Format("2006-01")] = m.Income
	}
	lastKey := lastStart.Format("2006-01")
	lastIncome, ok := byMonth[lastKey]
	// A completed month with no synced income is almost always a data gap, not a
	// real income of zero — don't flag it.
	if !ok || !lastIncome.IsPositive() {
		return nil, nil
	}

	sum := decimal.Zero
	n := 0
	for k, v := range byMonth {
		if k == lastKey || !v.IsPositive() {
			continue
		}
		sum = sum.Add(v)
		n++
	}
	if n < incomeChangeMinBaseline {
		return nil, nil
	}
	baseline := sum.Div(decimal.NewFromInt(int64(n)))
	if baseline.LessThan(decimal.NewFromInt(incomeChangeFloorDollars)) {
		return nil, nil
	}

	delta := lastIncome.Sub(baseline)
	if delta.Abs().Div(baseline).LessThan(incomeChangeFraction) {
		return nil, nil
	}

	up := delta.IsPositive()
	var title, body string
	monthLabel := lastStart.Format("January")
	if up {
		title = "Income up last month"
		body = fmt.Sprintf(
			"%s income came in at %s — about %s above your recent average of %s.",
			monthLabel, money(lastIncome.Round(2)), money(delta.Round(2)), money(baseline.Round(2)))
	} else {
		title = "Income down last month"
		body = fmt.Sprintf(
			"%s income came in at %s — about %s below your recent average of %s.",
			monthLabel, money(lastIncome.Round(2)), money(delta.Neg().Round(2)), money(baseline.Round(2)))
	}

	period := lastStart
	return []Candidate{{
		Kind:     "income_change",
		Priority: 4,
		Title:    title,
		Body:     body,
		Data: map[string]any{
			"month":     lastKey,
			"income":    lastIncome.StringFixed(2),
			"baseline":  baseline.Round(2).StringFixed(2),
			"delta":     delta.Round(2).StringFixed(2),
			"direction": map[bool]string{true: "up", false: "down"}[up],
		},
		Period:    &period,
		DedupeKey: "income_change:" + lastKey,
	}}, nil
}

// --------------------------------------------------------------------------
// savings_milestone — the completed month's savings rate crossed a round band it
// was below the month before. A celebratory, in-app-only nudge (priority stays
// below the push threshold on purpose). Reuses GetMonthlyTrend.
// --------------------------------------------------------------------------

// savingsBands are the round savings-rate percentages worth celebrating a
// crossing of, high to low so the highest newly-cleared band is chosen.
var savingsBands = []int{50, 40, 30, 20, 10}

type savingsMilestoneProducer struct{}

func (savingsMilestoneProducer) Kind() string { return "savings_milestone" }

func (savingsMilestoneProducer) Detect(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time) ([]Candidate, error) {
	lastStart := monthStart(now).AddDate(0, -1, 0)
	prevStart := lastStart.AddDate(0, -1, 0)

	trend, err := q.GetMonthlyTrend(ctx, dbgen.GetMonthlyTrendParams{
		HouseholdID: householdID, UserID: sharedUser,
		Date: prevStart, Date_2: monthEnd(lastStart),
	})
	if err != nil {
		return nil, err
	}
	byMonth := make(map[string]dbgen.GetMonthlyTrendRow, len(trend))
	for _, m := range trend {
		byMonth[m.Month.Format("2006-01")] = m
	}

	last, lok := byMonth[lastStart.Format("2006-01")]
	prev, pok := byMonth[prevStart.Format("2006-01")]
	if !lok || !pok || !last.Income.IsPositive() || !prev.Income.IsPositive() {
		return nil, nil
	}

	lastRate := savingsRatePct(last.Income, last.Spending)
	prevRate := savingsRatePct(prev.Income, prev.Spending)

	crossed := -1
	for _, band := range savingsBands {
		b := decimal.NewFromInt(int64(band))
		if lastRate.GreaterThanOrEqual(b) && prevRate.LessThan(b) {
			crossed = band
			break
		}
	}
	if crossed < 0 {
		return nil, nil
	}

	lastKey := lastStart.Format("2006-01")
	period := lastStart
	return []Candidate{{
		Kind:     "savings_milestone",
		Priority: 2,
		Title:    fmt.Sprintf("You crossed %d%% saved", crossed),
		Body: fmt.Sprintf(
			"Nice work — you saved %s%% of your income in %s, clearing %d%% for the first time in a while.",
			lastRate.Round(0).String(), lastStart.Format("January"), crossed),
		Data: map[string]any{
			"month":        lastKey,
			"savings_rate": lastRate.Round(1).StringFixed(1),
			"band":         crossed,
			"income":       last.Income.StringFixed(2),
			"spending":     last.Spending.StringFixed(2),
		},
		Period:    &period,
		DedupeKey: "savings_milestone:" + lastKey + ":" + strconv.Itoa(crossed),
	}}, nil
}

// savingsRatePct is (income − spending) / income × 100, guarded against a
// zero-income divide. Callers gate on positive income, but keep the guard local.
func savingsRatePct(income, spending decimal.Decimal) decimal.Decimal {
	if !income.IsPositive() {
		return decimal.Zero
	}
	return income.Sub(spending).Div(income).Mul(decimal.NewFromInt(100))
}
