package likelihood

import (
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// THE GUARDRAIL RULE. This text is reproduced VERBATIM in the chat system
// prompt (see chatGuardrailRule in chat_tools_likelihood.go), and the
// implementation below is a line-by-line transcription of it. If you change one,
// change all three, because the rule's entire claim is that it is deterministic
// and quotable.
//
//	Given a set of allocation plans the user is comparing, all simulated at the
//	same run count n:
//
//	FILTER, in this order. Each step names why a plan was dropped.
//	  F1. Drop any plan whose 5th-percentile peak-to-trough drawdown exceeds
//	      household.risk_drawdown_floor. Failing the floor is disqualifying, not
//	      a penalty -- a plan the household cannot sit through is not a plan.
//	      If risk_drawdown_floor is NULL, F1 is skipped and the answer says so.
//	  F2. Of the survivors, keep those meeting every stated goal, where "meets"
//	      is funded % >= 100 at horizon AT THE MEDIAN SIMULATED OUTCOME (P50),
//	      not at doc 32's compound-at-mu figure and not at P10.
//	  F3. If F2 leaves nothing, fall back to the F1 survivors and mark the
//	      result "no plan meets every goal" -- which the AI must state.
//
//	SORT the surviving set, first key first:
//	  S1. Success rate, P(terminal >= target), DESCENDING.
//	  S2. Sigma of the terminal distribution, ASCENDING (less spread wins).
//	  S3. Plan name, ASCENDING -- so the order is TOTAL and two plans that tie
//	      on every computed figure still produce one stable pick.
//
//	The top pick is the first plan after sorting. If the filtered set is empty
//	after F1, there is NO top pick: every plan breaches the drawdown floor, and
//	that -- not a least-bad choice -- is the answer.
//
// Three details in that text are load-bearing and were ambiguous in doc 33's
// first draft, which stated four AND-joined clauses:
//
//   - It is a FILTER AND THEN A SORT, not a conjunction. Failing the drawdown
//     floor is disqualifying; a low success rate is only a demotion.
//   - Goal-fit is judged at P50, and the draft did not say which figure. At the
//     compound-at-μ figure a plan can "meet every goal" that the median
//     simulated outcome misses by 15%, which is exactly the gap doc 33 exists
//     to make visible.
//   - The sort is TOTAL. Without S3 two plans identical on every computed
//     figure would tie, and Go's sort is not stable unless asked, so the "pick"
//     would depend on input order — for a rule whose whole claim is determinism.

// MaxComparisonPlans caps the comparison set.
//
// Four, matching the UI. Over the cap the comparison is REFUSED rather than
// degraded: silently ranking the first four of six would answer a question
// nobody asked, and the cost of a fifth plan is a fifth full simulation inside
// one synchronous request.
const MaxComparisonPlans = 4

// Candidate is one plan entering the guardrail, with its simulation already run.
type Candidate struct {
	PlanID uuid.UUID
	Name   string
	Sim    Result
	// GoalFit is how this plan's stated goals fare at the median simulated
	// outcome. See EvaluateGoals — the caller computes it, because goals live in
	// doc 32's result and this package does not read the database.
	GoalFit GoalFit
}

// GoalFit is F2's input: whether a plan meets EVERY stated goal at P50.
type GoalFit struct {
	// MeetsAll is false when at least one stated goal is under 100% funded at
	// the median simulated outcome.
	MeetsAll bool
	// Stated is how many goals were judged. Zero means the household has stated
	// none, in which case MeetsAll is true — F2 cannot exclude a plan for
	// missing a goal that does not exist.
	Stated int
	// Missed names the goals that fell short, so the answer can say WHICH.
	Missed []string
}

// RankedPlan is one plan's place in the answer, carrying the reason.
type RankedPlan struct {
	PlanID uuid.UUID `json:"plan_id"`
	Name   string    `json:"name"`

	SuccessRate *decimal.Decimal `json:"success_rate"`
	P50         decimal.Decimal  `json:"p50"`
	Sigma       decimal.Decimal  `json:"sigma"`
	DrawdownP5  decimal.Decimal  `json:"drawdown_p5"`

	// MeetsAllGoals and MissedGoals are F2's finding.
	MeetsAllGoals bool     `json:"meets_all_goals"`
	MissedGoals   []string `json:"missed_goals"`

	// Excluded is true when a filter dropped this plan; ExcludedBy names the
	// clause ("F1") and Reason says it in words. A plan that vanished from a
	// comparison without a reason is worse than one that was never offered.
	Excluded   bool   `json:"excluded"`
	ExcludedBy string `json:"excluded_by,omitempty"`
	Reason     string `json:"reason,omitempty"`

	// TopPick is set on exactly one plan, or on none at all.
	TopPick bool `json:"top_pick"`
}

// Ranking is the guardrail's whole answer.
type Ranking struct {
	// Runs is the pinned run count every plan was simulated at.
	Runs int `json:"runs"`
	// Plans is every candidate, in the rule's sort order, excluded ones last.
	Plans []RankedPlan `json:"plans"`

	// TopPick is the winning plan's id, or nil when there is none. NO PICK IS A
	// REAL ANSWER: when every plan breaches the drawdown floor, the answer is
	// that fact and not a least-bad choice.
	TopPick *uuid.UUID `json:"top_pick"`
	// NoPickReason is set exactly when TopPick is nil.
	NoPickReason string `json:"no_pick_reason,omitempty"`

	// FloorApplied is false when the household has no risk_drawdown_floor on
	// file, in which case F1 was skipped — and the answer says so rather than
	// quietly ranking as though the floor had been met.
	FloorApplied bool             `json:"floor_applied"`
	FloorPct     *decimal.Decimal `json:"floor_pct,omitempty"`

	// NoPlanMeetsEveryGoal records that F3 fired: F2 emptied the set, so the
	// ranking below is over the F1 survivors and the AI must state this.
	NoPlanMeetsEveryGoal bool `json:"no_plan_meets_every_goal"`

	// Rule is the guardrail text, verbatim, so a client can show the user the
	// rule that produced the pick rather than asking them to trust it.
	Rule string `json:"rule"`
	// Explanation is the deterministic sentence naming which clause placed the
	// top pick. With AI disabled this IS the answer; with AI enabled the model
	// may rephrase it but may not contradict it.
	Explanation string `json:"explanation"`
}

// RuleText is the guardrail rule as the user and the model both see it.
const RuleText = `FILTER, in this order:
  F1. Drop any plan whose 5th-percentile peak-to-trough drawdown exceeds your
      stated drawdown floor. Failing the floor is disqualifying, not a penalty —
      a plan you cannot sit through is not a plan. With no floor on file, F1 is
      skipped and the answer says so.
  F2. Of the survivors, keep those meeting every stated goal, where "meets" is
      100% funded at horizon AT THE MEDIAN SIMULATED OUTCOME — not at the
      projected-at-your-assumed-return figure, and not at P10.
  F3. If F2 leaves nothing, fall back to the F1 survivors and say that no plan
      meets every goal.

SORT the survivors, first key first:
  S1. Success rate, descending.
  S2. Spread (sigma) of the terminal distribution, ascending.
  S3. Plan name, ascending — so two plans tied on every computed figure still
      produce one stable pick.

The top pick is the first plan after sorting. If every plan breaches the
drawdown floor there is NO top pick, and that is the answer.`

// ErrRunCountMismatch is returned when the candidates were not all simulated at
// the same run count.
//
// REFUSED RATHER THAN RENDERED. The P5 drawdown and the success rate both move
// with n, so ranking a 1,000-run plan against a 10,000-run one compares the
// dice as much as the plans. The doc promises the pick is deterministic; the
// only way to keep that promise is to decline the question.
var ErrRunCountMismatch = errors.New(
	"these plans were simulated at different run counts, so their drawdown and success figures are not comparable — " +
		"re-run the comparison with one run count")

// Rank applies the guardrail rule to a set of simulated plans.
//
// It is a pure function: same candidates in, same ranking out, twice and
// forever. That is the property the whole feature rests on, because the AI is
// permitted to explain this pick and forbidden to make one.
func Rank(candidates []Candidate, floor decimal.NullDecimal) (Ranking, error) {
	if len(candidates) == 0 {
		return Ranking{}, errors.New("no plans to compare")
	}
	if len(candidates) > MaxComparisonPlans {
		return Ranking{}, fmt.Errorf(
			"comparing more than %d plans at once is refused — a fifth plan is a fifth full simulation, "+
				"and the comparison view shows four", MaxComparisonPlans)
	}

	runs := candidates[0].Sim.Runs
	for _, c := range candidates {
		if c.Sim.Runs != runs {
			return Ranking{}, ErrRunCountMismatch
		}
	}

	out := Ranking{
		Runs:         runs,
		Plans:        make([]RankedPlan, 0, len(candidates)),
		FloorApplied: floor.Valid,
		Rule:         RuleText,
	}
	if floor.Valid {
		f := floor.Decimal.Round(2)
		out.FloorPct = &f
	}

	rows := make([]RankedPlan, len(candidates))
	for i, c := range candidates {
		rows[i] = RankedPlan{
			PlanID:        c.PlanID,
			Name:          c.Name,
			SuccessRate:   c.Sim.SuccessRate,
			P50:           c.Sim.P50,
			Sigma:         c.Sim.Sigma,
			DrawdownP5:    c.Sim.DrawdownP5,
			MeetsAllGoals: c.GoalFit.MeetsAll,
			MissedGoals:   c.GoalFit.Missed,
		}
		if rows[i].MissedGoals == nil {
			rows[i].MissedGoals = []string{}
		}
	}

	// ---- F1: the drawdown floor -------------------------------------------
	//
	// The floor is a PERCENT on households (20.00 means 20%); the simulated
	// drawdown is a FRACTION (0.19). One conversion, here, at the comparison.
	survivors := make([]int, 0, len(rows))
	for i := range rows {
		if floor.Valid {
			drawdownPct := rows[i].DrawdownP5.Mul(decimal.NewFromInt(100))
			if drawdownPct.GreaterThan(floor.Decimal) {
				rows[i].Excluded, rows[i].ExcludedBy = true, "F1"
				rows[i].Reason = fmt.Sprintf(
					"its 5th-percentile drawdown of %s%% is deeper than your %s%% floor",
					drawdownPct.Round(2).String(), floor.Decimal.Round(2).String())
				continue
			}
		}
		survivors = append(survivors, i)
	}

	if len(survivors) == 0 {
		// Every plan breaches the floor. THIS IS THE ANSWER, not a prompt to
		// promote the least-bad one.
		out.Plans = sortRanked(rows, nil)
		out.NoPickReason = fmt.Sprintf(
			"Every plan here has a 5th-percentile drawdown deeper than your %s%% floor. There is no pick: "+
				"a plan you could not sit through is not a plan, and the honest answer is that none of these "+
				"clears the bar you set.", floor.Decimal.Round(2).String())
		out.Explanation = out.NoPickReason
		return out, nil
	}

	// ---- F2: goal fit at the median simulated outcome ----------------------
	meeting := make([]int, 0, len(survivors))
	for _, i := range survivors {
		if rows[i].MeetsAllGoals {
			meeting = append(meeting, i)
		}
	}

	// ---- F3: fall back, and SAY SO -----------------------------------------
	eligible := meeting
	if len(meeting) == 0 {
		eligible = survivors
		out.NoPlanMeetsEveryGoal = true
	} else {
		for _, i := range survivors {
			if !rows[i].MeetsAllGoals {
				rows[i].Excluded, rows[i].ExcludedBy = true, "F2"
				rows[i].Reason = missedGoalsReason(rows[i].MissedGoals)
			}
		}
	}

	// ---- S1 / S2 / S3 -------------------------------------------------------
	sort.SliceStable(eligible, func(a, b int) bool {
		return lessByRule(rows[eligible[a]], rows[eligible[b]])
	})

	top := eligible[0]
	rows[top].TopPick = true
	id := rows[top].PlanID
	out.TopPick = &id
	out.Explanation = explain(rows[top], out)
	out.Plans = sortRanked(rows, eligible)
	return out, nil
}

// lessByRule is S1 → S2 → S3, and nothing else. A nil success rate (no target
// was set) sorts BELOW any stated rate: a plan measured against no target has
// not shown it meets one.
func lessByRule(a, b RankedPlan) bool {
	ra, rb := rateOrNeg(a.SuccessRate), rateOrNeg(b.SuccessRate)
	if !ra.Equal(rb) {
		return ra.GreaterThan(rb) // S1: descending
	}
	if !a.Sigma.Equal(b.Sigma) {
		return a.Sigma.LessThan(b.Sigma) // S2: ascending, less spread wins
	}
	return a.Name < b.Name // S3: total order
}

// rateOrNeg maps "no success rate" to −1 so it never ties with a real 0.0000.
// A plan with no target and a plan that met its target in zero runs are
// different answers.
func rateOrNeg(r *decimal.Decimal) decimal.Decimal {
	if r == nil {
		return decimal.NewFromInt(-1)
	}
	return *r
}

// sortRanked lays the answer out for the client: the eligible set in rule
// order first, then everything the filters dropped, each carrying its reason.
func sortRanked(rows []RankedPlan, eligible []int) []RankedPlan {
	out := make([]RankedPlan, 0, len(rows))
	seen := make(map[int]bool, len(rows))
	for _, i := range eligible {
		out = append(out, rows[i])
		seen[i] = true
	}
	rest := make([]RankedPlan, 0, len(rows))
	for i := range rows {
		if !seen[i] {
			rest = append(rest, rows[i])
		}
	}
	// Dropped plans in a stable, explicable order too — by name, because there
	// is no computed figure that ranks them and input order is not an answer.
	sort.SliceStable(rest, func(a, b int) bool { return rest[a].Name < rest[b].Name })
	return append(out, rest...)
}

func missedGoalsReason(missed []string) string {
	if len(missed) == 0 {
		return "it does not meet every stated goal at the median simulated outcome"
	}
	if len(missed) == 1 {
		return fmt.Sprintf(
			"%q is under 100%% funded at the median simulated outcome", missed[0])
	}
	return fmt.Sprintf(
		"%d goals are under 100%% funded at the median simulated outcome: %v", len(missed), missed)
}

// explain writes the deterministic sentence naming WHICH CLAUSE placed the top
// pick.
//
// This is what the model is required to cite and forbidden to replace. With AI
// disabled it is the whole answer, which is the point: the feature works with no
// key, because the rule did the work and the model was only ever phrasing it.
func explain(top RankedPlan, r Ranking) string {
	s := fmt.Sprintf("Top pick: %s.", top.Name)
	if r.NoPlanMeetsEveryGoal {
		s += " No plan here meets every stated goal at the median simulated outcome, so this is the best of" +
			" those that clear your drawdown floor."
	} else if top.MeetsAllGoals {
		s += " It meets every stated goal at the median simulated outcome."
	}
	if top.SuccessRate != nil {
		pct := top.SuccessRate.Mul(decimal.NewFromInt(100)).Round(0)
		s += fmt.Sprintf(" It reaches the target in %s%% of %d simulated futures, the highest of the set.",
			pct.String(), r.Runs)
	}
	if r.FloorApplied && r.FloorPct != nil {
		s += fmt.Sprintf(" Its 5th-percentile drawdown of %s%% is within your %s%% floor.",
			top.DrawdownP5.Mul(decimal.NewFromInt(100)).Round(2).String(), r.FloorPct.String())
	} else {
		s += " You have no drawdown floor on file, so no plan was excluded for risk — set one under" +
			" Assumptions to have that check applied."
	}
	return s
}

// EvaluateGoals judges F2 for one plan: is every stated goal 100% funded at the
// median simulated outcome?
//
// A goal is measured against ITS OWN LINKED ACCOUNT's median, not the
// portfolio's. Scaling a 529 by a portfolio-wide median would credit it with a
// good year in the 401(k) beside it, and the whole reason doc 32 maps goals to
// accounts is that money in the wrong bucket does not fund the goal.
//
// Two exclusions, both deliberate:
//
//   - A goal with NO LINKED ACCOUNT is not judged. Doc 32 already reports that
//     the plan cannot fund it; failing the plan for a goal the allocator cannot
//     reach would be blaming the plan for a missing link.
//   - An ACHIEVED goal is not judged. It is already funded, and re-testing it
//     against a horizon projection would fail plans for goals that are done.
func EvaluateGoals(goals []GoalTarget, sim Result) GoalFit {
	fit := GoalFit{MeetsAll: true, Missed: []string{}}
	for _, g := range goals {
		if !g.Linked || g.Achieved || !g.Target.IsPositive() {
			continue
		}
		fit.Stated++
		outcome, ok := sim.ByAccount[g.AccountID]
		if !ok {
			// The linked account is not a simulated bucket — a cash emergency
			// fund, say. It has no distribution, so this goal cannot be judged
			// at P50 and is not counted against the plan.
			fit.Stated--
			continue
		}
		if outcome.P50.LessThan(g.Target) {
			fit.MeetsAll = false
			fit.Missed = append(fit.Missed, g.Name)
		}
	}
	return fit
}

// GoalTarget is one stated goal, reduced to what F2 needs.
type GoalTarget struct {
	Name string
	// AccountID is the linked account's id as a string, matching
	// networth.AccountPlan.ID and Result.ByAccount's key.
	AccountID string
	Linked    bool
	Achieved  bool
	Target    decimal.Decimal
}
