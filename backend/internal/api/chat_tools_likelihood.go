package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/ai"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/allocation"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/likelihood"
)

// The likelihood layer's chat tools (doc 33).
//
// Same rule as every other tool file here: THESE WRAP THE ENGINES AND COMPUTE
// NOTHING NEW. plan_likelihood runs the same simulation POST
// /api/likelihood/plan/{id} runs, and compare_plans runs the same guardrail —
// so the figure the model quotes and the figure on the page are the same figure.
//
// AND THE MODEL DOES NOT PICK. The guardrail rule picks; the model explains
// WHICH CLAUSE placed the pick, citing the computed figures. That distinction is
// the entire reason this feature is allowed to name a "top pick" at all, and
// chatGuardrailPrompt below is where it is enforced.

// chatLikelihoodToolDefs are doc 33's three tools. They join the MODELLING set:
// a likelihood question needs the allocator's plans as input and the simulation
// on top.
func chatLikelihoodToolDefs() []ai.Tool {
	return []ai.Tool{
		{
			Name: "plan_likelihood",
			Description: "Run the Monte Carlo for ONE saved allocation plan. Returns the projected-at-your-assumed-" +
				"return figure, the simulated P10 / median / P90, the success rate against the plan's target, the " +
				"spread (sigma), and the 5th-percentile peak-to-trough drawdown. " +
				"Call allocation_plans first to get plan ids. " +
				"Quote the success rate as \"meets the target in N% of the simulated futures\" — NEVER as a chance " +
				"or a probability.",
			InputSchema: json.RawMessage(`{
"type":"object",
"properties":{
  "plan_id":{"type":"string","description":"Saved plan uuid from allocation_plans"},
  "volatility":{"type":"string","description":"Optional annual volatility of the real return as a fraction, e.g. \"0.15\""}
},
"required":["plan_id"]}`),
		},
		{
			Name: "compare_plans",
			Description: "Compare 2-4 saved allocation plans and apply the documented guardrail rule, which names a " +
				"top pick from computed figures. Every plan is simulated at ONE pinned run count. " +
				"Returns each plan's goal fit, success rate, sigma and 5th-percentile drawdown, plus the rule's pick " +
				"and the clause that produced it — or NO PICK with the reason when every plan breaches the " +
				"household's drawdown floor. " +
				"You must not reorder, override, or substitute your judgement for this result.",
			InputSchema: json.RawMessage(`{
"type":"object",
"properties":{
  "plan_ids":{"type":"array","description":"2-4 saved plan uuids","items":{"type":"string"}},
  "volatility":{"type":"string","description":"Optional annual volatility of the real return as a fraction"}
},
"required":["plan_ids"]}`),
		},
		{
			Name: "plan_tracker",
			Description: "Plan-vs-actual drift for one saved plan: what the plan said would go into each bucket, what " +
				"actually did, whether the household is on track or behind, and what the current monthly gap " +
				"compounds to by the horizon. Buckets with no contribution trail are reported as UNTRACKED, never " +
				"as zero — say so rather than treating them as missed contributions.",
			InputSchema: json.RawMessage(`{
"type":"object",
"properties":{
  "plan_id":{"type":"string","description":"Saved plan uuid from allocation_plans"}
},
"required":["plan_id"]}`),
		},
		{
			Name: "allocation_plans",
			Description: "Every saved allocation plan, with its id, name, lump, monthly and horizon. This is where " +
				"the plan ids for plan_likelihood, compare_plans and plan_tracker come from.",
			InputSchema: emptySchema,
		},
	}
}

// chatGuardrailPrompt is appended to the system prompt. THE RULE APPEARS
// VERBATIM, because the model is required to cite the clause that produced a
// pick and cannot cite a rule it was not given.
//
// The three prohibitions after it each close a way the model can undo the work
// this doc does to make the numbers honest. They are not stylistic:
//
//   - Restating a success rate as a probability is the shorter phrasing, and the
//     model will reach for it unless told not to. "94% of 1,000 simulated
//     futures" is a claim about a model; "94% chance" is a claim about the
//     world, and the app has no basis for the second.
//   - Presenting the pick as advice turns a computed filter into a
//     recommendation the app is not qualified to make.
//   - Resolving "no pick" by quietly naming the least-bad plan is the single
//     most damaging thing the model could do here, because it converts a real
//     answer — every plan breaches your floor — into a false reassurance.
const chatGuardrailPrompt = `

Likelihood and plan comparison (plan_likelihood, compare_plans, plan_tracker):
- These wrap computed engines. Quote their figures verbatim; never recompute, average, or interpolate them.
- NEVER restate a success rate as a probability. Say "meets the target in 94% of 1,000 simulated futures", never "94% chance" and never "94% likely". These are modelled sequences drawn around the user's own assumed return and volatility, not a forecast and not market history.
- Two figures on a plan are DIFFERENT STATISTICS and must be labelled differently: "projected at your assumed return" (compounding at the mean) and "the median simulated outcome". The second is normally LOWER, because volatility drags compounding. If you show both, say that in one line. Never call either of them "P50" and never present them as a discrepancy or an error.
- A drawdown figure is the FIFTH-PERCENTILE peak-to-trough drawdown at a stated run count. Name the percentile when you quote it.
- The top pick in compare_plans is chosen by this documented rule, not by you:

` + likelihood.RuleText + `

- NEVER name a top pick without citing the computed figures that place it there, and NEVER recommend a plan that fails the drawdown floor.
- Explain WHICH CLAUSE placed the pick; do not present it as advice. "Plan B is the pick — it meets both goals at the median outcome and has the highest success rate of the three within your drawdown floor" is in bounds. "You should choose Plan B" is not.
- When there is NO PICK because every plan breaches the drawdown floor, SAY SO. That is a real answer. Do not resolve it by quietly naming the least-bad plan.
- Doc 24's ranked options and this guardrail are two different rules answering two different questions. Do not blend them. Where they disagree on the page, the explanation is the disagreement, not a synthesis.`

// executeLikelihoodTool runs one of doc 33's tools.
//
// Returns ok=false for a name it does not own, so the caller falls through to
// the next executor — one dispatch point per file rather than one switch nobody
// can read.
func (s *Server) executeLikelihoodTool(
	ctx context.Context, identity auth.Identity, name string, input json.RawMessage,
) (out string, ok bool, err error) {
	switch name {
	case "plan_likelihood", "compare_plans", "plan_tracker", "allocation_plans":
	default:
		return "", false, nil
	}

	if name == "allocation_plans" {
		rows, err := s.Queries.ListAllocationPlans(ctx, identity.HouseholdID)
		if err != nil {
			return "", true, err
		}
		items := make([]map[string]any, 0, len(rows))
		for _, row := range rows {
			var inputs allocation.StoredInputs
			// A blob that will not unmarshal is reported per-plan rather than
			// silently zeroed. Swallowing the error here would list a plan with
			// empty lump, empty monthly, and a zero horizon — a fabricated record
			// indistinguishable from a real one, which the model would narrate as
			// the user's saved decision. The row is still the user's plan, so it
			// stays listed with its id and name plus an error marker rather than
			// dropped, matching toPlanResponse's convention in allocation_handlers.
			if err := json.Unmarshal(row.Inputs, &inputs); err != nil {
				items = append(items, map[string]any{
					"plan_id":    row.ID.String(),
					"name":       row.Name,
					"error":      "this plan's saved inputs could not be read",
					"created_at": row.CreatedAt.UTC().Format(time.DateOnly),
				})
				continue
			}
			items = append(items, map[string]any{
				"plan_id":       row.ID.String(),
				"name":          row.Name,
				"lump":          inputs.Lump,
				"monthly":       inputs.Monthly,
				"horizon_years": inputs.HorizonYears,
				"target":        inputs.Target,
				"created_at":    row.CreatedAt.UTC().Format(time.DateOnly),
			})
		}
		return marshalToolOK(map[string]any{"plans": items, "count": len(items)})
	}

	now := time.Now().UTC()
	baseline, err := allocation.AssembleBaseline(ctx, s.Queries, identity.HouseholdID, identity.UserID, now)
	if err != nil {
		return "", true, err
	}

	switch name {
	case "plan_likelihood":
		var in struct {
			PlanID     string `json:"plan_id"`
			Volatility string `json:"volatility"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return marshalToolOK(map[string]any{"error": "could not read the input: " + err.Error()})
		}
		planID, err := uuid.Parse(strings.TrimSpace(in.PlanID))
		if err != nil {
			return marshalToolOK(map[string]any{
				"error": "plan_id must be a uuid — use the ids from allocation_plans"})
		}
		_, req, err := s.loadPlanRequest(ctx, identity, planID)
		if err != nil {
			return marshalToolOK(map[string]any{"error": err.Error()})
		}
		resp, err := s.likelihoodFor(baseline, req, s.interactiveRuns(""), toolVolatility(in.Volatility))
		if err != nil {
			return marshalToolOK(map[string]any{"error": err.Error()})
		}
		return marshalToolOK(resp)

	case "compare_plans":
		var in struct {
			PlanIDs    []string `json:"plan_ids"`
			Volatility string   `json:"volatility"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return marshalToolOK(map[string]any{"error": "could not read the input: " + err.Error()})
		}
		if len(in.PlanIDs) < 2 {
			return marshalToolOK(map[string]any{
				"error": "comparing plans needs at least two plan ids"})
		}
		if len(in.PlanIDs) > likelihood.MaxComparisonPlans {
			return marshalToolOK(map[string]any{"error": fmt.Sprintf(
				"more than %d plans at once is refused — each one is a full simulation",
				likelihood.MaxComparisonPlans)})
		}

		// ONE RUN COUNT ACROSS EVERY PLAN. The drawdown percentile and the
		// success rate both move with n, so a comparison assembled from
		// differing counts would rank the dice as much as the plans.
		runs := s.interactiveRuns("")
		volatility := toolVolatility(in.Volatility)

		candidates := make([]likelihood.Candidate, 0, len(in.PlanIDs))
		figures := make([]likelihoodResponse, 0, len(in.PlanIDs))
		for _, raw := range in.PlanIDs {
			planID, err := uuid.Parse(strings.TrimSpace(raw))
			if err != nil {
				return marshalToolOK(map[string]any{
					"error": "plan_ids must be uuids — use the ids from allocation_plans"})
			}
			row, req, err := s.loadPlanRequest(ctx, identity, planID)
			if err != nil {
				return marshalToolOK(map[string]any{"error": err.Error()})
			}
			result, sim, err := s.runPlan(baseline, req, runs, volatility)
			if err != nil {
				return marshalToolOK(map[string]any{"error": row.Name + ": " + err.Error()})
			}
			shaped := s.shapeLikelihood(req, result, sim, runs, volatility)
			shaped.PlanID, shaped.Name = &row.ID, row.Name
			figures = append(figures, shaped)
			candidates = append(candidates, likelihood.Candidate{
				PlanID: row.ID, Name: row.Name, Sim: sim,
				GoalFit: likelihood.EvaluateGoals(goalTargets(result), sim),
			})
		}
		ranking, err := likelihood.Rank(candidates, baseline.RiskFloor)
		if err != nil {
			return marshalToolOK(map[string]any{"error": err.Error()})
		}
		return marshalToolOK(map[string]any{
			"ranking":             ranking,
			"plans":               figures,
			"monte_carlo_enabled": s.Config.Retirement.MonteCarloEnabled,
		})

	case "plan_tracker":
		var in struct {
			PlanID string `json:"plan_id"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return marshalToolOK(map[string]any{"error": "could not read the input: " + err.Error()})
		}
		planID, err := uuid.Parse(strings.TrimSpace(in.PlanID))
		if err != nil {
			return marshalToolOK(map[string]any{
				"error": "plan_id must be a uuid — use the ids from allocation_plans"})
		}
		row, req, err := s.loadPlanRequest(ctx, identity, planID)
		if err != nil {
			return marshalToolOK(map[string]any{"error": err.Error()})
		}
		drift, err := s.reconcilePlan(ctx, identity, baseline, row, req, now)
		if err != nil {
			return "", true, err
		}
		return marshalToolOK(map[string]any{"plan": row.Name, "drift": drift})
	}

	return "", false, nil
}

// toolVolatility reads the model's optional volatility, falling back to the
// endpoint's default. A value the model could not parse falls back rather than
// failing the turn — the response names what was actually used either way.
func toolVolatility(raw string) decimal.Decimal {
	if strings.TrimSpace(raw) == "" {
		return defaultVolatility
	}
	v, err := decimal.NewFromString(strings.TrimSpace(raw))
	if err != nil || v.IsNegative() {
		return defaultVolatility
	}
	return v
}
