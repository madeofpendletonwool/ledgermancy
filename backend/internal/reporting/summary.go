// Package reporting holds request-free assembly of report figures shared by the
// HTTP handlers and the background jobs. It keeps the "hand the model finished
// strings" guarantee in one place: all arithmetic stays in SQL / decimal, and
// the AI layer only ever sees StringFixed(2) values.
package reporting

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/apex42group/ledgermancy/backend/internal/ai"
	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
)

// summaryTopCategories is how many categories the narrative is given to work
// with — enough to name the biggest one or two without drowning the prompt.
const summaryTopCategories = 5

// BuildMonthlySummaryInput assembles one month's figures for the AI recap. It is
// request-free so both the on-demand handler and the scheduled digest job call
// exactly the same code — the figures can never drift between the two surfaces.
// userID scopes visibility as the caller's other reports do (their own view, or
// uuid.Nil for household-shared).
func BuildMonthlySummaryInput(
	ctx context.Context,
	q *dbgen.Queries,
	householdID, userID uuid.UUID,
	from, to time.Time,
	label string,
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
		priorSpending = prev.Spending.StringFixed(2)
	}

	top := make([]ai.CategoryLine, 0, summaryTopCategories)
	for i, c := range cats {
		if i >= summaryTopCategories {
			break
		}
		top = append(top, ai.CategoryLine{Name: c.CategoryName, Total: c.Total.StringFixed(2)})
	}

	leftover := summary.Income.Sub(summary.Spending)
	return ai.MonthlySummaryInput{
		Month:                 label,
		Income:                summary.Income.StringFixed(2),
		Spending:              summary.Spending.StringFixed(2),
		Leftover:              leftover.StringFixed(2),
		FixedSpending:         summary.FixedSpending.StringFixed(2),
		DiscretionarySpending: summary.DiscretionarySpending.StringFixed(2),
		TopCategories:         top,
		PriorSpending:         priorSpending,
	}, nil
}
