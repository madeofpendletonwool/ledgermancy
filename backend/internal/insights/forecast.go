package insights

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
	"github.com/apex42group/ledgermancy/backend/internal/networth"
)

// forecast — a low-priority "here's where this path leads" feed row. All figures
// come straight from networth.Project (the same deterministic engine and
// defaults the projection endpoint uses); the engine's phrasing pass rewords the
// narrative when AI is on, but never recomputes a milestone. One row per
// household, refreshed monthly.

const forecastReturnRate = "0.06" // matches the projection endpoint default

type forecastProducer struct{}

func (forecastProducer) Kind() string { return "forecast" }

func (forecastProducer) Detect(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time) ([]Candidate, error) {
	current, err := networth.Compute(ctx, q, householdID)
	if err != nil {
		return nil, err
	}

	// Household-shared surplus, mirroring how the other producers scope
	// visibility (sharedUser), and the endpoint's other defaults.
	surplus, err := networth.DefaultMonthlySurplus(ctx, q, householdID, sharedUser, now)
	if err != nil {
		return nil, err
	}
	assumptions := networth.Assumptions{
		MonthlySurplus:    surplus,
		AnnualReturnRate:  decimal.RequireFromString(forecastReturnRate),
		AnnualDebtPaydown: decimal.Zero,
		Months:            120,
	}
	points := networth.Project(current, assumptions)

	milestones := make([]map[string]any, 0, len(networth.ForecastMilestones))
	var headline networth.ProjectionPoint
	var haveHeadline bool
	for _, i := range networth.ForecastMilestones {
		if i >= len(points) {
			continue
		}
		milestones = append(milestones, map[string]any{
			"months":    i + 1,
			"month":     points[i].Month,
			"net_worth": points[i].NetWorth.StringFixed(2),
		})
		headline = points[i] // last available milestone → the 10-year headline
		haveHeadline = true
	}
	if !haveHeadline {
		return nil, nil
	}

	years := (networth.ForecastMilestones[len(networth.ForecastMilestones)-1] + 1) / 12
	basis := "Straight-line projection from your current balances and trailing-12-month averages. Not a market forecast."

	mStart := monthStart(now)
	return []Candidate{{
		Kind:     "forecast",
		Priority: 1,
		Title:    "Where your current path leads",
		Body: fmt.Sprintf(
			"Saving about %s a month, your net worth is on track for roughly %s in %d years. This is an estimate, not a forecast — it assumes things carry on as they are.",
			money(surplus), money(headline.NetWorth), years),
		Data: map[string]any{
			"milestones":         milestones,
			"monthly_surplus":    surplus.StringFixed(2),
			"annual_return_rate": forecastReturnRate,
			"basis":              basis,
		},
		Period:    &mStart,
		DedupeKey: "forecast:" + mStart.Format("2006-01"),
	}}, nil
}
