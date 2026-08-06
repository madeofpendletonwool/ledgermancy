package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/advisor"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/ai"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/goals"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/networth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/reporting"
)

// The advisor's chat tools: the engines the app already ships, made reachable
// from the conversation.
//
// EVERY TOOL HERE WRAPS AN EXISTING ENGINE AND COMPUTES NOTHING NEW. That is the
// point of the file, and it is what keeps the chat's figures identical to the
// pages'. safe_to_spend calls reporting.BuildSafeToSpend, the same function the
// Budgets page and the advisor panel call; debt_payoff calls
// goals.ComputePayoff, the same function the Goals page calls. A reimplemented
// figure here would be a household told two different things about one number,
// which is worse than the tool not existing.
//
// Money is returned as StringFixed(2) — finished, so the model quotes it
// verbatim and never rounds. Percentages carry their own convention and are
// documented at each site.

// emptySchema is the input schema for a tool that takes no arguments.
var emptySchema = json.RawMessage(`{"type":"object","properties":{}}`)

// chatAdvisorToolDefs are the definitions doc 31 adds. They are filtered into
// sets by toolSetDefs; nothing sends all of them at once.
func chatAdvisorToolDefs() []ai.Tool {
	return []ai.Tool{
		{
			Name: "advisor_briefing",
			Description: "The household's opening position in one call: net worth, this month's slack, " +
				"financial-independence age, the debt-free date, emergency-fund runway, and the top few " +
				"things needing attention. Start here for any broad question about how the household is doing.",
			InputSchema: emptySchema,
		},
		{
			Name: "safe_to_spend",
			Description: "How much is left to spend freely this month after expected income, typical fixed costs, " +
				"discretionary budgets and goal contributions. This is the same figure the Budgets page shows. " +
				"It describes a TYPICAL month from trailing medians, not the current bank balance.",
			InputSchema: emptySchema,
		},
		{
			Name: "project_balance",
			Description: "Day-by-day account balance projection over the next N days from known scheduled bills. " +
				"Answers \"will I overdraft before payday / before {date}?\" — returns the lowest projected balance, " +
				"the date it happens, and whether any account goes negative.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"days":{"type":"integer","description":"How many days ahead, 1-365 (default 30)"}}}`),
		},
		{
			Name:        "upcoming_obligations",
			Description: "Scheduled bills and recurring obligations falling due in the next N days (default 14), with dates and amounts.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"days":{"type":"integer","description":"Window in days, 1-90 (default 14)"}}}`),
		},
		{
			Name: "debt_summary",
			Description: "Every debt the household carries: balance, APR, minimum payment, credit utilisation, and where " +
				"each figure came from. A rate or payment nobody has supplied is reported as unknown, never as zero.",
			InputSchema: emptySchema,
		},
		{
			Name: "debt_payoff",
			Description: "When a debt is paid off and what the interest costs, optionally with an extra monthly payment. " +
				"Answers \"if I put $200/month extra at this card, when does it die?\" — returns months, payoff date and " +
				"total interest both at the current payment and with the extra.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"account":{"type":"string","description":"Account name substring; omit for every debt"},"extra_monthly":{"type":"number","description":"Extra dollars per month to add to the payment"}}}`),
		},
		{
			Name:        "goal_status",
			Description: "Every savings and debt-payoff goal with its standing: progress, required monthly, shortfall, and whether it is on track.",
			InputSchema: emptySchema,
		},
		{
			Name: "goal_solve",
			Description: "How much per month is needed to hit a goal by a given date. Answers \"what would it take to have " +
				"$20k saved by next June?\" for an existing goal.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"goal":{"type":"string","description":"Goal name substring"},"target_date":{"type":"string","description":"Target date YYYY-MM-DD; omit to use the goal's own"}},"required":["goal"]}`),
		},
		{
			Name: "retirement_projection",
			Description: "The household's retirement projection: financial-independence age, nest egg at the target age, " +
				"and what it supports. Carries the assumptions it was computed under — the figures are an estimate, not a forecast.",
			InputSchema: emptySchema,
		},
		{
			Name:        "retirement_solve",
			Description: "The total monthly contribution needed to retire at a target age, and that figure as a share of income.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"target_age":{"type":"integer","description":"Retirement age to solve for; omit to use the household's stored target"}}}`),
		},
		{
			Name: "investment_performance",
			Description: "Portfolio returns over a period: time-weighted (the portfolio's return) and money-weighted " +
				"(the investor's actual outcome), with the span actually measured.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"period":{"type":"string","enum":["ytd","1y","3y","5y","inception"],"description":"Window (default ytd)"}}}`),
		},
		{
			Name:        "asset_allocation",
			Description: "How the portfolio is split, by asset class and by tax treatment. Holdings whose type is unknown are shown as their own slice, never redistributed.",
			InputSchema: emptySchema,
		},
		{
			Name: "fees_summary",
			Description: "Annual fund expense-ratio cost, with the share of the portfolio the figure could be computed over. " +
				"Coverage is usually partial and is always stated.",
			InputSchema: emptySchema,
		},
		{
			Name: "contribution_room",
			Description: "Remaining room under each IRS annual contribution cap (401(k), IRA, HSA), with what has been " +
				"contributed year to date. Reports the cap and whether the household is ELIGIBLE to use it as separate facts.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"year":{"type":"integer","description":"Tax year; omit for the current one"},"family_hsa":{"type":"boolean","description":"True if HSA coverage is family rather than self-only"}}}`),
		},
	}
}

// toolSetDefs returns the definitions for one set, in the set's declared order.
//
// It is built by FILTERING the full catalogue rather than by holding a second
// list of definitions, so a tool named in toolSetMembers but never defined is a
// missing entry the guard test catches, not a silently smaller set.
func toolSetDefs(set string) []ai.Tool {
	catalogue := map[string]ai.Tool{}
	for _, t := range chatBaseToolDefs() {
		catalogue[t.Name] = t
	}
	for _, t := range chatAllocationToolDefs() {
		catalogue[t.Name] = t
	}
	for _, t := range chatAdvisorToolDefs() {
		catalogue[t.Name] = t
	}

	names := toolSetNames(set)
	out := make([]ai.Tool, 0, len(names))
	for _, n := range names {
		if t, ok := catalogue[n]; ok {
			out = append(out, t)
		}
	}
	return out
}

// executeAdvisorTool runs one of this doc's tools.
//
// It returns ok=false for a name it does not own, so the caller falls through to
// the spending executor — one dispatch point per file, rather than one giant
// switch nobody can read.
func (s *Server) executeAdvisorTool(
	ctx context.Context, identity auth.Identity, name string, input json.RawMessage,
) (out string, ok bool, err error) {
	now := time.Now().UTC()

	switch name {
	case "advisor_briefing":
		b, err := advisor.BuildBriefing(ctx, s.Queries, identity.HouseholdID, now)
		if err != nil {
			return "", true, err
		}
		return marshalToolOK(briefingToolResult(b))

	case "safe_to_spend":
		sts, err := reporting.BuildSafeToSpend(ctx, s.Queries, identity.HouseholdID, now)
		if err != nil {
			return "", true, err
		}
		// The same fields GET /api/budgets/safe-to-spend returns, from the same
		// call. The agreement test asserts they match to the cent, and the only
		// way to keep that true is for both to be this one function.
		return marshalToolOK(map[string]any{
			"expected_income":           sts.ExpectedIncome.StringFixed(2),
			"fixed_costs":               sts.FixedCosts.StringFixed(2),
			"budgeted_discretionary":    sts.BudgetedDiscretionary.StringFixed(2),
			"goal_contributions":        sts.GoalContributions.StringFixed(2),
			"safe_to_spend":             sts.Amount.StringFixed(2),
			"income_months":             sts.IncomeMonths,
			"upcoming_obligations":      sts.UpcomingObligations.StringFixed(2),
			"safe_to_spend_after_bills": sts.AmountAfterBills.StringFixed(2),
			"obligation_coverage":       sts.ObligationCoverage,
			"basis": "A typical month, from trailing medians — not the current bank balance. " +
				"One-time-flagged transactions are excluded.",
		})

	case "project_balance":
		days := toolInt(input, "days", 30, 1, 365)
		est, err := reporting.BuildEstimatedProjection(ctx, s.Queries,
			identity.HouseholdID, identity.UserID, now, days)
		if err != nil {
			return "", true, err
		}
		c := est.Known.Combined
		accounts := make([]map[string]any, 0, len(est.Known.Accounts))
		for _, a := range est.Known.Accounts {
			accounts = append(accounts, map[string]any{
				"account":         a.Name,
				"current_balance": a.CurrentBalance.StringFixed(2),
				"lowest_balance":  a.LowestBalance.StringFixed(2),
				"lowest_date":     a.LowestDate.Format(time.DateOnly),
				"goes_negative":   a.GoesNegative,
			})
		}
		return marshalToolOK(map[string]any{
			"from":            est.Known.From.Format(time.DateOnly),
			"to":              est.Known.To.Format(time.DateOnly),
			"current_balance": c.CurrentBalance.StringFixed(2),
			"lowest_balance":  c.LowestBalance.StringFixed(2),
			"lowest_date":     c.LowestDate.Format(time.DateOnly),
			"goes_negative":   c.GoesNegative,
			"total_due":       est.Known.TotalDue.StringFixed(2),
			// Bills with no account named cannot be attributed to a balance, so
			// they are reported separately rather than dropped from the total.
			"unassigned_total": est.Known.UnassignedTotal.StringFixed(2),
			"accounts":         accounts,
			"basis": "Projected from KNOWN scheduled bills only. Unscheduled spending is not included, " +
				"so the real low point may be lower.",
		})

	case "upcoming_obligations":
		days := toolInt(input, "days", 14, 1, 90)
		userID := identity.UserID
		rows, err := s.Queries.ListUpcomingObligations(ctx, dbgen.ListUpcomingObligationsParams{
			HouseholdID: identity.HouseholdID, UserID: &userID,
			Column3: now, Column4: now.AddDate(0, 0, days),
		})
		if err != nil {
			return "", true, err
		}
		total := decimal.Zero
		items := make([]map[string]any, 0, len(rows))
		for _, r := range rows {
			total = total.Add(r.Amount)
			items = append(items, map[string]any{
				"label":    r.Label,
				"due_date": r.DueDate.Format(time.DateOnly),
				"amount":   r.Amount.StringFixed(2),
				"cadence":  fmt.Sprintf("every %d %s", r.IntervalCount, r.IntervalUnit),
			})
		}
		return marshalToolOK(map[string]any{
			"days": days, "count": len(items),
			"total": total.StringFixed(2), "obligations": items,
		})

	case "debt_summary":
		return marshalToolOK2(s.debtSummary(ctx, identity))

	case "debt_payoff":
		var in struct {
			Account      string      `json:"account"`
			ExtraMonthly json.Number `json:"extra_monthly"`
		}
		_ = json.Unmarshal(input, &in)
		extra, err := toolDecimal(in.ExtraMonthly)
		if err != nil {
			return "", true, err
		}
		return marshalToolOK2(s.debtPayoff(ctx, identity, in.Account, extra, now))

	case "goal_status":
		return marshalToolOK2(s.goalStatus(ctx, identity, now))

	case "goal_solve":
		var in struct {
			Goal       string `json:"goal"`
			TargetDate string `json:"target_date"`
		}
		_ = json.Unmarshal(input, &in)
		return marshalToolOK2(s.goalSolve(ctx, identity, in.Goal, in.TargetDate, now))

	case "retirement_projection":
		return marshalToolOK2(s.retirementTool(ctx, identity, 0, now))

	case "retirement_solve":
		targetAge := toolInt(input, "target_age", 0, 0, 120)
		return marshalToolOK2(s.retirementTool(ctx, identity, targetAge, now))

	case "investment_performance":
		var in struct {
			Period string `json:"period"`
		}
		_ = json.Unmarshal(input, &in)
		period := reporting.PeriodYTD
		if raw := strings.TrimSpace(in.Period); raw != "" {
			if !reporting.ValidPeriod(raw) {
				return "", true, fmt.Errorf("unknown period %q (use ytd, 1y, 3y, 5y or inception)", raw)
			}
			period = reporting.Period(raw)
		}
		perf, err := reporting.BuildPortfolioPerformance(ctx, s.Queries,
			identity.HouseholdID, identity.UserID, period, now)
		if err != nil {
			return "", true, err
		}
		result := map[string]any{
			"period":     string(perf.Period),
			"computable": perf.Computable,
		}
		if perf.HistoryCaveat != "" {
			result["history_caveat"] = perf.HistoryCaveat
		}
		if perf.Computable {
			result["measured_from"] = perf.Start.Format(time.DateOnly)
			result["measured_to"] = perf.End.Format(time.DateOnly)
			result["days"] = perf.Days
			result["start_value"] = perf.StartValue.StringFixed(2)
			result["end_value"] = perf.EndValue.StringFixed(2)
			result["net_flows"] = perf.NetFlows.StringFixed(2)
			result["gain"] = perf.Gain.StringFixed(2)
			// Returns are stored as fractions and quoted as percentages, which
			// is the unit a person reads. Two places, because a third digit on a
			// return implies a precision the snapshot cadence does not support.
			result["time_weighted_return_pct"] = pctOrNil(perf.TWR)
			result["money_weighted_return_pct"] = pctOrNil(perf.MWR)
			result["annualised_return_pct"] = pctOrNil(perf.Annualised)
			if perf.MWRNote != "" {
				result["money_weighted_note"] = perf.MWRNote
			}
		}
		return marshalToolOK(result)

	case "asset_allocation":
		rows, err := s.Queries.ListVisibleHoldingsDetailed(ctx, dbgen.ListVisibleHoldingsDetailedParams{
			HouseholdID: identity.HouseholdID, UserID: identity.UserID,
		})
		if err != nil {
			return "", true, err
		}
		return marshalToolOK(map[string]any{
			"by_asset_class":   allocationSlices(reporting.Allocation(rows)),
			"by_tax_treatment": allocationSlices(reporting.AllocationByTaxTreatment(rows)),
			"holdings_counted": len(rows),
		})

	case "fees_summary":
		rows, err := s.Queries.ListVisibleHoldingsDetailed(ctx, dbgen.ListVisibleHoldingsDetailedParams{
			HouseholdID: identity.HouseholdID, UserID: identity.UserID,
		})
		if err != nil {
			return "", true, err
		}
		// The ratio source is empty until one exists — ComputeFeeDrag's own note.
		// Handing it an invented map here would be exactly the misinformation
		// that function refuses to produce.
		drag := reporting.ComputeFeeDrag(rows, map[string]decimal.Decimal{})
		return marshalToolOK(map[string]any{
			"annual_cost":       drag.AnnualCost.StringFixed(2),
			"covered_value":     drag.CoveredValue.StringFixed(2),
			"uncovered_value":   drag.UncoveredValue.StringFixed(2),
			"covered_holdings":  drag.CoveredHoldings,
			"excluded_holdings": drag.ExcludedHoldings,
			"basis": "Computed only over holdings that report an expense ratio. " +
				"When coverage is zero, no fee figure is available — that is not the same as zero fees.",
		})

	case "contribution_room":
		year := toolInt(input, "year", now.Year(), 1900, 2200)
		var in struct {
			FamilyHSA bool `json:"family_hsa"`
		}
		_ = json.Unmarshal(input, &in)
		return marshalToolOK2(s.contributionRoom(ctx, identity, year, in.FamilyHSA, now))
	}

	return "", false, nil
}

// --------------------------------------------------------------------------
// The tools with enough body to want their own function
// --------------------------------------------------------------------------

// debtSummary lists every debt with its terms resolved through the SAME helper
// the Net Worth page, the Goals page and the advisor's ranker use.
//
// An unknown APR is reported as null with an `apr_source` of "", never as 0.00.
// A genuine 0% promotional card and a card whose rate nobody has supplied are
// different facts, and collapsing them is how the most expensive debt in a
// household gets presented as the cheapest.
func (s *Server) debtSummary(ctx context.Context, identity auth.Identity) (map[string]any, error) {
	rows, err := s.Queries.ListVisibleLiabilities(ctx, dbgen.ListVisibleLiabilitiesParams{
		HouseholdID: identity.HouseholdID, UserID: identity.UserID,
	})
	if err != nil {
		return nil, err
	}

	total, unknownAPR := decimal.Zero, 0
	items := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		balance := decimal.Zero
		if r.CurrentBalance.Valid {
			balance = r.CurrentBalance.Decimal
		}
		if !balance.IsPositive() {
			continue
		}
		total = total.Add(balance)

		terms := goals.MergeDebtTerms(
			r.ManualApr, r.Apr, r.InterestRatePercentage,
			r.ObligationAmount, r.ManualMinimumPayment, r.MinimumPayment,
		)
		item := map[string]any{
			"account":        r.AccountName,
			"type":           r.Type,
			"balance":        balance.StringFixed(2),
			"apr_source":     terms.APRSource,
			"payment_source": terms.PaymentSource,
		}
		if terms.APRSource == goals.TermSourceNone {
			item["apr"] = nil
			unknownAPR++
		} else {
			// A percentage, matching how every APR in this app is stored and
			// quoted: 22.99 means 22.99%.
			item["apr"] = terms.APR.StringFixed(2)
		}
		if terms.PaymentSource == goals.TermSourceNone {
			item["minimum_payment"] = nil
		} else {
			item["minimum_payment"] = terms.Payment.StringFixed(2)
		}
		if r.Type == "credit" && r.CreditLimit.Valid && r.CreditLimit.Decimal.IsPositive() {
			util := balance.Div(r.CreditLimit.Decimal).Mul(decimal.NewFromInt(100)).Round(1)
			item["credit_limit"] = r.CreditLimit.Decimal.StringFixed(2)
			item["utilisation_pct"] = util.String()
		}
		items = append(items, item)
	}

	return map[string]any{
		"count": len(items), "total_balance": total.StringFixed(2),
		"debts": items, "unknown_apr_count": unknownAPR,
	}, nil
}

// debtPayoff amortizes each matching debt, with and without an extra payment.
func (s *Server) debtPayoff(
	ctx context.Context, identity auth.Identity, match string, extra decimal.NullDecimal, now time.Time,
) (map[string]any, error) {
	rows, err := s.Queries.ListVisibleLiabilities(ctx, dbgen.ListVisibleLiabilitiesParams{
		HouseholdID: identity.HouseholdID, UserID: identity.UserID,
	})
	if err != nil {
		return nil, err
	}

	needle := strings.ToLower(strings.TrimSpace(match))
	extraAmount := decimal.Zero
	if extra.Valid && extra.Decimal.IsPositive() {
		extraAmount = extra.Decimal
	}

	items := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		if needle != "" && !strings.Contains(strings.ToLower(r.AccountName), needle) {
			continue
		}
		balance := decimal.Zero
		if r.CurrentBalance.Valid {
			balance = r.CurrentBalance.Decimal
		}
		if !balance.IsPositive() {
			continue
		}
		terms := goals.MergeDebtTerms(
			r.ManualApr, r.Apr, r.InterestRatePercentage,
			r.ObligationAmount, r.ManualMinimumPayment, r.MinimumPayment,
		)

		item := map[string]any{"account": r.AccountName, "balance": balance.StringFixed(2)}
		// A schedule needs both a rate and a payment. Missing either is reported
		// as such rather than defaulted: a zero APR flatters the debt and a zero
		// payment reports "never" for a reason that is ours, not the household's.
		if terms.APRSource == goals.TermSourceNone || terms.PaymentSource == goals.TermSourceNone {
			item["projectable"] = false
			item["reason"] = missingTermsReason(terms)
			items = append(items, item)
			continue
		}

		item["projectable"] = true
		item["apr"] = terms.APR.StringFixed(2)
		item["monthly_payment"] = terms.Payment.StringFixed(2)
		item["current"] = payoffToolResult(
			goals.ComputePayoff(balance, terms.APR, terms.Payment, nil, now))

		if extraAmount.IsPositive() {
			with := goals.ComputePayoff(balance, terms.APR, terms.Payment.Add(extraAmount), nil, now)
			item["with_extra"] = payoffToolResult(with)
			item["extra_monthly"] = extraAmount.StringFixed(2)
		}
		items = append(items, item)
	}

	out := map[string]any{"count": len(items), "debts": items}
	if extraAmount.IsPositive() {
		out["extra_monthly"] = extraAmount.StringFixed(2)
	}
	return out, nil
}

// missingTermsReason names which half of the terms is missing, so the answer is
// "add the rate" rather than "cannot compute".
func missingTermsReason(t goals.DebtTerms) string {
	switch {
	case t.APRSource == goals.TermSourceNone && t.PaymentSource == goals.TermSourceNone:
		return "no interest rate and no monthly payment are on file for this account"
	case t.APRSource == goals.TermSourceNone:
		return "no interest rate is on file for this account"
	default:
		return "no monthly payment is on file for this account"
	}
}

// payoffToolResult renders one PayoffFeasibility. The schedule is deliberately
// dropped: a 360-point amortization series in a tool result is tokens the model
// cannot use and will be tempted to summarise arithmetically.
func payoffToolResult(f goals.PayoffFeasibility) map[string]any {
	out := map[string]any{
		"never_pays_off":   f.NeverPaysOff,
		"monthly_interest": f.MonthlyInterest.StringFixed(2),
	}
	if f.NeverPaysOff {
		out["note"] = "the payment is at or below the monthly interest, so the balance never falls"
		return out
	}
	out["months"] = f.Months
	out["total_interest"] = f.TotalInterest.StringFixed(2)
	if f.PayoffDate != nil {
		out["payoff_date"] = f.PayoffDate.Format(time.DateOnly)
	}
	return out
}

// goalStatus reuses buildGoalResponse, so the chat and the Goals page cannot
// disagree about whether a goal is on track.
func (s *Server) goalStatus(ctx context.Context, identity auth.Identity, now time.Time) (map[string]any, error) {
	userID, personID, all := goalVisibility(identity)
	rows, err := s.Queries.ListGoals(ctx, dbgen.ListGoalsParams{
		HouseholdID: identity.HouseholdID, UserID: userID,
		PersonID: personID, AllPersonGoals: all,
	})
	if err != nil {
		return nil, err
	}

	items := make([]map[string]any, 0, len(rows))
	behind := 0
	for _, g := range rows {
		resp, err := s.buildGoalResponse(ctx, g, now)
		if err != nil {
			return nil, err
		}
		if !resp.OnTrack && !resp.Achieved {
			behind++
		}
		item := map[string]any{
			"name":             resp.Name,
			"kind":             resp.Kind,
			"target_amount":    resp.TargetAmount,
			"current_amount":   resp.CurrentAmount,
			"required_monthly": resp.RequiredMonthly,
			"shortfall":        resp.Shortfall,
			"months_left":      resp.MonthsLeft,
			"on_track":         resp.OnTrack,
			"open_ended":       resp.OpenEnded,
			"achieved":         resp.Achieved,
		}
		if resp.TargetDate != nil {
			item["target_date"] = *resp.TargetDate
		}
		if resp.Payoff != nil && resp.Payoff.Available {
			item["payoff_date"] = resp.Payoff.PayoffDate
			item["never_pays_off"] = resp.Payoff.NeverPaysOff
		}
		items = append(items, item)
	}
	return map[string]any{"count": len(items), "behind": behind, "goals": items}, nil
}

// goalSolve answers "how much a month to hit this by then".
//
// It dispatches on kind for the same reason buildGoalResponse does: a savings
// goal divides a remainder by months, a payoff goal has to amortize. Using the
// accumulation answer on a debt understates the payment by the whole of the
// interest.
func (s *Server) goalSolve(
	ctx context.Context, identity auth.Identity, match, targetDate string, now time.Time,
) (map[string]any, error) {
	if strings.TrimSpace(match) == "" {
		return nil, fmt.Errorf("goal is required — name the goal to solve for")
	}

	userID, personID, all := goalVisibility(identity)
	rows, err := s.Queries.ListGoals(ctx, dbgen.ListGoalsParams{
		HouseholdID: identity.HouseholdID, UserID: userID,
		PersonID: personID, AllPersonGoals: all,
	})
	if err != nil {
		return nil, err
	}

	needle := strings.ToLower(strings.TrimSpace(match))
	var goal *dbgen.Goal
	for i := range rows {
		if strings.Contains(strings.ToLower(rows[i].Name), needle) {
			goal = &rows[i]
			break
		}
	}
	if goal == nil {
		return nil, fmt.Errorf("no goal matching %q", match)
	}

	// An explicit date overrides the goal's own, which is the whole point of the
	// tool: "what if I wanted it by June instead".
	when := goal.TargetDate
	if raw := strings.TrimSpace(targetDate); raw != "" {
		parsed, err := time.Parse(time.DateOnly, raw)
		if err != nil {
			return nil, fmt.Errorf("invalid target_date %q (use YYYY-MM-DD)", raw)
		}
		when = &parsed
	}
	if when == nil {
		return nil, fmt.Errorf("goal %q is open-ended — give a target_date to solve for", goal.Name)
	}

	out := map[string]any{
		"goal": goal.Name, "kind": goal.Kind,
		"target_amount": goal.TargetAmount.StringFixed(2),
		"target_date":   when.Format(time.DateOnly),
	}

	if goal.Kind == goalKindPayoff {
		resp, err := s.buildGoalResponse(ctx, *goal, now)
		if err != nil {
			return nil, err
		}
		if resp.Payoff == nil || !resp.Payoff.Available {
			reason := "this payoff goal has no linked debt terms to amortize"
			if resp.Payoff != nil && resp.Payoff.Reason != "" {
				reason = resp.Payoff.Reason
			}
			return nil, fmt.Errorf("%s", reason)
		}
		balance, err := decimal.NewFromString(resp.Payoff.Balance)
		if err != nil {
			return nil, err
		}
		apr, err := decimal.NewFromString(resp.Payoff.APR)
		if err != nil {
			return nil, err
		}
		payment, _ := decimal.NewFromString(resp.Payoff.MonthlyPayment)
		f := goals.ComputePayoff(balance, apr, payment, when, now)
		out["required_monthly"] = f.RequiredMonthly.StringFixed(2)
		out["reachable"] = f.TargetReachable
		out["months_left"] = f.MonthsLeft
		out["current_payment"] = payment.StringFixed(2)
		out["basis"] = "Interest-aware: the required payment amortizes the balance at its APR, " +
			"so it is larger than the balance divided by the months."
		return out, nil
	}

	current, err := reporting.GoalProgress(ctx, s.Queries, *goal, now)
	if err != nil {
		return nil, err
	}
	surplus, err := networth.DefaultMonthlySurplus(ctx, s.Queries,
		identity.HouseholdID, identity.UserID, now)
	if err != nil {
		return nil, err
	}
	f := goals.Compute(goal.TargetAmount, current, surplus, when, now)
	out["current_amount"] = current.StringFixed(2)
	out["remaining"] = f.Remaining.StringFixed(2)
	out["required_monthly"] = f.RequiredMonthly.StringFixed(2)
	out["months_left"] = f.MonthsLeft
	out["monthly_surplus"] = surplus.StringFixed(2)
	out["shortfall"] = f.Shortfall.StringFixed(2)
	out["on_track"] = f.OnTrack
	out["achieved"] = f.Achieved
	return out, nil
}

// retirementTool runs the projection, and the savings-rate solve when a target
// age is available. solveAge of 0 means "use the household's stored target".
func (s *Server) retirementTool(
	ctx context.Context, identity auth.Identity, solveAge int, now time.Time,
) (map[string]any, error) {
	stored, err := s.loadAssumptions(ctx, identity.HouseholdID)
	if err != nil {
		return nil, err
	}
	defaulted, err := s.defaultTargetSpending(ctx, identity, now)
	if err != nil {
		return nil, err
	}
	rows, err := s.Queries.ListProjectableAccounts(ctx, dbgen.ListProjectableAccountsParams{
		HouseholdID: identity.HouseholdID, UserID: identity.UserID,
	})
	if err != nil {
		return nil, err
	}

	a := toRetirementAssumptions(stored, defaulted, nil, now)
	a.Months = defaultProjectionMonths(stored)
	if solveAge > 0 {
		a.TargetRetirementAge = solveAge
		// The horizon has to reach the age being solved for, or the projection
		// answers a question nobody asked.
		if years := solveAge - a.CurrentAge; years > 0 && (years+5)*12 > a.Months {
			a.Months = (years + 5) * 12
		}
	}

	plans := toAccountPlans(rows, now)
	proj := networth.ProjectRetirement(plans, a, now)

	out := map[string]any{
		"current_age":            a.CurrentAge,
		"target_retirement_age":  a.TargetRetirementAge,
		"real_return_rate_pct":   a.RealReturnRate.Mul(decimal.NewFromInt(100)).Round(2).String(),
		"withdrawal_rate_pct":    a.WithdrawalRate.Mul(decimal.NewFromInt(100)).Round(2).String(),
		"target_annual_spending": a.TargetAnnualSpending.StringFixed(2),
		"fi_age":                 proj.FIAge,
		"already_fi":             proj.AlreadyFI,
		"limits_year":            proj.LimitsYear,
		"limits_configured":      proj.LimitsConfigured,
		"excluded_accounts":      proj.ExcludedAccounts,
		"excluded_value":         proj.ExcludedValue.StringFixed(2),
		"basis": "An estimate at a flat real return, not a market forecast. " +
			"Tax on withdrawals and required minimum distributions are NOT modelled.",
	}
	if proj.NestEggAtTarget != nil {
		out["nest_egg_at_target"] = proj.NestEggAtTarget.StringFixed(2)
	}
	if proj.SupportedAtTarget != nil {
		out["supported_spending_at_target"] = proj.SupportedAtTarget.StringFixed(2)
	}

	// The solve needs both ages. Without them it is omitted rather than returned
	// empty, so the model has nothing to phrase as though it were an answer.
	if a.TargetRetirementAge > 0 && a.CurrentAge > 0 {
		income, err := s.annualIncome(ctx, identity, now)
		if err != nil {
			return nil, err
		}
		solve := networth.SolveRequiredSavings(plans, a, income, now)
		solved := map[string]any{"reachable": solve.Reachable}
		if solve.Note != "" {
			solved["note"] = solve.Note
		}
		if solve.RequiredMonthly != nil {
			solved["required_extra_monthly"] = solve.RequiredMonthly.StringFixed(2)
		}
		if solve.RequiredRate != nil {
			solved["required_rate_pct"] = solve.RequiredRate.Mul(decimal.NewFromInt(100)).Round(2).String()
		}
		solved["annual_income"] = income.StringFixed(2)
		out["required_savings"] = solved
	}
	return out, nil
}

// contributionRoom MUST NOT CONFLATE A CAP WITH PERMISSION.
//
// AnnualLimitFor answers "what is the annual cap for this account type at this
// age" and nothing else — it has no idea whether the household is ALLOWED to
// contribute. A Roth IRA has a MAGI phase-out, and above it the correct headroom
// is $0, not $7,500. An HSA requires HDHP coverage. So each group carries three
// separate facts:
//
//   - annual_limit — from AnnualLimitFor, null where it declines to cap.
//   - used_ytd     — real confirmed-paystub deferrals (doc 23 shipped, so this
//     is a figure and not a null; unverified is still reported when no stub is
//     on file, because zero-by-default implies full headroom).
//   - eligibility  — eligible / phased_out / ineligible / unknown.
//
// Doc 31 shipped the SHAPE with eligibility hard-coded to "unknown"; doc 32
// filled it in from networth.EligibilityFor, and the per-group mapping below is
// the part worth reading:
//
//   - 401k — no MAGI test exists. Always eligible.
//   - ira  — the group's representative treatment is trad_ira, which has no
//     CONTRIBUTION phase-out (its phase-out is on DEDUCTIBILITY, a different
//     question). The limit the household may actually be barred from using is
//     the ROTH one, so the check is run against roth_ira and reported as such.
//   - hsa  — needs HDHP coverage for the month, which this app has no data for.
//     Permanently `unknown`, and it says why.
//
// A household with no filing status or no MAGI on file gets `unknown` with a
// note, never `eligible`.
func (s *Server) contributionRoom(
	ctx context.Context, identity auth.Identity, taxYear int, familyHSA bool, now time.Time,
) (map[string]any, error) {
	year, stubCount, err := s.buildPayrollYear(ctx, identity, taxYear)
	if err != nil {
		return nil, err
	}
	age, ageKnown := s.resolveCallerAge(ctx, identity, now)

	// The eligibility inputs. A failed read here must not take the headroom
	// figures with it — a cap the household can see is worth more than a 500 —
	// so the household read degrades to "no filing status, no MAGI", which the
	// eligibility check reports as `unknown` anyway.
	filingStatus, magi := "", decimal.NullDecimal{}
	if household, herr := s.Queries.GetHousehold(ctx, identity.HouseholdID); herr == nil {
		if household.FilingStatus != nil {
			filingStatus = *household.FilingStatus
		}
		// A MAGI is a statement about ONE tax year; one from a different year is
		// treated as absent rather than silently reused. Same rule the allocator
		// applies.
		if household.Magi.Valid && household.MagiTaxYear != nil && int(*household.MagiTaxYear) == taxYear {
			magi = household.Magi
		}
	}

	headroom, configured := year.ContributionHeadroom(age, familyHSA)
	groups := make([]map[string]any, 0, len(headroom))
	for _, h := range headroom {
		elig, yearOK := groupEligibility(h.Group, taxYear, filingStatus, magi, h.Limit)
		g := map[string]any{
			"group":            h.Group,
			"label":            h.Label,
			"annual_limit":     h.Limit.StringFixed(2),
			"used_ytd":         h.Contributed.StringFixed(2),
			"remaining":        h.Remaining.StringFixed(2),
			"over_by":          h.OverBy.StringFixed(2),
			"eligibility":      elig.Status,
			"eligibility_note": elig.Note,
			// Without a confirmed stub the year's deferrals are not zero, they
			// are unmeasured — and reporting them as zero implies full headroom
			// that may not exist.
			"used_ytd_verified": stubCount > 0,
		}
		// Where the household is inside the Roth phase-out, the cap that
		// actually applies is smaller than the group's. Both are reported: the
		// group limit is still the traditional-IRA cap, and the reduced one is
		// what a Roth contribution is held to.
		if elig.Status == networth.EligibilityPhasedOut {
			g["eligible_limit"] = elig.Limit.StringFixed(2)
		}
		if !yearOK {
			g["eligibility_year_configured"] = false
		}
		if h.PeriodsLeft != nil {
			g["pay_periods_left"] = *h.PeriodsLeft
		}
		if h.PerPeriod != nil {
			g["per_remaining_paycheck"] = h.PerPeriod.StringFixed(2)
		}
		groups = append(groups, g)
	}

	out := map[string]any{
		"tax_year":          taxYear,
		"limits_configured": configured,
		"age_known":         ageKnown,
		"paystub_count":     stubCount,
		"groups":            groups,
	}
	if !configured {
		out["note"] = fmt.Sprintf(
			"IRS limits for %d are not configured (the table runs through %d), so no cap can be reported.",
			taxYear, networth.LatestConfiguredYear())
	}
	// Taxable, 529, trust and UTMA have no federal annual deferral cap, and
	// AnnualLimitFor honestly declines to invent one. Saying so is better than
	// omitting them and letting a reader conclude they were forgotten.
	out["uncapped_account_types"] = []string{"taxable", "529", "trust", "utma"}
	out["filing_status_set"] = filingStatus != ""
	out["magi_known"] = magi.Valid
	return out, nil
}

// groupEligibility maps a shared-limit GROUP onto the eligibility question that
// actually applies to it. See the comment on contributionRoom for why the IRA
// group is checked as a Roth.
func groupEligibility(
	group string, taxYear int, filingStatus string, magi decimal.NullDecimal, limit decimal.Decimal,
) (networth.Eligibility, bool) {
	switch group {
	case "ira":
		return networth.EligibilityFor("roth_ira", taxYear, filingStatus, magi, limit)
	case "hsa":
		return networth.EligibilityFor("hsa", taxYear, filingStatus, magi, limit)
	default:
		// A 401(k) elective deferral has no income test. Saying "eligible" here
		// is a claim this app can actually stand behind.
		return networth.Eligibility{Status: networth.EligibilityEligible, Limit: limit}, true
	}
}

// --------------------------------------------------------------------------
// Small shared helpers
// --------------------------------------------------------------------------

// briefingToolResult flattens a Briefing for the model. Money is finished; the
// nullable fields stay nullable so "not reached" cannot be read as a number.
func briefingToolResult(b advisor.Briefing) map[string]any {
	debtFree := map[string]any{
		"never":          b.DebtFree.Never,
		"projected":      b.DebtFree.Projected,
		"excluded":       b.DebtFree.Excluded,
		"excluded_names": b.DebtFree.ExcludedNames,
		"total_balance":  b.DebtFree.TotalBalance.StringFixed(2),
	}
	if b.DebtFree.Date != nil {
		debtFree["date"] = b.DebtFree.Date.Format(time.DateOnly)
	}
	if b.DebtFree.NeverAccount != "" {
		debtFree["never_because"] = b.DebtFree.NeverAccount
	}
	// The one clause a reader has to see, because the alternative reading is
	// much more flattering and much more wrong.
	debtFree["basis"] = "The date the LAST debt clears, not the first."

	runway := map[string]any{
		"liquid":              b.Runway.Liquid.StringFixed(2),
		"monthly_fixed_costs": b.Runway.MonthlyFixed.StringFixed(2),
		"target_months":       b.Runway.TargetMonths,
	}
	if b.Runway.Months != nil {
		runway["months_covered"] = b.Runway.Months.String()
	}

	attention := make([]map[string]any, 0, len(b.Attention))
	for _, a := range b.Attention {
		attention = append(attention, map[string]any{
			"title": a.Title, "body": a.Body, "kind": a.Kind, "priority": a.Priority,
		})
	}

	out := map[string]any{
		"net_worth":            b.NetWorth.StringFixed(2),
		"assets":               b.Assets.StringFixed(2),
		"debts":                b.Debts.StringFixed(2),
		"monthly_slack":        b.MonthlySlack.StringFixed(2),
		"slack_basis":          b.SlackBasis,
		"income_months":        b.IncomeMonths,
		"retirement_projected": b.RetirementProjected,
		"already_fi":           b.AlreadyFI,
		"fi_age":               b.FIAge,
		"debt_free":            debtFree,
		"emergency_fund":       runway,
		"attention":            attention,
	}
	return out
}

// allocationSlices renders a slice list with percentages as bare numbers.
func allocationSlices(slices []reporting.AllocationSlice) []map[string]any {
	out := make([]map[string]any, 0, len(slices))
	for _, s := range slices {
		out = append(out, map[string]any{
			"label": s.Label,
			"value": s.Value.StringFixed(2),
			// Already 0–100 and rounded by the engine; String rather than
			// StringFixed(2) because a share is read, not summed.
			"percent": s.Percent.String(),
		})
	}
	return out
}

// pctOrNil renders a fraction as a percentage string, or nil when the engine
// could not compute it. Nil is a real answer here — see Performance.MWR.
func pctOrNil(fraction *decimal.Decimal) any {
	if fraction == nil {
		return nil
	}
	return fraction.Mul(decimal.NewFromInt(100)).Round(2).String()
}

// toolInt reads one optional integer field, clamped, with a default.
func toolInt(input json.RawMessage, field string, def, min, max int) int {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(input, &raw); err != nil {
		return def
	}
	v, ok := raw[field]
	if !ok {
		return def
	}
	var n int
	if err := json.Unmarshal(v, &n); err != nil {
		return def
	}
	if n < min || n > max {
		return def
	}
	return n
}

// marshalToolOK adapts a value to executeAdvisorTool's (out, owned, err) shape.
func marshalToolOK(v any) (string, bool, error) {
	out, err := marshalTool(v)
	return out, true, err
}

// marshalToolOK2 is marshalToolOK for a helper that already returns an error.
func marshalToolOK2(v any, err error) (string, bool, error) {
	if err != nil {
		return "", true, err
	}
	return marshalToolOK(v)
}
