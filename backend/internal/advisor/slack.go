package advisor

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/reporting"
)

// SLACK IS reporting.BuildSafeToSpend, CALLED AND NOT REIMPLEMENTED.
//
// This is the single most load-bearing line in the package. Two surfaces
// disagreeing about how much slack a household has is worse than neither
// existing: the Budgets page prints this number, the advisor allocates it, and a
// user who notices they differ will trust neither page again.
//
// Three properties of BuildSafeToSpend the advisor inherits and must not quietly
// restate anywhere:
//
//   - It is a MEDIAN OF HISTORY, not "income so far plus projected income". A
//     bonus month does not raise slack and a thin month does not sink it. The
//     figure describes a TYPICAL month, which is why the advisor's copy is
//     conditional rather than a claim about this month's bank balance.
//   - ONE-TIME-FLAGGED ROWS ARE EXCLUDED, so a transaction the household marked
//     non-repeating never reaches the baseline.
//   - It is SCOPED TO SHARED VISIBILITY. A member's private accounts are
//     invisible to it, so advisor slack is a household-shared figure and the UI
//     must not imply it covers a private account.
//
// AND SLACK IS NOT "SPARE MONEY". The Budgets page presents this identical
// number as "after everything that's already spoken for, how much is left to
// spend freely this month?" The advisor presents it as the input to "here is the
// highest-value thing to do with it" — same figure, opposite instruction.
//
// The resolution is FRAMING, not a second number. Advisor copy says "if you
// don't spend this, here is what it would do", never "you have $400 available".
// The tradeoff is conditional on not spending it, which is true, is computable,
// and does not contradict the Budgets page. Anything written against this
// package must keep that conditional.
type slackResult struct {
	amount       decimal.Decimal
	basis        string
	coverage     int
	incomeMonths int
	parts        SlackParts
	// fixedCosts is the trailing typical fixed-cost figure, held separately
	// because the emergency-fund tiers measure their target in months of it.
	// The bill-aware variant is deliberately NOT used for that: an emergency
	// fund covers a typical month of outgoings, not this particular month's
	// dated bills.
	fixedCosts decimal.Decimal
	// typicalSpending is the trailing typical FULL month (median of total
	// monthly spending, same window as fixedCosts), and spendingMonths is how
	// many months it rests on. The emergency fund's full-spending bar is
	// measured in months of it: some households want the target to cover
	// everything a normal month costs, not only the bills.
	typicalSpending decimal.Decimal
	spendingMonths  int
}

// resolveSlack picks between SafeToSpend's two figures and records which.
//
// The rule: AmountAfterBills when ObligationCoverage > 0, Amount otherwise. With
// no known obligations the bill-aware figure is the ordinary one by
// construction, so reporting it as "bill-aware" would imply information the app
// does not have.
func resolveSlack(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time) (slackResult, error) {
	sts, err := reporting.BuildSafeToSpend(ctx, q, householdID, now)
	if err != nil {
		return slackResult{}, err
	}

	out := slackResult{
		coverage:        sts.ObligationCoverage,
		incomeMonths:    sts.IncomeMonths,
		fixedCosts:      sts.FixedCosts,
		typicalSpending: sts.TypicalMonthlySpending,
		spendingMonths:  sts.SpendingMonths,
		parts: SlackParts{
			ExpectedIncome:        sts.ExpectedIncome,
			FixedCosts:            sts.FixedCosts,
			FixedCostsAfterBills:  sts.FixedCostsAfterBills,
			BudgetedDiscretionary: sts.BudgetedDiscretionary,
			GoalContributions:     sts.GoalContributions,
			UpcomingObligations:   sts.UpcomingObligations,
		},
	}

	if sts.ObligationCoverage > 0 {
		out.amount, out.basis = sts.AmountAfterBills, SlackBasisAfterBills
	} else {
		out.amount, out.basis = sts.Amount, SlackBasisTypicalMonth
	}
	return out, nil
}

// Preference keys. Household-scoped, JSONB, no migration — the same store doc 02
// gave the anomaly detectors their sensitivity through.
const (
	thresholdKey  = "advisor.slack_threshold"
	efMonthsKey   = "advisor.emergency_fund_months"
	suppressedKey = "advisor.suppressed_options"
)

// defaultThreshold is the slack an advisor run must clear before it says
// anything. An advisor that fires on $12 trains the user to ignore it.
var defaultThreshold = decimal.NewFromInt(100)

// defaultEmergencyMonths is the full emergency fund target, in months of
// trailing fixed costs. Three is the conservative end of the usual 3–6 range:
// this figure only decides when tier 4 STOPS being offered, so aiming low means
// the app stops nagging sooner rather than holding a household at tier 4 while a
// 22% card runs.
const defaultEmergencyMonths = 3

// thresholdFor reads the household's threshold, degrading to the default on any
// error. Same reasoning as insights.sensitivityFor: a run must never fail
// because a preference row is missing or malformed.
func thresholdFor(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID) decimal.Decimal {
	raw, ok := readPref(ctx, q, householdID, thresholdKey)
	if !ok {
		return defaultThreshold
	}
	// Stored as a JSON number or a decimal string; accept both rather than
	// failing a run over which one a client wrote.
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil && f >= 0 {
		return decimal.NewFromFloat(f)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if d, err := decimal.NewFromString(s); err == nil && !d.IsNegative() {
			return d
		}
	}
	return defaultThreshold
}

// emergencyMonthsFor reads the household's full-EF target in months.
func emergencyMonthsFor(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID) int {
	raw, ok := readPref(ctx, q, householdID, efMonthsKey)
	if !ok {
		return defaultEmergencyMonths
	}
	var n int
	if err := json.Unmarshal(raw, &n); err == nil && n > 0 && n <= 24 {
		return n
	}
	return defaultEmergencyMonths
}

// readPref fetches one household preference, reporting "not set" for a missing
// row and logging anything else. Errors never fail a run.
func readPref(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, key string) ([]byte, bool) {
	hh := householdID
	raw, err := q.GetHouseholdPreference(ctx, dbgen.GetHouseholdPreferenceParams{
		HouseholdID: &hh, Key: key,
	})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Error("read advisor preference", "error", err, "key", key, "household_id", householdID)
		}
		return nil, false
	}
	return raw, true
}
