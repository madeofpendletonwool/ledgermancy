package networth

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
)

// ForecastMilestones are the 0-indexed projection months the app narrates and
// tabulates — one, five, and ten years out. Shared so the projection endpoint,
// its narration, and the feed producer all point at exactly the same horizons.
var ForecastMilestones = []int{11, 59, 119}

// DefaultMonthlySurplus is the trailing-twelve-month average leftover (income
// minus spending), the figure the projection defaults its monthly surplus to.
// Factored out so the endpoint and any narrator compute it identically rather
// than drifting. userID scopes visibility exactly as the caller's other
// reports do (a member's own view, or uuid.Nil for household-shared).
func DefaultMonthlySurplus(ctx context.Context, q *dbgen.Queries, householdID, userID uuid.UUID, now time.Time) (decimal.Decimal, error) {
	from := now.AddDate(-1, 0, 0)
	s, err := q.GetSpendingSummary(ctx, dbgen.GetSpendingSummaryParams{
		HouseholdID: householdID,
		UserID:      userID,
		Date:        from,
		Date_2:      now,
	})
	if err != nil {
		return decimal.Zero, err
	}
	return s.Income.Sub(s.Spending).Div(decimal.NewFromInt(12)).Round(2), nil
}
