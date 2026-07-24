// Package reporting holds request-free assembly of report figures shared by the
// HTTP handlers and the background jobs. It keeps the "hand the model finished
// strings" guarantee in one place: all arithmetic stays in SQL / decimal, and
// the AI layer only ever sees display-ready values.
package reporting

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/apex42group/ledgermancy/backend/internal/ai"
	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
)

const (
	// summaryTopCategories is how many category totals the narrative is handed.
	summaryTopCategories = 5
	// summaryAboveBaseline is how many "running hot vs. usual" categories to
	// surface — the signals that actually explain a month, kept short so the
	// prompt stays focused.
	summaryAboveBaseline = 4
	// summaryTopMerchants / summaryBiggestTxns bound the concrete specifics the
	// model gets to name.
	summaryTopMerchants = 5
	summaryBiggestTxns  = 5
	// baselineTrailingMonths is the trailing full-month window the per-category
	// baseline averages over. Mirrors the spending-spike insight's window so the
	// recap and the feed agree on what "usual" means.
	baselineTrailingMonths = 6
	// baselineFloor keeps small categories quiet: a few dollars over a tiny
	// baseline is noise, not a story.
	baselineFloor = 25
	// recurringLookbackMonths is how far back GetRecurringMerchants scans for a
	// cadence; recurringActiveDays then gates out anything that has gone quiet, so
	// a paid-off subscription stops counting toward the recurring total.
	recurringLookbackMonths = 12
	recurringActiveDays     = 45
)

// formatUSD renders a decimal as a display-ready dollar figure with a leading
// "$" and thousands separators — "$20,256.87", "-$1,240.00". The AI layer quotes
// these verbatim, so the model never sees a bare "20256.87" to mangle.
func formatUSD(d decimal.Decimal) string {
	neg := d.IsNegative()
	s := d.Abs().StringFixed(2)

	dot := strings.IndexByte(s, '.')
	intPart, frac := s[:dot], s[dot:]

	var grouped strings.Builder
	n := len(intPart)
	for i := 0; i < n; i++ {
		if i > 0 && (n-i)%3 == 0 {
			grouped.WriteByte(',')
		}
		grouped.WriteByte(intPart[i])
	}

	out := "$" + grouped.String() + frac
	if neg {
		out = "-" + out
	}
	return out
}

// BuildMonthlySummaryInput assembles one month's figures for the AI recap. It is
// request-free so both the on-demand handler and the scheduled digest job call
// exactly the same code — the figures can never drift between the two surfaces.
// userID scopes visibility as the caller's other reports do (their own view, or
// uuid.Nil for household-shared). asOf is "now": it decides whether the month is
// still in progress (present tense) or complete (past tense).
func BuildMonthlySummaryInput(
	ctx context.Context,
	q *dbgen.Queries,
	householdID, userID uuid.UUID,
	from, to time.Time,
	label string,
	asOf time.Time,
) (ai.MonthlySummaryInput, error) {
	summary, err := q.GetSpendingSummary(ctx, dbgen.GetSpendingSummaryParams{
		HouseholdID: householdID, UserID: userID, Date: from, Date_2: to,
	})
	if err != nil {
		return ai.MonthlySummaryInput{}, err
	}

	cats, err := q.GetSpendingByCategory(ctx, dbgen.GetSpendingByCategoryParams{
		HouseholdID: householdID, UserID: userID, Date: from, Date_2: to,
	})
	if err != nil {
		return ai.MonthlySummaryInput{}, err
	}

	// Previous month's spend, for a one-line comparison. A failure here is not
	// fatal — the summary just omits the comparison.
	prevFrom := from.AddDate(0, -1, 0)
	prevTo := from.AddDate(0, 0, -1)
	var priorSpending string
	if prev, err := q.GetSpendingSummary(ctx, dbgen.GetSpendingSummaryParams{
		HouseholdID: householdID, UserID: userID, Date: prevFrom, Date_2: prevTo,
	}); err == nil {
		priorSpending = formatUSD(prev.Spending)
	}

	top := make([]ai.CategoryLine, 0, summaryTopCategories)
	for i, c := range cats {
		if i >= summaryTopCategories {
			break
		}
		top = append(top, ai.CategoryLine{Name: c.CategoryName, Total: formatUSD(c.Total)})
	}

	// Above-baseline: which categories are running hottest versus their own
	// trailing average. Reuses the same query and window as the spending-spike
	// insight so the two surfaces never disagree on "usual".
	aboveBaseline := buildAboveBaseline(ctx, q, householdID, userID, from, cats)

	// Concrete specifics the model can name instead of only totals.
	var topMerchants []ai.MerchantLine
	if merchants, err := q.GetTopMerchants(ctx, dbgen.GetTopMerchantsParams{
		HouseholdID: householdID, UserID: userID, Date: from, Date_2: to, Limit: summaryTopMerchants,
	}); err == nil {
		for _, m := range merchants {
			topMerchants = append(topMerchants, ai.MerchantLine{
				Name: m.Merchant, Total: formatUSD(m.Total), Count: m.TransactionCount,
			})
		}
	}

	var biggest []ai.TxnLine
	if txns, err := q.GetLargestTransactions(ctx, dbgen.GetLargestTransactionsParams{
		HouseholdID: householdID, UserID: userID, Date: from, Date_2: to, Limit: summaryBiggestTxns,
	}); err == nil {
		for _, t := range txns {
			biggest = append(biggest, ai.TxnLine{
				Merchant: t.Merchant,
				Amount:   formatUSD(t.Amount),
				Date:     t.Date.Format("Jan 2"),
				Category: t.CategoryName,
			})
		}
	}

	// Estimated monthly cost of the household's still-active recurring charges.
	recurringTotal := buildRecurringTotal(ctx, q, householdID, userID, asOf)

	leftover := summary.Income.Sub(summary.Spending)

	// Savings rate: leftover as a share of income. Only meaningful when money
	// actually came in, so it is omitted for a zero-income window.
	var savingsRate string
	if summary.Income.IsPositive() {
		pct := leftover.Div(summary.Income).Mul(decimal.NewFromInt(100)).Round(0)
		savingsRate = pct.String() + "%"
	}

	// A month is in progress when "now" falls inside its window; a completed past
	// month gets the past tense and no as-of caveat.
	inProgress := !asOf.Before(from) && !asOf.After(to)
	asOfLabel := ""
	if inProgress {
		asOfLabel = asOf.Format("January 2, 2006")
	}

	return ai.MonthlySummaryInput{
		Month:                 label,
		InProgress:            inProgress,
		AsOf:                  asOfLabel,
		Income:                formatUSD(summary.Income),
		Spending:              formatUSD(summary.Spending),
		Leftover:              formatUSD(leftover),
		FixedSpending:         formatUSD(summary.FixedSpending),
		DiscretionarySpending: formatUSD(summary.DiscretionarySpending),
		SavingsRate:           savingsRate,
		RecurringTotal:        recurringTotal,
		TransactionCount:      int(summary.TransactionCount),
		TopCategories:         top,
		AboveBaseline:         aboveBaseline,
		TopMerchants:          topMerchants,
		BiggestTransactions:   biggest,
		PriorSpending:         priorSpending,
	}, nil
}

// buildAboveBaseline compares this month's per-category spend against each
// category's trailing average and returns the biggest overages first. A failure
// or a thin history just yields no deltas — the recap degrades to totals only.
func buildAboveBaseline(
	ctx context.Context,
	q *dbgen.Queries,
	householdID, userID uuid.UUID,
	from time.Time,
	thisMonth []dbgen.GetSpendingByCategoryRow,
) []ai.CategoryDelta {
	// Average over prior FULL months only, so the current partial month never
	// dilutes its own baseline.
	averages, err := q.GetCategoryAverages(ctx, dbgen.GetCategoryAveragesParams{
		HouseholdID: householdID, UserID: userID,
		Date: from.AddDate(0, -baselineTrailingMonths, 0), Date_2: from.AddDate(0, 0, -1),
	})
	if err != nil {
		return nil
	}
	avgBySlug := make(map[string]decimal.Decimal, len(averages))
	for _, a := range averages {
		avgBySlug[a.CategorySlug] = a.MonthlyAverage
	}

	floor := decimal.NewFromInt(baselineFloor)
	// Gather with the raw overage kept for sorting, then map to the display struct.
	type scored struct {
		delta ai.CategoryDelta
		over  decimal.Decimal
	}
	var scoredDeltas []scored
	for _, c := range thisMonth {
		avg, ok := avgBySlug[c.CategorySlug]
		if !ok || !avg.IsPositive() {
			continue
		}
		over := c.Total.Sub(avg)
		if over.LessThan(floor) {
			continue
		}
		scoredDeltas = append(scoredDeltas, scored{
			delta: ai.CategoryDelta{
				Name:      c.CategoryName,
				ThisMonth: formatUSD(c.Total),
				Typical:   formatUSD(avg),
				Over:      formatUSD(over),
			},
			over: over,
		})
	}

	sort.SliceStable(scoredDeltas, func(i, j int) bool {
		return scoredDeltas[i].over.GreaterThan(scoredDeltas[j].over)
	})
	if len(scoredDeltas) > summaryAboveBaseline {
		scoredDeltas = scoredDeltas[:summaryAboveBaseline]
	}

	deltas := make([]ai.CategoryDelta, 0, len(scoredDeltas))
	for _, s := range scoredDeltas {
		deltas = append(deltas, s.delta)
	}
	return deltas
}

// buildRecurringTotal sums the monthly-normalised cost of the household's
// recurring charges that are still active (seen within recurringActiveDays), so
// a cancelled or paid-off subscription no longer inflates the figure. Returns ""
// when nothing qualifies.
func buildRecurringTotal(
	ctx context.Context,
	q *dbgen.Queries,
	householdID, userID uuid.UUID,
	asOf time.Time,
) string {
	rows, err := q.GetRecurringMerchants(ctx, dbgen.GetRecurringMerchantsParams{
		HouseholdID: householdID, UserID: userID,
		Date: asOf.AddDate(0, -recurringLookbackMonths, 0),
	})
	if err != nil {
		return ""
	}
	cutoff := asOf.AddDate(0, 0, -recurringActiveDays)
	total := decimal.Zero
	for _, r := range rows {
		if r.LastSeen.Before(cutoff) {
			continue
		}
		// avg_gap_days is guaranteed 6–40 by the query, so this normalisation
		// turns a per-charge average into an estimated monthly cost without any
		// divide-by-zero risk.
		if !r.AvgGapDays.IsPositive() {
			continue
		}
		monthly := r.AverageAmount.Mul(decimal.NewFromInt(30)).Div(r.AvgGapDays)
		total = total.Add(monthly)
	}
	if !total.IsPositive() {
		return ""
	}
	return formatUSD(total.Round(2))
}
