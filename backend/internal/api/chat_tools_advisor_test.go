package api

import (
	"strings"
	"testing"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/advisor"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/allocation"
	"github.com/shopspring/decimal"
)

// The household's assumed real return and the APR hurdle are DIFFERENT numbers
// whenever the assumed return is below the 6% floor, and the model conflated
// them in conversation — it told a 3%-household its return was "6%". The
// briefing surfaces both, labeled, so the model reads the actual figure rather
// than substituting the floor. This pins that surface: 3% in, "6% floor" out,
// never equal.
func TestBriefingSurfacesRealReturnDistinctFromHurdle(t *testing.T) {
	b := advisor.Briefing{
		Assumptions: advisor.Assumptions{
			RealReturn:  decimal.RequireFromString("0.03"), // the household's actual rate
			Inflation:   decimal.RequireFromString("0.0331"),
			Hurdle:      decimal.NewFromInt(6), // the floor, because 3% < 6%
			HurdleBasis: "the 6% floor (your assumed real return is lower)",
		},
	}

	res := briefingToolResult(b, nil)

	ret, ok := res["assumed_real_return"].(string)
	if !ok {
		t.Fatalf("assumed_real_return missing or not a string: %v", res["assumed_real_return"])
	}
	hurdle, ok := res["apr_hurdle"].(string)
	if !ok {
		t.Fatalf("apr_hurdle missing or not a string: %v", res["apr_hurdle"])
	}

	// The whole point: the household's rate (3%) is not the hurdle (6%). If a
	// change collapses these, the model will again quote the floor as the
	// household's return.
	if ret == hurdle {
		t.Fatalf("assumed_real_return == apr_hurdle (%s) — the two figures must be distinct", ret)
	}
	if want := "3"; ret != want {
		t.Errorf("assumed_real_return = %q, want %q (3%%)", ret, want)
	}
	if want := "6"; hurdle != want {
		t.Errorf("apr_hurdle = %q, want %q (the 6%% floor)", hurdle, want)
	}
	if basis, _ := res["apr_hurdle_basis"].(string); basis == "" {
		t.Error("apr_hurdle_basis is empty — the model needs it to tell the two apart")
	}
}

// A college goal carries the rate ITS OWN drawdown used, distinct from the
// household default sitting in the same payload.
//
// This is the exact shape that produced the bug. The briefing emitted a college
// goal's monthly-needed figure — computed at the linked 529's own 6% — with no
// rate beside it, and a top-level assumed_real_return of 3%. Asked "what's our
// assumed return rate on that?", the model had exactly one rate in front of it,
// quoted the household's, and asserted it was what the college projection
// compounded at. It then defended that against a household correctly telling it
// the account was set to 6%, reaching for apr_hurdle as the explanation.
//
// Both rates must be present, labeled, and NOT equal when the account carries
// its own. If a change drops account_real_return_pct from this payload, the
// model is back to having one rate and one wrong answer.
func TestBriefingCollegeCarriesItsOwnReturnRate(t *testing.T) {
	b := advisor.Briefing{
		Assumptions: advisor.Assumptions{
			RealReturn:  decimal.RequireFromString("0.03"), // household default
			Inflation:   decimal.RequireFromString("0.0331"),
			Hurdle:      decimal.NewFromInt(6),
			HurdleBasis: "the 6% floor (your assumed real return is lower)",
		},
		College: []advisor.CollegeBrief{{
			Name:        "Get Hazel Through College",
			Projectable: true,
			// The 529's own rate, which is what the drawdown actually used.
			AccountRealReturnPct: decimal.NewFromInt(6),
			ReturnRateSource:     allocation.ReturnRateFromAccount,
		}},
	}

	res := briefingToolResult(b, nil)

	goals, ok := res["college"].([]map[string]any)
	if !ok || len(goals) != 1 {
		t.Fatalf("college = %v, want one goal", res["college"])
	}
	rate, ok := goals[0]["account_real_return_pct"].(string)
	if !ok {
		t.Fatalf("account_real_return_pct missing or not a string: %v", goals[0]["account_real_return_pct"])
	}
	if want := "6"; rate != want {
		t.Errorf("account_real_return_pct = %q, want %q (the 529's own rate)", rate, want)
	}
	if src, _ := goals[0]["return_rate_source"].(string); src != allocation.ReturnRateFromAccount {
		t.Errorf("return_rate_source = %q, want %q", src, allocation.ReturnRateFromAccount)
	}

	// The household rate is still there, still correct, and still a DIFFERENT
	// number. Both must be present: the bug was not a wrong household rate, it
	// was the absence of anything to distinguish it from the goal's.
	household, _ := res["assumed_real_return"].(string)
	if want := "3"; household != want {
		t.Errorf("assumed_real_return = %q, want %q (the household default)", household, want)
	}
	if rate == household {
		t.Fatal("the goal's rate and the household default are identical — the two are no longer distinguishable")
	}
}

// An unprojectable goal carries no rate, for the same reason it carries no
// funding figures: nothing ran. A "0" here would be read as "projected at 0%".
func TestBriefingUnprojectableCollegeGoalHasNoRate(t *testing.T) {
	b := advisor.Briefing{
		Assumptions: advisor.Assumptions{
			RealReturn: decimal.RequireFromString("0.03"), HurdleBasis: "x",
		},
		College: []advisor.CollegeBrief{{
			Name:        "Get Hazel Through College",
			Projectable: false,
			Note:        "No account is linked to this goal.",
		}},
	}

	goals, ok := briefingToolResult(b, nil)["college"].([]map[string]any)
	if !ok || len(goals) != 1 {
		t.Fatalf("want one goal, got %v", goals)
	}
	for _, key := range []string{"account_real_return_pct", "return_rate_source"} {
		if v, present := goals[0][key]; present {
			t.Errorf("%s present (%v) on an unprojectable goal; want it omitted", key, v)
		}
	}
}

// When the household's own return is ABOVE the floor, the hurdle IS the
// household's return — the two legitimately coincide, and the basis says so.
// This is the other half of the rule and must not be broken by the floor logic.
func TestBriefingHurdleEqualsAssumedReturnAboveFloor(t *testing.T) {
	b := advisor.Briefing{
		Assumptions: advisor.Assumptions{
			RealReturn:  decimal.RequireFromString("0.07"),
			Hurdle:      decimal.NewFromInt(7),
			HurdleBasis: "your assumed real return",
		},
	}
	res := briefingToolResult(b, nil)
	if ret, _ := res["assumed_real_return"].(string); ret != "7" {
		t.Errorf("assumed_real_return = %q, want 7", ret)
	}
	if hurdle, _ := res["apr_hurdle"].(string); hurdle != "7" {
		t.Errorf("apr_hurdle = %q, want 7 (equals the assumed return above the floor)", hurdle)
	}
}

// A household that has never opened the projections page has no
// projection_assumptions row, so fillRetirement returns before Assumptions is
// populated and the briefing's rates are not known. Reporting them as 0% would
// be the same substitution the whole rates surface exists to stop — the model
// was quoting the 6% floor as "your return", and a fabricated 0% is no better.
// Every rate key is emitted as null so the model cannot read them
// inconsistently (a null return beside a "0" hurdle would be the trap). This
// is the rule the system prompt already states verbatim: a null figure means
// "not known", never zero.
func TestBriefingAssumptionsNullWhenNoProjectionRow(t *testing.T) {
	// The no-row case: fillRetirement left Assumptions at its zero value, which
	// is the briefing's representation of "rates not loaded".
	b := advisor.Briefing{}

	res := briefingToolResult(b, nil)

	for _, key := range []string{"assumed_real_return", "assumed_inflation", "apr_hurdle", "apr_hurdle_basis"} {
		v, present := res[key]
		if !present {
			t.Errorf("%s missing from briefing; want it present as null", key)
			continue
		}
		if v != nil {
			t.Errorf("%s = %v, want nil (no projection_assumptions row means rates are not known, not zero)", key, v)
		}
	}
}

// THE HEDGE THIS EXISTS TO STOP. Asked how to invest $2,000/month, the advisor
// looked at a $98 balance in an account named "Individual 6601" and wrote "the
// 529 is nearly empty — worth considering if college costs are on the horizon",
// for a household whose one-year-old had a birthdate on file, a linked 529 and a
// named goal seventeen years out. Every fact was in the database; none of it was
// in the briefing, which is the tool the model calls first for its position.
func TestBriefingSurfacesCollegeGoalsAsFacts(t *testing.T) {
	age, years := 1, 17
	monthly := decimal.RequireFromString("305.42")
	b := advisor.Briefing{
		College: []advisor.CollegeBrief{{
			Name:               "Get Hazel in College",
			BeneficiaryAge:     &age,
			YearsToEnrollment:  &years,
			YearsOfStudy:       4,
			AnnualCostToday:    decimal.RequireFromString("20000"),
			Projectable:        true,
			TotalCost:          decimal.RequireFromString("83105.19"),
			TotalShortfall:     decimal.RequireFromString("82938.42"),
			FundedPct:          decimal.RequireFromString("0.2"),
			FirstShortfallYear: 1,
			MonthlyNeeded:      &monthly,
			Summary:            "Funded through year 0; $19,833.23 short in year 1.",
		}},
	}

	res := briefingToolResult(b, nil)
	list, ok := res["college"].([]map[string]any)
	if !ok {
		t.Fatalf("college missing or wrong type: %#v", res["college"])
	}
	if len(list) != 1 {
		t.Fatalf("college entries = %d, want 1", len(list))
	}
	got := list[0]
	if p, _ := got["beneficiary_age"].(*int); p == nil || *p != 1 {
		t.Errorf("beneficiary_age = %v, want 1 — without it the model cannot know college is real", got["beneficiary_age"])
	}
	if p, _ := got["years_to_enrollment"].(*int); p == nil || *p != 17 {
		t.Errorf("years_to_enrollment = %v, want 17", got["years_to_enrollment"])
	}
	if got["annual_cost_today"] != "20000.00" {
		t.Errorf("annual_cost_today = %v, want the ONE-year figure 20000.00", got["annual_cost_today"])
	}
	// The standing rides along. Knowing the goal exists and NOT knowing whether
	// it is on track reproduced the same hedge one step later: the model would
	// name the goal and then decline to say anything about it rather than make a
	// second call. These figures are the drawdown's own — fillCollege reads them
	// off allocation.Run — so carrying them is not a second copy of anything.
	if got["funded_pct"] != "0.2" {
		t.Errorf("funded_pct = %v, want 0.2", got["funded_pct"])
	}
	if got["monthly_needed"] != "305.42" {
		t.Errorf("monthly_needed = %v, want 305.42", got["monthly_needed"])
	}
	if got["total_cost"] != "83105.19" {
		t.Errorf("total_cost = %v, want the FOUR-year inflated total 83105.19", got["total_cost"])
	}
	if got["total_shortfall"] != "82938.42" {
		t.Errorf("total_shortfall = %v, want 82938.42", got["total_shortfall"])
	}
	if got["first_shortfall_year"] != 1 {
		t.Errorf("first_shortfall_year = %v, want 1", got["first_shortfall_year"])
	}
	if s, _ := got["summary"].(string); s == "" {
		t.Error("no summary: it is the sentence the model quotes instead of doing arithmetic")
	}
	// total_cost and annual_cost_today must stay visibly different. Collapsing
	// them is how "$20,000 for college" gets said about an $83,000 obligation.
	if got["total_cost"] == got["annual_cost_today"] {
		t.Error("total_cost == annual_cost_today — one year is not four")
	}
	if basis, _ := got["basis"].(string); basis == "" {
		t.Error("no basis line: ONE year vs the whole cost is exactly the misreading this needs to prevent")
	}
}

// An unprojectable goal is NOT a 0%-funded goal. With no 529 linked, or no
// birthdate to resolve the horizon from, the drawdown has nothing to run — so
// the briefing carries the reason and no figures at all. Emitting a zeroed
// funded_pct here would be a worse failure than the hedge the college list
// exists to stop: "you are 0% funded" is a confident wrong answer, where "no
// account is linked to this goal" is actionable.
func TestBriefingCollegeOmitsFiguresWhenUnprojectable(t *testing.T) {
	age, years := 1, 17
	b := advisor.Briefing{
		College: []advisor.CollegeBrief{{
			Name:              "Get Hazel in College",
			BeneficiaryAge:    &age,
			YearsToEnrollment: &years,
			YearsOfStudy:      4,
			AnnualCostToday:   decimal.RequireFromString("20000"),
			Projectable:       false,
			Note:              "No account is linked to this goal, so there is no balance to project.",
		}},
	}

	got := briefingToolResult(b, nil)["college"].([]map[string]any)[0]
	if got["projectable"] != false {
		t.Errorf("projectable = %v, want false", got["projectable"])
	}
	if note, _ := got["note"].(string); note == "" {
		t.Error("no note — the reason it cannot be projected is the only useful thing to say")
	}
	for _, absent := range []string{"funded_pct", "monthly_needed", "total_cost", "total_shortfall", "first_shortfall_year"} {
		if _, present := got[absent]; present {
			t.Errorf("unprojectable goal carries %q = %v; it must carry no funding figure at all",
				absent, got[absent])
		}
	}
	// The FACTS survive: the beneficiary and the horizon are known regardless of
	// whether any money is linked, and they are what make the goal real.
	if p, _ := got["beneficiary_age"].(*int); p == nil || *p != 1 {
		t.Errorf("beneficiary_age = %v, want 1 even when unprojectable", got["beneficiary_age"])
	}
}

// A household with no college goals gets an empty list, not a missing key or a
// null the model reads as "unknown".
func TestBriefingCollegeEmptyWhenNoGoals(t *testing.T) {
	res := briefingToolResult(advisor.Briefing{}, nil)
	list, ok := res["college"].([]map[string]any)
	if !ok {
		t.Fatalf("college missing or wrong type: %#v", res["college"])
	}
	if len(list) != 0 {
		t.Errorf("college entries = %d, want 0", len(list))
	}
}

// The emergency fund must carry its target AS A DOLLAR FIGURE, not only as
// months and a monthly denominator. The system prompt forbids the model from
// multiplying, so "how much money do we need in liquid to hold the emergency
// fund target at all times" is only answerable when the product arrives
// finished. The first version of this surface shipped the two factors and the
// model correctly refused to combine them — the household asked for one number
// and got a refusal plus a math problem. This pins the product being present
// and the basis saying what it is measured against, because a fixed-cost
// target read as total spending is the opposite confusion.
func TestBriefingEmergencyFundCarriesDollarTarget(t *testing.T) {
	target := decimal.RequireFromString("30708.45") // 9 × 3412.05
	full := decimal.RequireFromString("72111.06")   // 9 × 8012.34
	months := decimal.RequireFromString("19.0")
	b := advisor.Briefing{
		Runway: advisor.Runway{
			Liquid:           decimal.RequireFromString("152535.72"),
			MonthlyFixed:     decimal.RequireFromString("3412.05"),
			TargetMonths:     9,
			TargetAmount:     &target,
			MonthlySpending:  decimal.RequireFromString("8012.34"),
			SpendingMonths:   6,
			FullTargetAmount: &full,
			FullMonths:       &months,
		},
	}

	ef, ok := briefingToolResult(b, nil)["emergency_fund"].(map[string]any)
	if !ok {
		t.Fatalf("emergency_fund missing or wrong type: %#v", briefingToolResult(b, nil)["emergency_fund"])
	}
	if got, _ := ef["target_amount"].(string); got != "30708.45" {
		t.Errorf("target_amount = %q, want 30708.45 — the product the model is forbidden from computing", got)
	}
	if basis, _ := ef["basis"].(string); basis == "" {
		t.Error("basis is empty — a fixed-cost target will be read as total spending without it")
	}

	// The FULL-SPENDING bar. It exists because the household asked for "9
	// months of full average cost" and the only dollar target in the payload
	// was the fixed-cost one — a correct number that was not the one asked
	// for, with nothing in the payload to widen it. All four fields travel
	// together; a target without its denominator cannot be caveated and a
	// denominator without its target is a number with no question.
	if got, _ := ef["target_amount_full_spending"].(string); got != "72111.06" {
		t.Errorf("target_amount_full_spending = %q, want 72111.06 (9 months of typical total spending)", got)
	}
	if got, _ := ef["typical_monthly_spending"].(string); got != "8012.34" {
		t.Errorf("typical_monthly_spending = %q, want 8012.34", got)
	}
	if got, _ := ef["months_covered_full_spending"].(string); got != "19" {
		t.Errorf("months_covered_full_spending = %q, want 19", got)
	}
	if n, _ := ef["typical_spending_months"].(int); n != 6 {
		t.Errorf("typical_spending_months = %d, want 6", n)
	}
	// The basis names both bars, because the model holding one figure and a
	// label that says "TWO bars" is what keeps it quoting the fixed-cost number
	// at a household that asked for the full-spending one.
	basis, _ := ef["basis"].(string)
	for _, want := range []string{"target_amount", "target_amount_full_spending", "FIXED"} {
		if !strings.Contains(basis, want) {
			t.Errorf("basis does not mention %q — the model cannot pick the right bar without it: %q", want, basis)
		}
	}
}

// A runway with no fixed costs on record carries NO dollar target, for the same
// reason it carries no months: a 0.00 would read as "target met" for a
// household whose outgoings are simply unmeasured.
func TestBriefingEmergencyFundOmitsTargetWithoutFixedCosts(t *testing.T) {
	res := briefingToolResult(advisor.Briefing{Runway: advisor.Runway{TargetMonths: 9}}, nil)
	ef, ok := res["emergency_fund"].(map[string]any)
	if !ok {
		t.Fatalf("emergency_fund missing or wrong type: %#v", res["emergency_fund"])
	}
	for _, absent := range []string{"target_amount", "months_covered"} {
		if _, present := ef[absent]; present {
			t.Errorf("runway without fixed costs carries %q = %v; unmeasured is not 0.00", absent, ef[absent])
		}
	}
}

// No spending history means no typical full month, and every field of the
// full-spending bar is absent together — a $0.00 full-spending target would
// read as "nine months of everything you spend costs nothing", which is the
// most wrong sentence this payload could produce.
func TestBriefingEmergencyFundOmitsFullBarWithoutSpendingHistory(t *testing.T) {
	target := decimal.RequireFromString("30708.45")
	b := advisor.Briefing{
		Runway: advisor.Runway{
			Liquid:       decimal.RequireFromString("152535.72"),
			MonthlyFixed: decimal.RequireFromString("3412.05"),
			TargetMonths: 9,
			TargetAmount: &target, // fixed bar present, full bar not
		},
	}

	ef, ok := briefingToolResult(b, nil)["emergency_fund"].(map[string]any)
	if !ok {
		t.Fatalf("emergency_fund missing or wrong type: %#v", briefingToolResult(b, nil)["emergency_fund"])
	}
	if got, _ := ef["target_amount"].(string); got != "30708.45" {
		t.Errorf("target_amount = %q, want 30708.45 — the fixed bar must stand alone", got)
	}
	for _, absent := range []string{
		"target_amount_full_spending", "typical_monthly_spending",
		"typical_spending_months", "months_covered_full_spending",
	} {
		if _, present := ef[absent]; present {
			t.Errorf("runway without spending history carries %q = %v", absent, ef[absent])
		}
	}
	if basis, _ := ef["basis"].(string); !strings.Contains(basis, "No typical total-spending") {
		t.Errorf("basis should say the full-spending figure does not exist yet: %q", basis)
	}
}
