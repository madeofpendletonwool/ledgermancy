package likelihood

import (
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Doc 33's "guardrail structure, step by step" verification.
//
// The rule's entire claim is that it is DETERMINISTIC AND QUOTABLE, so these
// tests are less about arithmetic than about the rule meaning one thing. Each
// one names the clause it pins.

// candidate builds a plan with the figures the rule reads. Everything else about
// a simulation is irrelevant here.
func candidate(name string, success, sigma, drawdown string, meetsGoals bool, missed ...string) Candidate {
	rate := dec(success)
	if missed == nil {
		missed = []string{}
	}
	return Candidate{
		PlanID: uuid.NewSHA1(uuid.Nil, []byte(name)),
		Name:   name,
		Sim: Result{
			Runs:        1000,
			SuccessRate: &rate,
			Sigma:       dec(sigma),
			DrawdownP5:  dec(drawdown),
		},
		GoalFit: GoalFit{MeetsAll: meetsGoals, Stated: 1, Missed: missed},
	}
}

func floor(pct string) decimal.NullDecimal {
	return decimal.NewNullDecimal(dec(pct))
}

func byName(r Ranking, name string) (RankedPlan, bool) {
	for _, p := range r.Plans {
		if p.Name == name {
			return p, true
		}
	}
	return RankedPlan{}, false
}

// --------------------------------------------------------------------------
// F1 — the drawdown floor is DISQUALIFYING, not a penalty
// --------------------------------------------------------------------------

// A plan breaching the floor is excluded EVEN WHEN ITS SUCCESS RATE IS HIGHEST.
// This is the clause most likely to be implemented as a demotion, and a
// demotion would let the rule recommend a plan the household cannot sit through.
func TestF1ExcludesBreachEvenWithBestSuccessRate(t *testing.T) {
	got, err := Rank([]Candidate{
		// The best success rate in the set, and a drawdown past the floor.
		candidate("Aggressive", "0.97", "100000", "0.34", true),
		candidate("Balanced", "0.88", "80000", "0.19", true),
	}, floor("25"))
	if err != nil {
		t.Fatal(err)
	}

	if got.TopPick == nil {
		t.Fatal("no top pick, but Balanced clears the floor")
	}
	pick, _ := byName(got, "Balanced")
	if !pick.TopPick {
		t.Errorf("top pick is not Balanced — F1 must exclude the deeper drawdown outright, not merely demote it")
	}
	dropped, _ := byName(got, "Aggressive")
	if !dropped.Excluded || dropped.ExcludedBy != "F1" {
		t.Errorf("Aggressive excluded=%v by %q, want excluded by F1", dropped.Excluded, dropped.ExcludedBy)
	}
	if dropped.Reason == "" {
		t.Error("an excluded plan must say why")
	}
}

// EVERY plan breaching the floor returns NO PICK with that reason — not a
// least-bad choice. This is the answer the model is forbidden to resolve by
// quietly naming a plan.
func TestF1AllBreachMeansNoPick(t *testing.T) {
	got, err := Rank([]Candidate{
		candidate("Aggressive", "0.97", "100000", "0.34", true),
		candidate("Wild", "0.91", "120000", "0.41", true),
	}, floor("25"))
	if err != nil {
		t.Fatal(err)
	}

	if got.TopPick != nil {
		t.Fatalf("top pick = %v, want none — every plan breaches the floor", *got.TopPick)
	}
	if got.NoPickReason == "" {
		t.Error("no-pick must carry its reason")
	}
	for _, p := range got.Plans {
		if p.TopPick {
			t.Errorf("%s is flagged as the top pick despite breaching the floor", p.Name)
		}
	}
}

// With no floor on file F1 is SKIPPED, and the answer says so rather than
// ranking as though the floor had been met.
func TestF1SkippedWithoutAFloor(t *testing.T) {
	got, err := Rank([]Candidate{
		candidate("Aggressive", "0.97", "100000", "0.34", true),
		candidate("Balanced", "0.88", "80000", "0.19", true),
	}, decimal.NullDecimal{})
	if err != nil {
		t.Fatal(err)
	}

	if got.FloorApplied {
		t.Error("FloorApplied is true with no floor on file")
	}
	pick, _ := byName(got, "Aggressive")
	if !pick.TopPick {
		t.Error("with F1 skipped the highest success rate should win")
	}
	if !contains(got.Explanation, "no drawdown floor") {
		t.Errorf("the explanation must say the floor check was skipped, got: %s", got.Explanation)
	}
}

// --------------------------------------------------------------------------
// F2 — goal fit is judged at the MEDIAN SIMULATED OUTCOME
// --------------------------------------------------------------------------

// A plan that misses a goal is excluded at F2 even with the better success rate,
// as long as something else meets every goal.
func TestF2ExcludesPlansMissingAGoal(t *testing.T) {
	got, err := Rank([]Candidate{
		candidate("Lean", "0.95", "70000", "0.18", false, "College"),
		candidate("Full", "0.80", "90000", "0.20", true),
	}, floor("25"))
	if err != nil {
		t.Fatal(err)
	}

	pick, _ := byName(got, "Full")
	if !pick.TopPick {
		t.Error("top pick should be the plan that meets every goal, even with the lower success rate")
	}
	dropped, _ := byName(got, "Lean")
	if !dropped.Excluded || dropped.ExcludedBy != "F2" {
		t.Errorf("Lean excluded=%v by %q, want excluded by F2", dropped.Excluded, dropped.ExcludedBy)
	}
	if !contains(dropped.Reason, "College") {
		t.Errorf("the reason must name the goal that fell short, got: %s", dropped.Reason)
	}
}

// GOAL FIT IS JUDGED AT P50, NOT AT THE COMPOUND-AT-μ FIGURE. EvaluateGoals is
// where that is decided, and this is the test that pins it: a goal funded at the
// deterministic projection but not at the median simulated outcome must FAIL.
func TestEvaluateGoalsUsesTheMedianNotTheProjection(t *testing.T) {
	sim := Result{
		ByAccount: map[string]AccountOutcome{
			// The median lands below the goal's target; a compound-at-mu figure
			// would have been higher and would have passed.
			"529": {Name: "College fund", P10: dec("60000"), P50: dec("90000"), P90: dec("140000")},
		},
	}
	fit := EvaluateGoals([]GoalTarget{
		{Name: "College", AccountID: "529", Linked: true, Target: dec("100000")},
	}, sim)

	if fit.MeetsAll {
		t.Error("a goal whose linked account medians BELOW its target must not count as met")
	}
	if len(fit.Missed) != 1 || fit.Missed[0] != "College" {
		t.Errorf("Missed = %v, want [College]", fit.Missed)
	}

	// And it passes once the median clears the target.
	sim.ByAccount["529"] = AccountOutcome{P50: dec("120000")}
	if fit := EvaluateGoals([]GoalTarget{
		{Name: "College", AccountID: "529", Linked: true, Target: dec("100000")},
	}, sim); !fit.MeetsAll {
		t.Error("a goal funded at the median must count as met")
	}
}

// An unlinked goal is NOT judged. Doc 32 already reports that the plan cannot
// fund it; failing the plan for a goal the allocator cannot reach would blame
// the plan for a missing link.
func TestEvaluateGoalsIgnoresUnlinkedAndAchievedGoals(t *testing.T) {
	sim := Result{ByAccount: map[string]AccountOutcome{}}
	fit := EvaluateGoals([]GoalTarget{
		{Name: "Unlinked", Linked: false, Target: dec("100000")},
		{Name: "Done", AccountID: "x", Linked: true, Achieved: true, Target: dec("100000")},
	}, sim)

	if !fit.MeetsAll {
		t.Errorf("unlinked and achieved goals must not be judged, Missed = %v", fit.Missed)
	}
	if fit.Stated != 0 {
		t.Errorf("Stated = %d, want 0", fit.Stated)
	}
}

// --------------------------------------------------------------------------
// F3 — no plan meets every goal
// --------------------------------------------------------------------------

// When F2 empties the set, the rule falls back to the F1 survivors and MARKS
// the result, which the AI is required to state.
func TestF3FallsBackAndSaysSo(t *testing.T) {
	got, err := Rank([]Candidate{
		candidate("A", "0.70", "70000", "0.18", false, "College"),
		candidate("B", "0.60", "60000", "0.19", false, "College", "House"),
	}, floor("25"))
	if err != nil {
		t.Fatal(err)
	}

	if !got.NoPlanMeetsEveryGoal {
		t.Error("NoPlanMeetsEveryGoal must be set when F2 empties the set")
	}
	if got.TopPick == nil {
		t.Fatal("F3 must still produce a pick from the F1 survivors")
	}
	pick, _ := byName(got, "A")
	if !pick.TopPick {
		t.Error("F3's pick should be the best of the F1 survivors by the sort keys")
	}
	if !contains(got.Explanation, "No plan here meets every stated goal") {
		t.Errorf("the explanation must state that no plan meets every goal, got: %s", got.Explanation)
	}
}

// --------------------------------------------------------------------------
// S1 / S2 / S3 — the sort is TOTAL
// --------------------------------------------------------------------------

// S2: on an equal success rate, LESS SPREAD WINS.
func TestS2BreaksTiesOnSigma(t *testing.T) {
	got, err := Rank([]Candidate{
		candidate("Wide", "0.90", "120000", "0.18", true),
		candidate("Tight", "0.90", "60000", "0.18", true),
	}, floor("25"))
	if err != nil {
		t.Fatal(err)
	}
	pick, _ := byName(got, "Tight")
	if !pick.TopPick {
		t.Error("on an equal success rate the tighter distribution must win (S2)")
	}
}

// S3: two plans identical on EVERY computed figure still produce one stable
// pick, by name — and the same one however the input is ordered.
func TestS3MakesTheOrderTotal(t *testing.T) {
	forward, err := Rank([]Candidate{
		candidate("Zeta", "0.90", "60000", "0.18", true),
		candidate("Alpha", "0.90", "60000", "0.18", true),
	}, floor("25"))
	if err != nil {
		t.Fatal(err)
	}
	reversed, err := Rank([]Candidate{
		candidate("Alpha", "0.90", "60000", "0.18", true),
		candidate("Zeta", "0.90", "60000", "0.18", true),
	}, floor("25"))
	if err != nil {
		t.Fatal(err)
	}

	if *forward.TopPick != *reversed.TopPick {
		t.Fatal("the pick depends on input order — the sort is not total")
	}
	pick, _ := byName(forward, "Alpha")
	if !pick.TopPick {
		t.Error("tied on every figure, the alphabetically first name must win (S3)")
	}
}

// Determinism end to end: the same set twice names the same pick with the same
// cited figures.
func TestRankIsDeterministic(t *testing.T) {
	set := []Candidate{
		candidate("A", "0.91", "70000", "0.18", true),
		candidate("B", "0.91", "70000", "0.18", true),
		candidate("C", "0.84", "50000", "0.12", true),
	}
	first, err := Rank(set, floor("25"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Rank(set, floor("25"))
	if err != nil {
		t.Fatal(err)
	}
	if *first.TopPick != *second.TopPick || first.Explanation != second.Explanation {
		t.Error("Rank is not deterministic over the same input")
	}
}

// --------------------------------------------------------------------------
// Pinned n, and the comparison cap
// --------------------------------------------------------------------------

// A comparison assembled from runs at DIFFERING run counts is REFUSED rather
// than rendered. Both figures the rule sorts on move with n.
func TestPinnedRunCountIsEnforced(t *testing.T) {
	a := candidate("A", "0.90", "60000", "0.18", true)
	b := candidate("B", "0.85", "60000", "0.18", true)
	b.Sim.Runs = 10000

	if _, err := Rank([]Candidate{a, b}, floor("25")); err == nil {
		t.Fatal("a comparison across differing run counts must be refused, not rendered")
	} else if err != ErrRunCountMismatch {
		t.Errorf("err = %v, want ErrRunCountMismatch", err)
	}
}

// Over the cap the comparison is refused rather than silently ranking the first
// four of six.
func TestComparisonSetIsCapped(t *testing.T) {
	set := make([]Candidate, 0, MaxComparisonPlans+1)
	for _, n := range []string{"A", "B", "C", "D", "E"} {
		set = append(set, candidate(n, "0.90", "60000", "0.18", true))
	}
	if _, err := Rank(set, floor("25")); err == nil {
		t.Fatalf("more than %d plans must be refused", MaxComparisonPlans)
	}
}

func TestRankRefusesAnEmptySet(t *testing.T) {
	if _, err := Rank(nil, floor("25")); err == nil {
		t.Error("an empty comparison must be refused")
	}
}

// --------------------------------------------------------------------------
// The explanation
// --------------------------------------------------------------------------

// The pick's explanation must CITE the computed figures that placed it, because
// that is what the model is required to quote and forbidden to replace.
func TestExplanationCitesTheFigures(t *testing.T) {
	got, err := Rank([]Candidate{
		candidate("Balanced", "0.94", "80000", "0.19", true),
		candidate("Lean", "0.71", "60000", "0.15", true),
	}, floor("25"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Balanced", "94", "19", "25"} {
		if !contains(got.Explanation, want) {
			t.Errorf("the explanation must cite %q, got: %s", want, got.Explanation)
		}
	}
	if got.Rule == "" {
		t.Error("the ranking must carry the rule text so the user can read what produced the pick")
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
