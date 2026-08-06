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
)

// The allocator's chat tools (doc 32).
//
// Same rule as every other tool file here: THESE WRAP THE ENGINE AND COMPUTE
// NOTHING NEW. allocation_plan calls allocation.Run, which is the same function
// POST /api/allocation/plan calls, so the figure the model quotes and the figure
// on the page are the same figure. Money is returned as StringFixed(2) —
// finished, so the model quotes it verbatim and never rounds.
//
// AND THE MODEL NEVER PROPOSES A SPLIT. It may phrase a finished result and it
// may ask the user what split they want, but the percentages come from the user
// or from nowhere. Auto-optimising the allocation is out of scope for the whole
// doc, and a tool that let the model pick would put it back in by the side door.

// chatAllocationToolDefs are doc 32's four tools. They join the MODELLING set —
// a "what should I do with $X" question needs the position inputs a planning
// question needs plus the allocator on top.
func chatAllocationToolDefs() []ai.Tool {
	return []ai.Tool{
		{
			Name: "allocation_plan",
			Description: "Split a lump sum and/or a monthly surplus across accounts and project each one. " +
				"Takes the split the USER chose — you must never invent percentages; ask for them. " +
				"Returns per-bucket projected value, IRS cap headroom and spill, Roth/HSA eligibility per bucket, " +
				"debt payoff and interest avoided, goal mapping, and college funding. " +
				"Call allocation_buckets first to get the account ids.",
			InputSchema: json.RawMessage(`{
"type":"object",
"properties":{
  "lump":{"type":"string","description":"One-off amount to allocate, e.g. \"30000.00\""},
  "monthly":{"type":"string","description":"Monthly surplus to allocate, e.g. \"1800.00\""},
  "horizon_years":{"type":"integer","description":"Years to project, 1-60"},
  "target_nest_egg":{"type":"string","description":"Optional target the plan is measured against"},
  "splits":{"type":"array","description":"One entry per bucket; percentages 0-100","items":{"type":"object","properties":{
    "account_id":{"type":"string","description":"Account uuid from allocation_buckets"},
    "lump_pct":{"type":"string","description":"Percent of the lump, e.g. \"40\""},
    "monthly_pct":{"type":"string","description":"Percent of the monthly surplus"},
    "real_return_rate":{"type":"string","description":"Optional per-bucket real return as a fraction, e.g. \"0.06\""}
  },"required":["account_id","lump_pct","monthly_pct"]}}
},
"required":["horizon_years","splits"]}`),
		},
		{
			Name: "allocation_buckets",
			Description: "Every account money can be allocated to, with its id, kind (investment / debt / cash), " +
				"tax treatment, balance, existing monthly contribution, and — for a debt — its APR and minimum " +
				"payment. Call this before allocation_plan; it is where the account ids come from.",
			InputSchema: emptySchema,
		},
		{
			Name: "idle_cash",
			Description: "Cash sitting at a lower yield than the household already earns somewhere else, and what " +
				"moving it would be worth per year. Measured against the household's OWN best deposit rate, never a " +
				"market rate. Silent when no deposit yields have been entered — that is unknown, not zero.",
			InputSchema: emptySchema,
		},
		{
			Name: "asset_location",
			Description: "Which asset classes conventionally belong in which account type for tax efficiency, WITH " +
				"the assumption each rule rests on. This is DISCLOSURE, not a recommendation: the rules depend on the " +
				"household's marginal tax bracket, which Ledgermancy does not know. Say so when you use it.",
			InputSchema: emptySchema,
		},
		{
			Name: "college_projection",
			Description: "Every college goal against the four-year inflated cost of study: balance at enrollment, " +
				"per-year cost and coverage, the first year the money runs short, percent funded, and the extra " +
				"monthly contribution that would close the gap.",
			InputSchema: emptySchema,
		},
	}
}

// executeAllocationTool runs one of doc 32's tools.
//
// Returns ok=false for a name it does not own, so the caller falls through to
// the next executor — one dispatch point per file rather than one switch nobody
// can read.
func (s *Server) executeAllocationTool(
	ctx context.Context, identity auth.Identity, name string, input json.RawMessage,
) (out string, ok bool, err error) {
	switch name {
	case "allocation_plan", "allocation_buckets", "idle_cash", "asset_location", "college_projection":
	default:
		return "", false, nil
	}

	// asset_location depends on nothing the household has, which is the whole
	// point it makes. Answering it without a baseline read keeps that honest.
	if name == "asset_location" {
		return marshalToolOK(allocation.AssetLocation())
	}

	now := time.Now().UTC()
	baseline, err := allocation.AssembleBaseline(ctx, s.Queries, identity.HouseholdID, identity.UserID, now)
	if err != nil {
		return "", true, err
	}

	switch name {
	case "allocation_buckets":
		return marshalToolOK(bucketsToolResult(baseline))

	case "idle_cash":
		return marshalToolOK(allocation.BuildIdleCash(baseline))

	case "college_projection":
		// A college projection is a plan with no money in it: the drawdown runs
		// against what the 529 is ALREADY doing. Reusing Run rather than calling
		// the drawdown directly is what keeps this tool and the allocator page
		// from ever disagreeing.
		result, err := allocation.Run(baseline, allocation.Request{HorizonYears: collegeToolHorizon})
		if err != nil {
			return "", true, err
		}
		return marshalToolOK(collegeToolResult(result))

	case "allocation_plan":
		req, err := parseAllocationToolInput(input)
		if err != nil {
			// A bad tool input is the model's mistake, not the server's, and it
			// is recoverable: returning the reason lets the model fix the call
			// rather than the turn failing.
			return marshalToolOK(map[string]any{"error": err.Error()})
		}
		result, err := allocation.Run(baseline, req)
		if err != nil {
			return marshalToolOK(map[string]any{"error": err.Error()})
		}
		return marshalToolOK(result)
	}

	return "", false, nil
}

// collegeToolHorizon is long enough to reach any plausible enrollment year from
// a newborn. The college drawdown reads the series at the enrollment year, so a
// horizon shorter than that would clamp to the last point and understate the
// balance — silently, and in the flattering-to-nobody direction.
const collegeToolHorizon = 25

// bucketsToolResult flattens the baseline's buckets for the model. Money is
// finished; an unknown APR stays null so it cannot be read as 0%.
func bucketsToolResult(b allocation.Baseline) map[string]any {
	items := make([]map[string]any, 0, len(b.Buckets))
	for _, bucket := range b.Buckets {
		item := map[string]any{
			"account_id":       bucket.AccountID.String(),
			"name":             bucket.Name,
			"institution":      bucket.Institution,
			"kind":             string(bucket.Kind),
			"balance":          bucket.Balance.StringFixed(2),
			"existing_monthly": bucket.ExistingMonthly.StringFixed(2),
		}
		if bucket.Treatment != "" {
			item["tax_treatment"] = bucket.Treatment
		}
		if bucket.DepositAPY.Valid {
			item["deposit_apy"] = bucket.DepositAPY.Decimal.StringFixed(2)
		} else if bucket.Kind == allocation.BucketCash {
			item["deposit_apy"] = nil
		}
		if bucket.Kind == allocation.BucketDebt {
			if apr, ok := knownTerm(bucket.Terms.APR, bucket.Terms.APRSource); ok {
				item["apr"] = apr
			} else {
				item["apr"] = nil
			}
			if pay, ok := knownTerm(bucket.Terms.Payment, bucket.Terms.PaymentSource); ok {
				item["minimum_payment"] = pay
			} else {
				item["minimum_payment"] = nil
			}
		}
		items = append(items, item)
	}
	return map[string]any{
		"buckets":            items,
		"excluded_accounts":  b.UntaggedAccounts,
		"real_return_rate":   b.Assumptions.RealReturnRate.String(),
		"filing_status_set":  b.FilingStatus != "",
		"magi_known":         b.MAGI.Valid,
		"age_known":          b.AgeKnown,
		"eligibility_caveat": "Without a filing status and a MAGI on the household profile, Roth eligibility is reported as unknown — never as eligible.",
	}
}

// collegeToolResult is the college section on its own, plus the one-line summary
// per goal so the model has a sentence it can quote without doing arithmetic.
func collegeToolResult(r allocation.Result) map[string]any {
	items := make([]map[string]any, 0, len(r.College))
	for _, c := range r.College {
		items = append(items, map[string]any{
			"goal":                  c.Name,
			"projectable":           c.Projectable,
			"note":                  c.Note,
			"years_to_enrollment":   c.YearsToEnrollment,
			"years_of_study":        c.Years,
			"annual_cost_today":     c.AnnualCostToday.StringFixed(2),
			"balance_at_enrollment": c.BalanceAtEnrollment.StringFixed(2),
			"total_cost":            c.TotalCost.StringFixed(2),
			"total_shortfall":       c.TotalShortfall.StringFixed(2),
			"funded_pct":            c.FundedPct.String(),
			"first_shortfall_year":  c.FirstShortfallYear,
			"monthly_needed":        optionalMoneyString(c.MonthlyNeeded),
			"summary":               c.CollegeSummaryLine(),
			"basis":                 c.Basis,
		})
	}
	return map[string]any{"goals": items, "count": len(items)}
}

// optionalMoneyString renders an absent figure as JSON null rather than as
// "0.00". "We could not compute how much more you need" and "you need nothing
// more" are opposite answers.
func optionalMoneyString(d *decimal.Decimal) any {
	if d == nil {
		return nil
	}
	return d.StringFixed(2)
}

// allocationToolInput is the model-facing shape. Percentages and money arrive as
// strings for the same reason they do on the HTTP boundary: a JSON number is a
// float, and a float is how a $30,000.50 lump becomes $30,000.499999.
type allocationToolInput struct {
	Lump          string `json:"lump"`
	Monthly       string `json:"monthly"`
	HorizonYears  int    `json:"horizon_years"`
	TargetNestEgg string `json:"target_nest_egg"`
	Splits        []struct {
		AccountID      string `json:"account_id"`
		LumpPct        string `json:"lump_pct"`
		MonthlyPct     string `json:"monthly_pct"`
		RealReturnRate string `json:"real_return_rate"`
	} `json:"splits"`
}

func parseAllocationToolInput(raw json.RawMessage) (allocation.Request, error) {
	var in allocationToolInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return allocation.Request{}, fmt.Errorf("could not read the allocation input: %w", err)
	}
	out := allocation.Request{
		HorizonYears: in.HorizonYears,
		Splits:       make([]allocation.Split, 0, len(in.Splits)),
	}
	var err error
	if out.Lump, err = parseMoneyField(in.Lump, "lump", true); err != nil {
		return allocation.Request{}, err
	}
	if out.Monthly, err = parseMoneyField(in.Monthly, "monthly", true); err != nil {
		return allocation.Request{}, err
	}
	if strings.TrimSpace(in.TargetNestEgg) != "" {
		t, err := parseMoneyField(in.TargetNestEgg, "target_nest_egg", false)
		if err != nil {
			return allocation.Request{}, err
		}
		out.TargetNestEgg = decimal.NewNullDecimal(t)
	}
	for _, s := range in.Splits {
		id, err := uuid.Parse(strings.TrimSpace(s.AccountID))
		if err != nil {
			return allocation.Request{}, fmt.Errorf(
				"%q is not a valid account id — use the ids from allocation_buckets", s.AccountID)
		}
		split := allocation.Split{AccountID: id}
		if split.LumpPct, err = parseMoneyField(s.LumpPct, "lump_pct", true); err != nil {
			return allocation.Request{}, err
		}
		if split.MonthlyPct, err = parseMoneyField(s.MonthlyPct, "monthly_pct", true); err != nil {
			return allocation.Request{}, err
		}
		if strings.TrimSpace(s.RealReturnRate) != "" {
			rate, err := parseRate(s.RealReturnRate, "real_return_rate")
			if err != nil {
				return allocation.Request{}, err
			}
			split.RealReturnRate = decimal.NewNullDecimal(rate)
		}
		out.Splits = append(out.Splits, split)
	}
	return out, nil
}
