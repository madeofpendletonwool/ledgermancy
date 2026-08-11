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
