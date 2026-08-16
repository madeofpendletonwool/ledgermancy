package advisor

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/allocation"
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
	// TargetAmount is that target expressed in DOLLARS: TargetMonths ×
	// MonthlyFixed, the same product the ranker's full emergency-fund tier
	// holds households to (gather's fullTarget). The chat is forbidden from
	// doing arithmetic — every figure it states must arrive finished — so when
	// the household asks "how much liquid do we need to hold the target at all
	// times", the answer has to be this field. Before it existed the model was
	// handed the two factors and correctly refused to multiply them, and the
	// household got a refusal where it asked for a number.
	//
	// Nil when MonthlyFixed is not positive, for the same reason Months is: no
	// outgoings on record means no target to state in dollars, and a 0.00 here
	// would read as "target met".
	TargetAmount *decimal.Decimal `json:"target_amount"`
	// MonthlySpending is the typical FULL month — median total spending over
	// the same trailing window MonthlyFixed came from — and SpendingMonths is
	// how many months it rests on.
	//
	// These exist because a household can measure its emergency fund against
	// two bars, and this app's official one (TargetAmount, above) is the
	// FIXED-COST bar: it answers "could we keep the roof on if income stopped",
	// and it is deliberately the smaller number. Some households want the
	// stricter bar — "N months of everything we actually spend" — and that
	// figure was computable from nothing the chat could see: the model may not
	// average, multiply or blend, so the full-spending bar has to arrive as
	// finished as the fixed one or it does not arrive at all.
	MonthlySpending decimal.Decimal `json:"monthly_spending"`
	SpendingMonths  int             `json:"spending_months"`
	// FullTargetAmount is TargetMonths × MonthlySpending — the full-spending
	// bar in dollars. FullMonths is the runway measured against it: how many
	// months of typical TOTAL spending the liquid balance covers. Both nil
	// when there is no spending history (SpendingMonths == 0): unmeasured is
	// not $0.00, and a full-spending runway of "0 months" would read as broke.
	FullTargetAmount *decimal.Decimal `json:"full_target_amount"`
	FullMonths       *decimal.Decimal `json:"full_months_covered"`
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

	// College is the household's college goals: who they are for, how far out
	// enrollment is, and where the funding actually stands.
	//
	// It exists because the briefing is the tool a model calls FIRST for "what
	// is my position", and without it it had no idea a college goal existed.
	// Asked how to invest $2,000 a month, the advisor looked at a $98 balance in
	// an account named "Individual 6601" and hedged — "the 529 is nearly empty,
	// worth considering IF college costs are on the horizon" — for a household
	// whose one-year-old has a birthdate on file, a linked 529 and a named goal
	// seventeen years out. Every fact it needed was in the database and none of
	// it was in the briefing.
	//
	// THE FUNDING FIGURES HERE ARE NOT A SECOND COPY. They are read off the same
	// allocation.Run the college_projection tool calls — see fillCollege — so
	// the two surfaces cannot report different numbers for the same goal, any
	// more than two callers of the same function can. The first version of this
	// field withheld them precisely to avoid a second copy, and that was the
	// right instinct aimed at the wrong target: the danger was reimplementing
	// the drawdown, not reporting its answer. Withholding it meant a model that
	// knew a goal existed and still could not say whether it was on track
	// without a second round-trip, which it did not always make.
	//
	// The one thing that must never appear here is a funding figure this package
	// computed itself. If you find yourself dividing anything, stop.
	College []CollegeBrief `json:"college"`

	// Plan is the household's authored intent (MAD-258): the written strategy,
	// the per-person notes, and the ACTIVE decisions. It exists for the reason
	// College does — the briefing is the tool a model calls first, and a model
	// that does not know the household DELIBERATELY holds a three-month
	// emergency fund will lecture it about the six-month rule it considered and
	// rejected. The plan is the sentence that makes every other briefing figure
	// interpretable.
	//
	// Bodies are SEALED here and opened only in the API layer, which owns the
	// cipher — this package stays cipher-free the way it always has, and the
	// digest never becomes a second plaintext copy floating between packages.
	// Best-effort like the attention feed: a plan read that fails must not
	// blank an advisor page whose figures are already computed.
	Plan PlanDigest `json:"plan"`

	Attention []AttentionItem `json:"attention"`
}

// PlanDigest is the plan as the briefing carries it: opaque until the API layer
// opens it. Exists is the whole-plan signal — a household with no plan renders
// an absent digest, never an "empty plan" that reads as "the strategy is to
// have no strategy".
type PlanDigest struct {
	Exists     bool        `json:"exists"`
	ReviewedAt *time.Time  `json:"reviewed_at"`
	Sections   []PlanSectionDigest `json:"sections"`
	Decisions  []PlanDecisionDigest `json:"decisions"`
}

// PlanSectionDigest is one sealed section. PersonName is set on the 'person'
// kind so the opener can attribute the note without another query.
type PlanSectionDigest struct {
	Kind       string `json:"kind"`
	PersonName string `json:"person_name,omitempty"`
	Body       []byte  `json:"body"`
}

// PlanDecisionDigest is one ACTIVE decision (confirmed, not superseded — the
// history stays on the Plan page; the briefing quotes what the household
// currently holds, and GetActivePlanDecisions enforces that in SQL).
type PlanDecisionDigest struct {
	Topic     string    `json:"topic"`
	DecidedAt time.Time `json:"decided_at"`
	Body      []byte    `json:"body"`
}

// CollegeBrief is one college goal: its facts and its standing.
type CollegeBrief struct {
	Name string `json:"name"`
	// BeneficiaryAge and YearsToEnrollment are nil when no birthdate or stored
	// age resolves. A missing horizon is a state to report, not a zero to
	// compute with — the same refusal ResolveAge exists for.
	BeneficiaryAge    *int `json:"beneficiary_age"`
	YearsToEnrollment *int `json:"years_to_enrollment"`
	// YearsOfStudy is how many years the goal funds, and AnnualCostToday is ONE
	// year in today's dollars — which is what target_amount means on a college
	// goal, and is misread as the whole cost often enough to be worth naming.
	YearsOfStudy    int             `json:"years_of_study"`
	AnnualCostToday decimal.Decimal `json:"annual_cost_today"`

	// Projectable is false when there is no linked account or no resolvable
	// horizon, and Note says which. EVERY FIELD BELOW IT IS MEANINGLESS THEN —
	// an unprojectable goal is not a 0%-funded goal, and rendering it as one
	// would be a worse lie than the hedge this whole field replaced.
	Projectable bool   `json:"projectable"`
	Note        string `json:"note,omitempty"`

	// TotalCost is all YearsOfStudy inflated separately and summed, in today's
	// dollars — the number AnnualCostToday is one slice of, and the one a
	// household actually has to fund.
	TotalCost      decimal.Decimal `json:"total_cost"`
	TotalShortfall decimal.Decimal `json:"total_shortfall"`
	// FundedPct is a PERCENT. FirstShortfallYear is 1-based and zero when the
	// money never runs out.
	FundedPct          decimal.Decimal `json:"funded_pct"`
	FirstShortfallYear int             `json:"first_shortfall_year"`
	// MonthlyNeeded is the extra monthly contribution that would fund every
	// year. Nil means either "already funded" or "no amount inside the search
	// bound gets there" — FundedPct tells those apart, and neither is zero.
	MonthlyNeeded *decimal.Decimal `json:"monthly_needed,omitempty"`
	// AccountRealReturnPct is the real return every figure above was computed
	// at, as a PERCENT, and ReturnRateSource says where it came from.
	//
	// This travels with the funding figures because it is the question that
	// follows them — "and what return is that assuming?" — and because the
	// briefing's own Assumptions.RealReturn is the household default, which is
	// a DIFFERENT number whenever the linked account carries its own rate.
	// Without this field the only rate in the briefing was the household one,
	// and it got quoted as the college projection's rate while the drawdown had
	// used the account's. Reporting the figures without the rate that produced
	// them is what made that misread possible.
	AccountRealReturnPct decimal.Decimal `json:"account_real_return_pct"`
	ReturnRateSource     string          `json:"return_rate_source"`
	// Summary is the engine's own one-sentence rendering, carried verbatim so a
	// model has a sentence it can quote without doing arithmetic over the
	// fields above.
	Summary string `json:"summary"`
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

	if out.Runway, err = buildRunway(ctx, q, householdID, slack); err != nil {
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

	if err := fillCollege(ctx, q, householdID, now, &out); err != nil {
		return Briefing{}, err
	}

	// The plan digest is best-effort for the same reason the attention feed
	// is: it interprets the figures rather than computing them, and a failing
	// plan read must not blank a briefing whose position is already computed.
	// A household with no plan is not an error — Exists=false is an answer.
	fillPlan(ctx, q, householdID, &out)

	// The attention feed is the one best-effort part. It is decoration on a
	// briefing whose four headline figures are already computed, and a failing
	// insights read should not blank the page — the opposite trade from the
	// figures above, and for the opposite reason.
	out.Attention = attentionItems(ctx, q, householdID, now)

	return out, nil
}

// fillPlan loads the plan digest. Sealed bodies and all: the cipher lives in
// the API layer, so this reads opaque bytes and the callers open them — the
// same split advisor_messages has always had.
func fillPlan(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, out *Briefing) {
	sections, err := q.ListPlanSections(ctx, householdID)
	if err != nil {
		return
	}
	decisions, err := q.GetActivePlanDecisions(ctx, householdID)
	if err != nil {
		return
	}
	if len(sections) == 0 && len(decisions) == 0 {
		return
	}

	digest := PlanDigest{Exists: true}
	for _, sec := range sections {
		name := ""
		if sec.PersonName != nil {
			name = *sec.PersonName
		}
		digest.Sections = append(digest.Sections, PlanSectionDigest{
			Kind: sec.Kind, PersonName: name, Body: sec.Body,
		})
	}
	for _, d := range decisions {
		digest.Decisions = append(digest.Decisions, PlanDecisionDigest{
			Topic: d.Topic, DecidedAt: d.DecidedAt, Body: d.Body,
		})
	}
	out.Plan = digest
}

// buildRunway divides liquid savings by trailing typical fixed costs, and
// carries the same window's typical FULL month so the emergency fund can be
// stated against either bar.
func buildRunway(
	ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, slack slackResult,
) (Runway, error) {
	liquid, err := liquidSavings(ctx, q, householdID)
	if err != nil {
		return Runway{}, err
	}
	r := Runway{
		Liquid:          liquid,
		MonthlyFixed:    slack.fixedCosts.Round(2),
		TargetMonths:    emergencyMonthsFor(ctx, q, householdID),
		MonthlySpending: slack.typicalSpending.Round(2),
		SpendingMonths:  slack.spendingMonths,
	}
	if r.MonthlyFixed.IsPositive() {
		months := liquid.Div(r.MonthlyFixed).Round(1)
		r.Months = &months
		target := r.MonthlyFixed.Mul(decimal.NewFromInt(int64(r.TargetMonths))).Round(2)
		r.TargetAmount = &target
	}
	// The full-spending bar is guarded on its OWN history, not on MonthlyFixed:
	// the two figures come off the same window but describe different columns,
	// and a household with bills on record and no completed spending months
	// (or the reverse) is thin, not impossible. Zero spending months means no
	// typical month exists to measure against — every field of this bar stays
	// nil rather than quoting a $0.00 target a household would read as met.
	if r.MonthlySpending.IsPositive() && r.SpendingMonths > 0 {
		full := liquid.Div(r.MonthlySpending).Round(1)
		r.FullMonths = &full
		fullTarget := r.MonthlySpending.Mul(decimal.NewFromInt(int64(r.TargetMonths))).Round(2)
		r.FullTargetAmount = &fullTarget
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

// collegeBriefHorizon is the projection horizon the college drawdown runs at,
// and it is deliberately the SAME constant value the college_projection tool
// uses: 25 years, long enough to reach any plausible enrollment year from a
// newborn. A shorter horizon would clamp the balance read to the last projected
// point and understate it silently.
const collegeBriefHorizon = 25

// fillCollege puts the household's college goals in the briefing, standing and
// all, by calling the drawdown rather than reproducing it.
//
// It runs allocation.Run over an EMPTY request, which is exactly what the
// college_projection tool does: a college projection is a plan with no money in
// it, so the drawdown runs against what the 529 is already doing. Reading
// result.College off that run is what makes the two surfaces incapable of
// disagreeing — they are one call, not two implementations that happen to
// match today.
//
// The first version of this stopped at the facts (name, age, horizon) and left
// the funded percentage and the monthly figure to college_projection alone, to
// avoid keeping a second copy of them. The copy was the right thing to fear and
// the wrong thing to prevent by omission: a model holding a briefing that said
// "college goal, seventeen years out" and nothing else still had to make a
// second call to say anything useful, and when it skipped that call it went
// back to guessing. Reporting the engine's own answer is not a copy of it.
//
// The list read comes first and short-circuits: a household with no college
// goals — most of them — pays nothing for this, and the allocation baseline and
// its two retirement projections are never assembled. Only a household that
// actually has a goal pays for the answer to it.
func fillCollege(
	ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time, out *Briefing,
) error {
	rows, err := q.ListCollegeGoals(ctx, householdID)
	if err != nil {
		return err
	}
	out.College = []CollegeBrief{}
	if len(rows) == 0 {
		return nil
	}

	// uuid.Nil is sharedUser: the household-shared visibility scope every read
	// in this package runs under. It also means AssembleBaseline finds no
	// calling person and falls back to the household's STORED age, which is the
	// behaviour fillRetirement documents wanting — the advisor runs for a
	// household, from a job as often as from a request, so it has no member to
	// prefer.
	baseline, err := allocation.AssembleBaseline(ctx, q, householdID, sharedUser, now)
	if err != nil {
		return err
	}
	result, err := allocation.Run(baseline, allocation.Request{HorizonYears: collegeBriefHorizon})
	if err != nil {
		return err
	}

	out.College = make([]CollegeBrief, 0, len(result.College))
	for _, c := range result.College {
		brief := CollegeBrief{
			Name:            c.Name,
			BeneficiaryAge:  c.BeneficiaryAge,
			YearsOfStudy:    c.Years,
			AnnualCostToday: c.AnnualCostToday,
			Projectable:     c.Projectable,
			Note:            c.Note,
			Summary:         c.CollegeSummaryLine(),
		}
		// The horizon travels with the age: both are known or neither is, and
		// nil is what says so. A goal whose beneficiary has no age on file must
		// not report "0 years to enrollment".
		if c.BeneficiaryAge != nil {
			years := c.YearsToEnrollment
			brief.YearsToEnrollment = &years
		}
		// The funding figures only mean anything for a goal that projected.
		// Leaving them at zero for one that did not would print "0% funded" for
		// a goal whose 529 simply is not linked yet.
		if c.Projectable {
			brief.TotalCost = c.TotalCost
			brief.TotalShortfall = c.TotalShortfall
			brief.FundedPct = c.FundedPct
			brief.FirstShortfallYear = c.FirstShortfallYear
			brief.MonthlyNeeded = c.MonthlyNeeded
			// Carried under the same guard as the figures it explains: an
			// unprojectable goal ran no drawdown, so there is no rate it was
			// computed at, and a 0 here would read as "projected at 0%".
			brief.AccountRealReturnPct = c.AccountRealReturnPct
			brief.ReturnRateSource = c.ReturnRateSource
		}
		out.College = append(out.College, brief)
	}
	return nil
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
