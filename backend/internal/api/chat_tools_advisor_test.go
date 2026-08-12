package api

import (
	"testing"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/advisor"
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

	res := briefingToolResult(b)

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
	res := briefingToolResult(b)
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

	res := briefingToolResult(b)

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
	b := advisor.Briefing{
		College: []advisor.CollegeBrief{{
			Name:              "Get Hazel in College",
			BeneficiaryAge:    &age,
			YearsToEnrollment: &years,
			YearsOfStudy:      4,
			AnnualCostToday:   decimal.RequireFromString("20000"),
		}},
	}

	res := briefingToolResult(b)
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
	// The funding numbers must NOT be here. They are college_projection's, and a
	// second copy is how two surfaces start disagreeing — which is the entire
	// history of this feature.
	for _, banned := range []string{"funded_pct", "monthly_needed", "total_cost", "balance_at_enrollment"} {
		if _, present := got[banned]; present {
			t.Errorf("briefing carries %q — that figure belongs to college_projection alone", banned)
		}
	}
	if basis, _ := got["basis"].(string); basis == "" {
		t.Error("no basis line: ONE year vs the whole cost is exactly the misreading this needs to prevent")
	}
}

// A household with no college goals gets an empty list, not a missing key or a
// null the model reads as "unknown".
func TestBriefingCollegeEmptyWhenNoGoals(t *testing.T) {
	res := briefingToolResult(advisor.Briefing{})
	list, ok := res["college"].([]map[string]any)
	if !ok {
		t.Fatalf("college missing or wrong type: %#v", res["college"])
	}
	if len(list) != 0 {
		t.Errorf("college entries = %d, want 0", len(list))
	}
}
