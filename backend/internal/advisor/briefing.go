package advisor

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/goals"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/networth"
)

// The briefing is what a real advisor meeting OPENS with, and it is composed by
// deterministic code rather than by the model.
//
// Every field below is the output of an engine that already ships, called — not
// reimplemented. The model's only permitted role is to phrase the one-paragraph
// summary over figures that were finished before it saw them, which is the same
// contract rank.go and chat_handlers.go run on. With no API key configured the
// briefing renders in full and unnarrated: nothing here needs AI to be correct.

// briefingAttentionCount is how many insight-feed rows ride along. Three,
// because a briefing that opens with eight things needing attention is a list,
// not a briefing.
const briefingAttentionCount = 3

// briefingProjectionMonths is the horizon the FI scan walks when the household
// has not set a target age. Sixty years from the current age is long enough that
// "not reached" means it, and short enough to stay cheap.
const briefingProjectionMonths = 60 * 12

// AttentionItem is one insight-feed row, carried verbatim.
type AttentionItem struct {
	ID       uuid.UUID `json:"id"`
	Kind     string    `json:"kind"`
	Priority int       `json:"priority"`
	Title    string    `json:"title"`
	Body     string    `json:"body"`
}

// DebtFree is the household's debt-free date, and the honesty around it.
//
// THE DATE IS THE MAX OVER ComputePayoff, NOT THE MIN. The min is the date the
// FIRST debt dies, which is a different and much more flattering number. A
// household is debt-free when the LAST one does.
//
// Two consequences are carried in the fields rather than smoothed over. A debt
// that never pays off (a payment at or below the interest, or a payoff past the
// 100-year horizon) makes the WHOLE household's debt-free date "never" — not the
// max of the finite ones. And a debt whose APR is unresolvable cannot be
// projected at all, so Excluded counts it and the briefing says so rather than
// quietly maxing over the rest.
type DebtFree struct {
	// Date is nil when there is nothing to pay off, when some debt never clears,
	// or when every debt was excluded. Never is what distinguishes the second
	// case from the first.
	Date  *time.Time `json:"date"`
	Never bool       `json:"never"`
	// NeverAccount names the debt that made it "never", so the answer is
	// actionable rather than a verdict.
	NeverAccount string `json:"never_account,omitempty"`
	// Projected is how many debts went into the max; Excluded how many could not
	// be projected, with ExcludedNames naming them.
	Projected     int      `json:"projected"`
	Excluded      int      `json:"excluded"`
	ExcludedNames []string `json:"excluded_names"`
	// TotalBalance is what is owed across every debt, projected or not — the one
	// figure that is honest even when the date is not available.
	TotalBalance decimal.Decimal `json:"total_balance"`
}

// Runway is the emergency-fund answer: how many months of outgoings the liquid
// balance covers.
//
// The denominator is TRAILING TYPICAL FIXED COSTS, the same figure the advisor's
// emergency-fund tiers measure their target in, so the briefing strip and the
// options panel cannot print two different runways for one household. The
// numerator is shared depository balances only — see liquidSavings for why a
// brokerage balance is not an emergency fund.
type Runway struct {
	Liquid       decimal.Decimal `json:"liquid"`
	MonthlyFixed decimal.Decimal `json:"monthly_fixed"`
	// Months is nil when MonthlyFixed is zero: a runway over no outgoings is a
	// division by zero, and "infinite" is not a figure worth printing.
	Months *decimal.Decimal `json:"months"`
	// Target is the household's full emergency-fund goal in months, echoed so
	// the strip can render progress against the same bar the ranker uses.
	TargetMonths int `json:"target_months"`
}

// Briefing is the whole opening statement.
type Briefing struct {
	AsOf time.Time `json:"as_of"`

	NetWorth     decimal.Decimal `json:"net_worth"`
	Assets       decimal.Decimal `json:"assets"`
	Debts        decimal.Decimal `json:"debts"`
	MonthlySlack decimal.Decimal `json:"monthly_slack"`
	// SlackBasis is which of SafeToSpend's two figures the slack came from,
	// carried so the strip can caveat it exactly as the Budgets page does.
	SlackBasis   string `json:"slack_basis"`
	IncomeMonths int    `json:"income_months"`

	// FIAge is nil when financial independence is not reached inside the
	// horizon, which is an answer rather than a gap. AlreadyFI says the
	// household clears its target spending today.
	FIAge     *int `json:"fi_age"`
	AlreadyFI bool `json:"already_fi"`
	// RetirementProjected is false when there is nothing to project — no
	// tax-treatment-confirmed accounts — so the UI omits the tile instead of
	// printing a confident "never".
	RetirementProjected bool `json:"retirement_projected"`

	DebtFree DebtFree `json:"debt_free"`
	Runway   Runway   `json:"runway"`

	// Assumptions is the household's own stated projection input — the real
	// return every invested dollar is assumed to earn, the general inflation
	// rate it is measured against, and the APR hurdle above which a guaranteed
	// return beats that assumption. These are surfaced so a model quoting "your
	// assumed return" reads the ACTUAL figure rather than the hurdle floor (6%)
	// or a figure from memory — the two were conflated in conversation and the
	// household was told its return was 6% when it had set 3%.
	Assumptions Assumptions `json:"assumptions"`

	Attention []AttentionItem `json:"attention"`
}

// Assumptions is the household-level projection input. Rates are FRACTIONS
// (0.03 = 3%) internally and rendered as percents for the model; the hurdle is
// already a percent because every APR in the app is.
type Assumptions struct {
	// RealReturn is the household's assumed REAL annual return, as a fraction.
	RealReturn decimal.Decimal `json:"real_return"`
	// Inflation is the household's assumed general inflation, as a fraction.
	Inflation decimal.Decimal `json:"inflation"`
	// Hurdle is the APR, as a PERCENT, above which a guaranteed return beats
	// RealReturn. HurdleBasis says whether that figure IS the household's real
	// return or the 6% floor that applies when the assumed return is lower.
	Hurdle      decimal.Decimal `json:"hurdle"`
	HurdleBasis string          `json:"hurdle_basis"`
}

// BuildBriefing composes one household's briefing as of `now`.
//
// One failure sinks the run for the same reason gather's does: a briefing with a
// silently-missing debt-free date is indistinguishable from a household with no
// debts, and that is the wrong thing to show somebody at the top of the page.
// The one exception is the attention feed — see below.
func BuildBriefing(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time) (Briefing, error) {
	out := Briefing{AsOf: now}

	slack, err := resolveSlack(ctx, q, householdID, now)
	if err != nil {
		return Briefing{}, err
	}
	out.MonthlySlack = slack.amount.Round(2)
	out.SlackBasis = slack.basis
	out.IncomeMonths = slack.incomeMonths

	nw, err := q.ComputeNetWorth(ctx, householdID)
	if err != nil {
		return Briefing{}, err
	}
	out.Assets = nw.Cash.Add(nw.Investments).Add(nw.OtherAssets).Add(nw.ManualAssets).Round(2)
	out.Debts = nw.CreditDebt.Add(nw.LoanDebt).Add(nw.ManualDebt).Round(2)
	out.NetWorth = out.Assets.Sub(out.Debts)

	if out.Runway, err = buildRunway(ctx, q, householdID, slack.fixedCosts); err != nil {
		return Briefing{}, err
	}

	debts, err := loadDebts(ctx, q, householdID)
	if err != nil {
		return Briefing{}, err
	}
	out.DebtFree = debtFreeDate(debts, now)

	if err := fillRetirement(ctx, q, householdID, now, &out); err != nil {
		return Briefing{}, err
	}

	// The attention feed is the one best-effort part. It is decoration on a
	// briefing whose four headline figures are already computed, and a failing
	// insights read should not blank the page — the opposite trade from the
	// figures above, and for the opposite reason.
	out.Attention = attentionItems(ctx, q, householdID, now)

	return out, nil
}

// buildRunway divides liquid savings by trailing typical fixed costs.
func buildRunway(
	ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, monthlyFixed decimal.Decimal,
) (Runway, error) {
	liquid, err := liquidSavings(ctx, q, householdID)
	if err != nil {
		return Runway{}, err
	}
	r := Runway{
		Liquid:       liquid,
		MonthlyFixed: monthlyFixed.Round(2),
		TargetMonths: emergencyMonthsFor(ctx, q, householdID),
	}
	if r.MonthlyFixed.IsPositive() {
		months := liquid.Div(r.MonthlyFixed).Round(1)
		r.Months = &months
	}
	return r, nil
}

// debtFreeDate is the MAX over every projectable debt's payoff date.
//
// Read the field comments on DebtFree before changing anything here: the min is
// the tempting and wrong answer, "never" is contagious rather than skippable,
// and a debt with no resolvable APR is counted as excluded rather than dropped.
func debtFreeDate(debts []debt, now time.Time) DebtFree {
	out := DebtFree{ExcludedNames: []string{}, TotalBalance: decimal.Zero}

	for _, d := range debts {
		out.TotalBalance = out.TotalBalance.Add(d.balance)

		// A rate nobody knows cannot be amortized. Defaulting it to zero would
		// project the debt as interest-free, which flatters exactly the debt
		// most likely to be the expensive one. A payment nobody knows is the
		// same problem one field over: ComputePayoff would report "never" from
		// an absent figure rather than from the household's actual situation.
		if !d.hasAPR() || !d.hasPayment() {
			out.Excluded++
			out.ExcludedNames = append(out.ExcludedNames, d.name)
			continue
		}

		f := goals.ComputePayoff(d.balance, d.terms.APR, d.terms.Payment, nil, now)
		if f.NeverPaysOff || f.PayoffDate == nil {
			// Contagious on purpose. One debt that never clears means the
			// HOUSEHOLD is never debt-free, and reporting the max of the finite
			// ones would be a date the household never reaches.
			out.Never = true
			out.NeverAccount = d.name
			out.Date = nil
			out.Projected++
			continue
		}
		out.Projected++
		if out.Never {
			continue
		}
		if out.Date == nil || f.PayoffDate.After(*out.Date) {
			date := *f.PayoffDate
			out.Date = &date
		}
	}

	out.TotalBalance = out.TotalBalance.Round(2)
	return out
}

// fillRetirement runs the household's own retirement projection for the FI age.
//
// It uses the household's STORED age rather than a caller's birthdate, for the
// reason storedAge already records: the advisor runs for a household, from a job
// as often as from a request, so it has no calling member to prefer.
func fillRetirement(
	ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time, out *Briefing,
) error {
	stored, err := q.GetProjectionAssumptions(ctx, householdID)
	if errors.Is(err, pgx.ErrNoRows) {
		// A household that has never opened the projections page still gets a
		// briefing; it just gets one without an FI age, since a projection with
		// no assumptions behind it is not a figure.
		return nil
	} else if err != nil {
		return err
	}

	// Surface the household's OWN rates so a model quoting them reads the actual
	// figures. resolveHurdle is the same rule the advisor ranker uses: the
	// assumed return where it clears the 6% floor, the floor otherwise — and the
	// basis string says which, which is exactly the distinction the model
	// conflated when it called the 6% floor "your assumed return".
	out.Assumptions.RealReturn = stored.RealReturnRate
	out.Assumptions.Inflation = stored.InflationRate
	out.Assumptions.Hurdle, out.Assumptions.HurdleBasis = resolveHurdle(stored.RealReturnRate)

	rows, err := q.ListProjectableAccounts(ctx, dbgen.ListProjectableAccountsParams{
		HouseholdID: householdID, UserID: sharedUser,
	})
	if err != nil {
		return err
	}

	plans := make([]networth.AccountPlan, 0, len(rows))
	for _, r := range rows {
		p := toPlan(r, now)
		if p.Treatment == "" {
			continue // the projection excludes it anyway; see ExcludedAccounts
		}
		plans = append(plans, p)
	}
	if len(plans) == 0 {
		return nil
	}

	target, err := targetAnnualSpending(ctx, q, householdID, stored, now)
	if err != nil {
		return err
	}

	a := networth.RetirementAssumptions{
		RealReturnRate:       stored.RealReturnRate,
		WithdrawalRate:       stored.WithdrawalRate,
		CurrentAge:           storedAge(stored, now),
		TargetAnnualSpending: target,
		Months:               briefingProjectionMonths,
	}
	if stored.TargetRetirementAge != nil {
		a.TargetRetirementAge = int(*stored.TargetRetirementAge)
	}
	if stored.SsStartAge != nil {
		a.SSStartAge = int(*stored.SsStartAge)
	}
	if stored.AnnualSsIncome.Valid {
		a.AnnualSSIncome = stored.AnnualSsIncome.Decimal
	}
	// An FI age is an AGE, so without one the scan has nothing to count from.
	// Reporting "not reached" would be a claim the app cannot support.
	if a.CurrentAge <= 0 || !a.TargetAnnualSpending.IsPositive() {
		return nil
	}

	proj := networth.ProjectRetirement(plans, a, now)
	out.RetirementProjected = true
	out.FIAge = proj.FIAge
	out.AlreadyFI = proj.AlreadyFI
	return nil
}

// targetAnnualSpending is the household's stated retirement spending, defaulting
// to its actual trailing-twelve-month spend.
//
// The same default the projections endpoint applies: the app knows this figure,
// so it must not make somebody guess it.
func targetAnnualSpending(
	ctx context.Context, q *dbgen.Queries, householdID uuid.UUID,
	stored dbgen.ProjectionAssumption, now time.Time,
) (decimal.Decimal, error) {
	if stored.TargetAnnualSpending.Valid {
		return stored.TargetAnnualSpending.Decimal, nil
	}
	summary, err := q.GetSpendingSummary(ctx, dbgen.GetSpendingSummaryParams{
		HouseholdID: householdID, UserID: sharedUser,
		Date: now.AddDate(-1, 0, 0), Date_2: now,
	})
	if err != nil {
		return decimal.Zero, err
	}
	return summary.Spending.Round(2), nil
}

// attentionItems is the top few undismissed, still-current insights, in feed
// order. Best-effort: an error yields an empty list, never a failed briefing.
func attentionItems(
	ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time,
) []AttentionItem {
	rows, err := q.ListInsights(ctx, dbgen.ListInsightsParams{
		HouseholdID: householdID, IncludeDismissed: false, AsOf: now,
	})
	if err != nil {
		return []AttentionItem{}
	}
	out := make([]AttentionItem, 0, briefingAttentionCount)
	for _, r := range rows {
		if len(out) == briefingAttentionCount {
			break
		}
		out = append(out, AttentionItem{
			ID: r.ID, Kind: r.Kind, Priority: int(r.Priority),
			Title: r.Title, Body: r.Body,
		})
	}
	return out
}
