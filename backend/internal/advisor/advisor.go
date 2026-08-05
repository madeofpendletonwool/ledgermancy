// Package advisor turns "this household has slack this month" into a ranked
// list of computed tradeoffs for that slack.
//
// THE SPLIT, which is the whole point of the package: slack, every option,
// every projected outcome and the ORDER are computed here in exact decimal. The
// model's only job is to read the finished list aloud. It never invents an
// option, never reorders, and never computes — the same contract the insight
// spine and the chat handlers already run on.
//
// The distinction the copy has to hold: this presents COMPUTED TRADEOFFS, it
// does not recommend. "Paying the 22% card returns a guaranteed 22%; investing
// at an assumed 7% real return does not" is arithmetic. "You should pay off your
// card" is advice, and this app does not give it.
//
// The honest version of that claim is narrower than it sounds, because a ranked
// list IS closer to a recommendation than "we don't recommend" admits. The
// position this package actually holds: THE ORDERING IS A PUBLISHED RULE, NOT A
// JUDGEMENT. The rule is in rank.go, it is the same for every household, every
// figure that places an option in it is computed, and it is surfaced beside the
// options. What the app does not do is override that rule for a particular user,
// or say "you should" about anything below tier 2.
//
// No new tables. Every option is computed on demand from state that already
// exists; per-option suppression rides the preferences store.
package advisor

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// Option kinds. These are HALF of an option's stable key — the other half is the
// subject id — and they are persisted inside suppression preferences, so
// renaming one silently un-suppresses whatever a household had already told the
// app to stop suggesting. Treat them as a wire format.
const (
	KindEmergencyFund = "emergency_fund"
	KindEmployerMatch = "employer_match"
	KindDebtPaydown   = "debt_paydown"
	KindTaxAdvantaged = "tax_advantaged"
	KindGoalAccel     = "goal_acceleration"
	KindTaxableInvest = "taxable_investing"
)

// Value kinds name what an option's Value MEANS, so the UI never has to infer it
// from the tier. A dollar figure that is "interest you would not pay" and one
// that is "match you would collect" are not interchangeable, and rendering both
// as a bare number is how a panel starts implying they are.
const (
	ValueInterestAvoided = "interest_avoided"
	ValueMatchCaptured   = "match_captured"
	ValueMonthsEarlier   = "months_earlier"
	ValueGapClosed       = "gap_closed"
	ValueHeadroom        = "headroom"
	ValueProjectedGrowth = "projected_growth"
)

// BasisLine is one row of the "how this was calculated" panel: the label of an
// input and the finished figure that went in.
//
// This is what separates the feature from a horoscope. Every option carries the
// arithmetic that placed it, in the same spirit as doc 15's visible projection
// assumptions — including, always, the hurdle and which side of it the option
// fell on.
type BasisLine struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// Option is one computed use of one marginal dollar.
type Option struct {
	// Key is the stable (kind, subject) identity a household suppresses. Stable
	// across runs and across TIERS: a card that drops below the hurdle when the
	// household's assumed return rises keeps its key, so "stop suggesting this
	// card" keeps meaning that.
	Key       string `json:"key"`
	Kind      string `json:"kind"`
	SubjectID string `json:"subject_id,omitempty"`

	// Tier is the waterfall step that placed this option (1–7). Zero means
	// unranked — see Unranked.
	Tier  int    `json:"tier"`
	Label string `json:"label"`
	// Detail is the deterministic sentence: every number in it is already
	// computed. The model may reword this; it may not change a figure.
	Detail string `json:"detail"`

	// Amount is what this option proposes putting in, per month.
	Amount decimal.Decimal `json:"amount"`
	// Value is what that buys, and ValueKind says in what unit. For
	// ValueMonthsEarlier the figure is a count of months, not money.
	Value     decimal.Decimal `json:"value"`
	ValueKind string          `json:"value_kind"`

	// Unranked marks an option that could not be placed because an input it
	// needs is missing — in practice a debt with no APR anywhere. It is listed,
	// it is labelled, and it sorts AFTER every ranked option. It is never
	// defaulted to zero and never silently dropped: a debt sorted to the bottom
	// because its rate is unknown is exactly the debt most likely to be the
	// expensive one.
	Unranked bool   `json:"unranked"`
	Note     string `json:"note,omitempty"`

	// Tradeoff marks tier 7. Those options are presented SIDE BY SIDE rather
	// than ranked against each other: the app has no opinion on 3.5%
	// guaranteed versus 7% assumed, and pretending to would be exactly the
	// recommendation this package says it does not make.
	Tradeoff bool `json:"tradeoff"`

	// Unbounded marks a debt that never clears at its current payment. Interest
	// avoided is then not a difference of two finite totals, so Value stays zero
	// and this flag carries the meaning. Within its tier an unbounded debt sorts
	// first — "this never gets paid off" outranks any finite saving beside it.
	Unbounded bool `json:"unbounded"`

	Basis []BasisLine `json:"basis"`
}

// Slack basis labels: which of BuildSafeToSpend's two figures the advisor used.
const (
	SlackBasisAfterBills   = "after_bills"
	SlackBasisTypicalMonth = "typical_month"
)

// Advice is one advisor run: the slack, what it was derived from, and the ranked
// options for it.
type Advice struct {
	// Slack is reporting.BuildSafeToSpend's figure — CALLED, never
	// reimplemented. See slack.go for why that matters more than it looks.
	Slack      decimal.Decimal `json:"slack"`
	SlackBasis string          `json:"slack_basis"`
	// ObligationCoverage is echoed straight from SafeToSpend so a reader can
	// see why the bill-aware figure was or was not the one used.
	ObligationCoverage int `json:"obligation_coverage"`
	// IncomeMonths caveats a thin history, exactly as the Budgets page does.
	IncomeMonths int `json:"income_months"`

	// Threshold is the bar Slack had to clear for options to be computed at all.
	Threshold decimal.Decimal `json:"threshold"`
	// Significant is false when there is nothing to say: slack at or below the
	// threshold, or zero or negative. Options is then empty and no insight is
	// raised — never a "you have -$300 to allocate" suggestion.
	Significant bool `json:"significant"`

	// Hurdle is the APR, AS A PERCENTAGE, above which a guaranteed return beats
	// the household's own assumed one. Steps 3 and 7 of the waterfall sit on
	// opposite sides of it. Surfaced because it is the honest part of the rule:
	// derived from the household's stated assumption rather than a constant.
	Hurdle      decimal.Decimal `json:"hurdle"`
	HurdleBasis string          `json:"hurdle_basis"`

	Options []Option `json:"options"`

	// Suppressed is the option keys this household has muted, echoed so the
	// settings UI can offer them back without a second round trip.
	Suppressed []string `json:"suppressed"`

	// SlackParts is the breakdown behind Slack, for the "how this was
	// calculated" panel. Straight from SafeToSpend; nothing is re-derived.
	SlackParts SlackParts `json:"slack_parts"`
}

// SlackParts is BuildSafeToSpend's component breakdown, carried through
// unchanged so the advisor's arithmetic panel and the Budgets page cannot
// print different components for the same figure.
type SlackParts struct {
	ExpectedIncome        decimal.Decimal `json:"expected_income"`
	FixedCosts            decimal.Decimal `json:"fixed_costs"`
	FixedCostsAfterBills  decimal.Decimal `json:"fixed_costs_after_bills"`
	BudgetedDiscretionary decimal.Decimal `json:"budgeted_discretionary"`
	GoalContributions     decimal.Decimal `json:"goal_contributions"`
	UpcomingObligations   decimal.Decimal `json:"upcoming_obligations"`
}

// Build computes one household's advice as of `now`.
//
// The shape of this function is the doc's rule in order: resolve slack, stop if
// it is not worth saying anything about, gather the deterministic inputs, build
// every option, then rank. Nothing here phrases anything — narration is the
// caller's step, and the caller must work without it.
func Build(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time) (Advice, error) {
	slack, err := resolveSlack(ctx, q, householdID, now)
	if err != nil {
		return Advice{}, err
	}

	adv := Advice{
		Slack:              slack.amount,
		SlackBasis:         slack.basis,
		ObligationCoverage: slack.coverage,
		IncomeMonths:       slack.incomeMonths,
		SlackParts:         slack.parts,
		Threshold:          thresholdFor(ctx, q, householdID),
		Options:            []Option{},
		Suppressed:         []string{},
	}

	suppressed := suppressedKeys(ctx, q, householdID)
	adv.Suppressed = suppressed.list()

	// Zero or negative slack produces no options and no insight. So does slack
	// below the household's threshold: an advisor that fires on $12 trains the
	// user to ignore it.
	if !slack.amount.IsPositive() || slack.amount.LessThan(adv.Threshold) {
		return adv, nil
	}
	adv.Significant = true

	in, err := gather(ctx, q, householdID, now, slack)
	if err != nil {
		return Advice{}, err
	}
	adv.Hurdle, adv.HurdleBasis = in.hurdle, in.hurdleBasis

	adv.Options = rank(in, suppressed)
	return adv, nil
}
