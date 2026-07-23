package insights

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
	"github.com/apex42group/ledgermancy/backend/internal/goals"
	"github.com/apex42group/ledgermancy/backend/internal/networth"
)

// goal — coaching for a household savings goal that is behind schedule or newly
// achieved. Feasibility is deterministic (goals.Compute over the same projection
// surplus the forecast uses); the model, when on, only rewords the nudge. Money
// in Data is finished StringFixed(2) — the model never divides target by months.
//
// Only HOUSEHOLD-scoped goals are coached: the insights feed is household-shared
// with no per-user visibility, so surfacing a personal goal here would leak it to
// every member. Personal goals still get their full standing on the Goals page;
// they just don't push coaching into the shared feed. This mirrors the sharedUser
// scoping the other producers use to keep private data out of the feed.

type goalProducer struct{}

func (goalProducer) Kind() string { return "goal" }

func (goalProducer) Detect(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time) ([]Candidate, error) {
	rows, err := q.ListActiveHouseholdGoals(ctx, householdID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}

	// Household-shared trailing surplus, the same figure the projection defaults
	// to — computed once and reused across every goal.
	surplus, err := networth.DefaultMonthlySurplus(ctx, q, householdID, sharedUser, now)
	if err != nil {
		return nil, err
	}

	mStart := monthStart(now)
	var out []Candidate
	for _, g := range rows {
		current, err := goalProgress(ctx, q, g, now)
		if err != nil {
			return nil, err
		}
		f := goals.Compute(g.TargetAmount, current, surplus, g.TargetDate, now)

		// Newly achieved: a one-time congratulations, and stamp achieved_at so it
		// isn't re-raised as "behind" and the celebration fires once.
		if f.Achieved {
			if g.AchievedAt == nil {
				if err := q.MarkGoalAchieved(ctx, dbgen.MarkGoalAchievedParams{
					ID: g.ID, HouseholdID: householdID,
				}); err != nil {
					return nil, err
				}
			}
			out = append(out, Candidate{
				Kind:     "goal",
				Priority: 3,
				Title:    fmt.Sprintf("Goal reached: %s", g.Name),
				Body: fmt.Sprintf(
					"You've reached your %s goal of %s. Nicely done.",
					g.Name, money(g.TargetAmount.Round(2))),
				Data: map[string]any{
					"goal_id": g.ID.String(),
					"name":    g.Name,
					"target":  g.TargetAmount.StringFixed(2),
					"current": current.Round(2).StringFixed(2),
					"status":  "achieved",
				},
				Period:    &mStart,
				DedupeKey: fmt.Sprintf("goal:%s:%s", g.ID, mStart.Format("2006-01")),
			})
			continue
		}

		// Only coach when behind (a positive shortfall). On-track and open-ended
		// goals stay quiet — the Goals page shows their standing.
		if f.OpenEnded || f.OnTrack || !f.Shortfall.IsPositive() {
			continue
		}

		out = append(out, Candidate{
			Kind:     "goal",
			Priority: goalPriority(f.Shortfall, f.RequiredMonthly),
			Title:    fmt.Sprintf("%s goal is behind", g.Name),
			Body: fmt.Sprintf(
				"To hit your %s goal of %s in %d months you'd need about %s a month, but you're saving around %s — roughly %s a month short.",
				g.Name, money(g.TargetAmount.Round(2)), f.MonthsLeft,
				money(f.RequiredMonthly), money(surplus), money(f.Shortfall)),
			Data: map[string]any{
				"goal_id":          g.ID.String(),
				"name":             g.Name,
				"target":           g.TargetAmount.StringFixed(2),
				"current":          current.Round(2).StringFixed(2),
				"required_monthly": f.RequiredMonthly.StringFixed(2),
				"monthly_surplus":  surplus.StringFixed(2),
				"shortfall":        f.Shortfall.StringFixed(2),
				"months_left":      f.MonthsLeft,
				"status":           "behind",
			},
			Period:    &mStart,
			DedupeKey: fmt.Sprintf("goal:%s:%s", g.ID, mStart.Format("2006-01")),
		})
	}
	return out, nil
}

// goalProgress derives a goal's current progress the same way the API does: the
// linked account's balance, or accumulated household surplus since created_at.
func goalProgress(ctx context.Context, q *dbgen.Queries, g dbgen.Goal, now time.Time) (decimal.Decimal, error) {
	if g.AccountID != nil {
		return q.GetGoalAccountBalance(ctx, dbgen.GetGoalAccountBalanceParams{
			ID: *g.AccountID, HouseholdID: g.HouseholdID,
		})
	}
	sum, err := q.GetSpendingSummary(ctx, dbgen.GetSpendingSummaryParams{
		HouseholdID: g.HouseholdID, UserID: sharedUser, Date: g.CreatedAt, Date_2: now,
	})
	if err != nil {
		return decimal.Zero, err
	}
	surplus := sum.Income.Sub(sum.Spending)
	if surplus.IsNegative() {
		surplus = decimal.Zero
	}
	return surplus, nil
}

// goalPriority scales with how far behind a goal is: a shortfall that is a large
// share of the required contribution is more urgent.
func goalPriority(shortfall, required decimal.Decimal) int {
	if !required.IsPositive() {
		return 3
	}
	ratio := shortfall.Div(required)
	switch {
	case ratio.GreaterThanOrEqual(decimal.NewFromFloat(0.75)):
		return 5
	case ratio.GreaterThanOrEqual(decimal.NewFromFloat(0.4)):
		return 4
	default:
		return 3
	}
}
