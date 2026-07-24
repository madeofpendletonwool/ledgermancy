package insights

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
)

// --------------------------------------------------------------------------
// budget_trend — a monthly budget the household has blown for several completed
// months running. Where budget_pace warns mid-month that the CURRENT month is
// on track to go over, this looks BACKWARD at closed months: a budget that is
// consistently exceeded is a budget set too low (or a habit worth naming), and
// either way it is worth a nudge to adjust. Deterministic: it reuses
// GetSpendingByCategory per completed month and compares to the budget amount.
// --------------------------------------------------------------------------

const (
	// budgetTrendMonths is how many completed months must ALL be over budget.
	budgetTrendMonths = 3
	// budgetTrendMargin keeps a rounding-level overshoot from counting: spend must
	// clear the budget by this factor to be "over".
	budgetTrendMargin = 1.05
)

type budgetTrendProducer struct{}

func (budgetTrendProducer) Kind() string { return "budget_trend" }

func (budgetTrendProducer) Detect(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time) ([]Candidate, error) {
	mStart := monthStart(now)

	// Current household monthly budgets (amount per category). Only the amount and
	// category identity are used here; the window passed is irrelevant to those.
	budgets, err := q.GetBudgetProgress(ctx, dbgen.GetBudgetProgressParams{
		HouseholdID: householdID, UserID: sharedUser,
		WindowStart: mStart, WindowEnd: monthEnd(mStart), Ref: now,
	})
	if err != nil {
		return nil, err
	}

	// Spend per category for each of the last budgetTrendMonths COMPLETED months,
	// indexed slug -> month-index -> total.
	spentByMonth := make([]map[string]decimal.Decimal, budgetTrendMonths)
	for i := 0; i < budgetTrendMonths; i++ {
		ms := mStart.AddDate(0, -(i + 1), 0) // i=0 → last month, i=1 → two months ago…
		rows, err := q.GetSpendingByCategory(ctx, dbgen.GetSpendingByCategoryParams{
			HouseholdID: householdID, UserID: sharedUser, Date: ms, Date_2: monthEnd(ms),
		})
		if err != nil {
			return nil, err
		}
		m := make(map[string]decimal.Decimal, len(rows))
		for _, r := range rows {
			m[r.CategorySlug] = r.Total
		}
		spentByMonth[i] = m
	}

	margin := decimal.NewFromFloat(budgetTrendMargin)
	var out []Candidate
	for _, b := range budgets {
		if b.Period != "monthly" || !b.Budgeted.IsPositive() {
			continue
		}
		threshold := b.Budgeted.Mul(margin)

		// Every one of the last N months must be over, and we sum the overage to
		// report a typical figure.
		allOver := true
		totalOver := decimal.Zero
		for i := 0; i < budgetTrendMonths; i++ {
			spent := spentByMonth[i][b.CategorySlug] // zero value if absent
			if spent.LessThan(threshold) {
				allOver = false
				break
			}
			totalOver = totalOver.Add(spent.Sub(b.Budgeted))
		}
		if !allOver {
			continue
		}
		avgOver := totalOver.Div(decimal.NewFromInt(budgetTrendMonths)).Round(2)

		lastMonth := mStart.AddDate(0, -1, 0)
		period := lastMonth
		out = append(out, Candidate{
			Kind:     "budget_trend",
			Priority: 3,
			Title:    fmt.Sprintf("%s is over budget every month", b.CategoryName),
			Body: fmt.Sprintf(
				"You've gone over your %s budget of %s each of the last %d months — about %s over on average. It might be worth raising the budget or trimming the spending.",
				b.CategoryName, money(b.Budgeted), budgetTrendMonths, money(avgOver)),
			Data: map[string]any{
				"category":      b.CategoryName,
				"category_slug": b.CategorySlug,
				"budgeted":      b.Budgeted.StringFixed(2),
				"average_over":  avgOver.StringFixed(2),
				"months":        budgetTrendMonths,
			},
			Period:    &period,
			DedupeKey: fmt.Sprintf("budget_trend:%s:%s", b.CategorySlug, lastMonth.Format("2006-01")),
		})
	}
	return out, nil
}
