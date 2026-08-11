package api

import (
	"encoding/json"
	"testing"
)

// The tool-set guards. These are the tests doc 31 asked for by name, and the
// reason they exist is that every failure they catch is a QUALITY cliff rather
// than an error: a set that quietly grows past the retrieval budget, or a
// classifier that answers differently on the same input, degrades answers
// without breaking anything a build would notice.

// Every advisor tool's schema must parse, and every one must have a
// description — an undescribed tool is one the model will never pick correctly.
func TestChatAdvisorToolDefs(t *testing.T) {
	defs := chatAdvisorToolDefs()
	if len(defs) == 0 {
		t.Fatal("no advisor tools defined")
	}
	seen := map[string]bool{}
	for _, d := range defs {
		if seen[d.Name] {
			t.Errorf("duplicate tool %q", d.Name)
		}
		seen[d.Name] = true
		if d.Description == "" {
			t.Errorf("tool %q has no description", d.Name)
		}
		var schema map[string]any
		if err := json.Unmarshal(d.InputSchema, &schema); err != nil {
			t.Errorf("tool %q has invalid input schema: %v", d.Name, err)
		}
	}
}

// THE CAP IS THE POINT OF THE SETS. A set that grows past it has to be split,
// and finding that out as a build failure is the whole reason this is a test
// rather than a runtime clamp.
func TestToolSetsAreUnderTheCap(t *testing.T) {
	for _, set := range ToolSets() {
		defs := toolSetDefs(set)
		if len(defs) > maxToolsPerSet {
			t.Errorf("tool set %q has %d definitions, over the cap of %d — split the set",
				set, len(defs), maxToolsPerSet)
		}
		if len(defs) == 0 {
			t.Errorf("tool set %q is empty", set)
		}
	}
}

// Every name in the membership table must resolve to a real definition.
// toolSetDefs FILTERS the catalogue, so a typo there would silently produce a
// smaller set rather than an error.
func TestToolSetMembersAllExist(t *testing.T) {
	catalogue := map[string]bool{}
	for _, d := range chatBaseToolDefs() {
		catalogue[d.Name] = true
	}
	for _, d := range chatAdvisorToolDefs() {
		catalogue[d.Name] = true
	}
	for _, d := range chatAllocationToolDefs() {
		catalogue[d.Name] = true
	}
	for _, d := range chatLikelihoodToolDefs() {
		catalogue[d.Name] = true
	}

	for _, set := range ToolSets() {
		for _, name := range toolSetNames(set) {
			if !catalogue[name] {
				t.Errorf("tool set %q names %q, which is not defined anywhere", set, name)
			}
		}
	}
}

// The briefing and safe_to_spend are in EVERY set, because every advisor
// conversation starts from the same two places.
func TestCommonToolsAreInEverySet(t *testing.T) {
	for _, set := range ToolSets() {
		present := map[string]bool{}
		for _, d := range toolSetDefs(set) {
			present[d.Name] = true
		}
		for _, want := range []string{"advisor_briefing", "safe_to_spend"} {
			if !present[want] {
				t.Errorf("tool set %q is missing %q", set, want)
			}
		}
	}
}

// The classifier's contract is determinism, not cleverness: the same message
// must select the same set, twice and forever. A model-chosen set would fail
// this, which is exactly why it is not one.
func TestClassifyToolSetIsDeterministic(t *testing.T) {
	cases := map[string]string{
		"How many times did I eat out in July?":      ToolSetSpending,
		"What are my biggest merchants this month?":  ToolSetSpending,
		"Is my dining spending up vs last month?":    ToolSetSpending,
		"When will my credit card be paid off?":      ToolSetPlanning,
		"Am I on track to retire at 55?":             ToolSetPlanning,
		"How much contribution room do I have left?": ToolSetPlanning,
		"Will I overdraft before payday?":            ToolSetPlanning,
		"What should I do with $30,000?":             ToolSetModelling,
		"where should i put my bonus":                ToolSetModelling,

		// Doc 33 moved the likelihood phrasings into their own set. "What are
		// the odds" used to select modelling, which held the allocator but no
		// simulation — the tools that actually answer it live in the likelihood
		// set now, so that is where it routes.
		"what are the odds I hit my number by 60?": ToolSetLikelihood,
		"how likely is Plan A to work?":            ToolSetLikelihood,
		"compare my plans and tell me which wins":  ToolSetLikelihood,
		"am I on track with the plan I saved?":     ToolSetLikelihood,
	}

	for message, want := range cases {
		got := classifyToolSet(message)
		if got != want {
			t.Errorf("classifyToolSet(%q) = %q, want %q", message, got, want)
		}
		// Twice, on the same input. The determinism claim is load-bearing: it
		// is what makes a wrong pick reproducible and therefore fixable.
		if again := classifyToolSet(message); again != got {
			t.Errorf("classifyToolSet(%q) is not deterministic: %q then %q", message, got, again)
		}
	}
}

// An unrecognised question falls back to the spending set, so nothing that
// worked before doc 31 stops working because a keyword was missing.
func TestClassifyToolSetDefaultsToSpending(t *testing.T) {
	for _, message := range []string{"", "hello", "what is the meaning of all this"} {
		if got := classifyToolSet(message); got != ToolSetSpending {
			t.Errorf("classifyToolSet(%q) = %q, want the spending default", message, got)
		}
	}
}

// The classifier reads the LAST user turn, not the whole transcript: a
// conversation that opened with a debt question and moved on to spending must
// follow the user rather than staying pinned to planning.
func TestLastUserMessageFollowsTheConversation(t *testing.T) {
	transcript := []chatMessage{
		{Role: "user", Content: "When will my credit card be paid off?"},
		{Role: "assistant", Content: "…"},
		{Role: "user", Content: "and what did I spend at Costco last month?"},
	}
	last := lastUserMessage(transcript)
	if last != "and what did I spend at Costco last month?" {
		t.Fatalf("lastUserMessage = %q", last)
	}
	if got := classifyToolSet(last); got != ToolSetSpending {
		t.Errorf("follow-up classified as %q, want the spending set", got)
	}
}

// hasTool reports whether a tool is offered in a set. Used to assert the one
// whose absence caused the fabrication — college_projection — is actually
// reachable on a follow-up turn.
func hasTool(set, tool string) bool {
	for _, name := range toolSetNames(set) {
		if name == tool {
			return true
		}
	}
	return false
}

// THE REGRESSION: an ambiguous follow-up in a thread that was using a deeper
// set must inherit that set, so the engine the turn depends on stays
// available. This is the exact transcript shape that produced a fabricated 529
// figure: the thread opened on a modelling question (college_projection was
// reachable), then a follow-up with no keyword — "I fixed the birthday tool so
// you should see Hazel's birthday now" — dropped to spending and the model
// invented a number instead of computing one.
func TestFollowUpInheritsTheThreadSet(t *testing.T) {
	transcript := []chatMessage{
		{Role: "user", Content: "what should I do with $2000 a month?"},
		{Role: "assistant", Content: "…"},
		{Role: "user", Content: "I also just fixed your tool so that you should see Hazel's birthday now."},
	}
	got := classifyFromMessages(transcript)
	if got != ToolSetModelling {
		t.Fatalf("follow-up classified as %q, want %q (inherited from the thread)", got, ToolSetModelling)
	}
	if !hasTool(got, "college_projection") {
		t.Error("college_projection is not reachable on the follow-up — the fabrication will recur")
	}
}

// A college follow-up inherits college_projection even when the thread's
// established set is planning (college_projection now lives in planning too,
// so a first-turn "save for education" that routes to planning also reaches it).
func TestCollegeProjectionReachableOnPlanningFollowUp(t *testing.T) {
	transcript := []chatMessage{
		{Role: "user", Content: "I'm saving for my son's education"}, // routes to planning via "saving for"
		{Role: "assistant", Content: "…"},
		{Role: "user", Content: "now what's the monthly number?"}, // no keyword
	}
	got := classifyFromMessages(transcript)
	if got != ToolSetPlanning {
		t.Fatalf("follow-up classified as %q, want %q", got, ToolSetPlanning)
	}
	if !hasTool(got, "college_projection") {
		t.Error("college_projection missing from planning — a first-turn education question loses it")
	}
}

// THE REGRESSION FROM THE LIVE BUG REPORT: a monthly_trend follow-up that
// explains WHY it is dropping a month — "because of a big loan payoff" —
// contains the planning keyword "loan", so the last-message rule routes the
// turn to planning. monthly_trend lives in the spending set, so without it in
// planning too the advisor truthfully told the user it had no per-month
// income/spending tool, even though the tool (with custom ranges and exclude)
// had shipped the day before. The classifier route is correct — "loan payoff"
// genuinely is a planning cue — so the fix is the same shape as
// college_projection's: the tool has to be in the set the misroute lands in.
func TestMonthlyTrendReachableOnLoanPayoffFollowUp(t *testing.T) {
	transcript := []chatMessage{
		{Role: "user", Content: "Average money leftover at the end of each month over the course of the last year."},
		{Role: "assistant", Content: "…"},
		{Role: "user", Content: "Can you do the math without this month - 2026-08 and last month - 2026-07 " +
			"because those are skewed because of a big loan payoff and this month income not coming in yet. " +
			"So just start 2 months prior maybe?"},
	}
	got := classifyFromMessages(transcript)
	if got != ToolSetPlanning {
		t.Fatalf("follow-up classified as %q, want %q (the \"loan payoff\" cue routes here)", got, ToolSetPlanning)
	}
	if !hasTool(got, "monthly_trend") {
		t.Error("monthly_trend missing from planning — the advisor will tell the user the tool does not exist")
	}
}

// The other half of the fix: a topic change INTO spending must still route to
// the transaction tools, even mid-thread. Once an ambiguous follow-up can
// inherit a deeper set, "and what did I spend at Costco" must not stay pinned
// to planning — the spending keywords exist precisely so this still works.
func TestTopicChangeToSpendingIsRespected(t *testing.T) {
	transcript := []chatMessage{
		{Role: "user", Content: "When will my credit card be paid off?"},
		{Role: "assistant", Content: "…"},
		{Role: "user", Content: "and what did I spend at Costco last month?"},
	}
	got := classifyFromMessages(transcript)
	if got != ToolSetSpending {
		t.Errorf("follow-up classified as %q, want %q (explicit spending keyword)", got, ToolSetSpending)
	}
}

// An ambiguous turn with no established topic behind it still defaults to
// spending, so the inheritance does not invent a deeper set from nothing.
func TestAmbiguousFirstTurnStillDefaultsToSpending(t *testing.T) {
	transcript := []chatMessage{
		{Role: "user", Content: "hello there"},
	}
	if got := classifyFromMessages(transcript); got != ToolSetSpending {
		t.Errorf("first-turn ambiguous classified as %q, want the spending default", got)
	}
}

// The inheritance window is bounded: a topic raised many turns ago must not pin
// the present. A thread that opened on modelling and then wandered through
// several ambiguous replies lands back on spending once the modelling turn is
// outside the window.
func TestInheritanceIsBoundedByTheWindow(t *testing.T) {
	transcript := []chatMessage{
		{Role: "user", Content: "what should I do with $30000?"}, // modelling, but long ago
	}
	// Nine ambiguous user/assistant pairs push the modelling turn past the
	// eight-turn window.
	for i := 0; i < 9; i++ {
		transcript = append(transcript,
			chatMessage{Role: "assistant", Content: "…"},
			chatMessage{Role: "user", Content: "okay thanks"},
		)
	}
	if got := classifyFromMessages(transcript); got != ToolSetSpending {
		t.Errorf("stale topic pinned the turn to %q, want spending (outside the window)", got)
	}
}

// toolInt clamps an out-of-range window and defaults an absent one, so a model
// that sends a silly number gets a sensible one instead of a failed turn. An
// UNREADABLE value is not silly, it is unknown: it gets an error the model can
// correct on the next iteration, because quietly substituting the default there
// answers a question nobody asked and reads exactly like a real answer.
func TestToolInt(t *testing.T) {
	cases := []struct {
		input string
		want  int
	}{
		{`{}`, 30},
		{`{"days":7}`, 7},
		{`{"days":0}`, 30},
		{`{"days":9999}`, 30},
		{`{"other":5}`, 30}, // absent field is optional, not an error
	}
	for _, c := range cases {
		got, err := toolInt(json.RawMessage(c.input), "days", 30, 1, 365)
		if err != nil {
			t.Errorf("toolInt(%s) unexpected error: %v", c.input, err)
			continue
		}
		if got != c.want {
			t.Errorf("toolInt(%s) = %d, want %d", c.input, got, c.want)
		}
	}

	for _, in := range []string{`{"days":"soon"}`, `not json`, `{"days":1.5}`} {
		if _, err := toolInt(json.RawMessage(in), "days", 30, 1, 365); err == nil {
			t.Errorf("toolInt(%s) = nil error, want a retryable decode error", in)
		}
	}
}

// "APRIL" IS NOT AN APR. The planning keyword "apr" was matched as a bare
// substring, and planning is checked before spending, so every message
// mentioning that month routed to planning and lost spending_summary,
// list_transactions and spend_by_category. A twelfth of the calendar could not
// ask the app how much it had spent — the same shape as the 529 fabrication:
// the tool that answers the question was not in the set.
func TestMonthNamesDoNotRouteToPlanning(t *testing.T) {
	for _, msg := range []string{
		"How much did I spend in April?",
		"what were my biggest purchases in april",
		"how much did we spend on dining out in April",
	} {
		got := classifyToolSet(msg)
		if got != ToolSetSpending {
			t.Errorf("%q routed to %q, want spending — \"apr\" matched inside \"april\"", msg, got)
		}
		if !hasTool(got, "spending_summary") {
			t.Errorf("%q lands in a set without spending_summary", msg)
		}
	}
}

// The whole-word treatment must not cost the acronym its real matches. "apr"
// and "ira" still select planning when they are actually words.
func TestAcronymKeywordsStillMatch(t *testing.T) {
	cases := map[string]string{
		"what's the APR on the Quicksilver?": ToolSetPlanning,
		"0% apr, is that worth it":           ToolSetPlanning,
		"how much room is left in my IRA":    ToolSetPlanning,
		"are my IRAs maxed out":              ToolSetPlanning,
		"April rent and my apr question":     ToolSetPlanning, // a rejected hit must not hide a real one
	}
	for msg, want := range cases {
		if got := classifyToolSet(msg); got != want {
			t.Errorf("%q routed to %q, want %q", msg, got, want)
		}
	}
}

// The bare verb "spend" is how the question is usually asked, and it matched
// nothing. On a first turn that fell to the spending default and looked fine;
// inside a planning thread the inheritance rule then pinned it to planning and
// took the transaction tools away. The same sentence must reach the same tools
// either way.
func TestBareSpendVerbIsASpendingKeyword(t *testing.T) {
	const q = "how much did I spend on gas last month"

	if got := classifyToolSet(q); got != ToolSetSpending {
		t.Errorf("%q classified as %q, want spending", q, got)
	}

	inThread := []chatMessage{
		{Role: "user", Content: "how's my debt payoff looking"},
		{Role: "assistant", Content: "…"},
		{Role: "user", Content: q},
	}
	if got := classifyFromMessages(inThread); got != ToolSetSpending {
		t.Errorf("inside a planning thread %q inherited %q — a plain spending question "+
			"must not lose the transaction tools", q, got)
	}
}

// "Safe to spend" is the deliberate overlap: it contains the spending keyword
// "spend" and is a planning question. Check order is what resolves it, so this
// pins the order rather than the keyword lists.
func TestSafeToSpendStaysPlanning(t *testing.T) {
	if got := classifyToolSet("am I safe to spend $500 this week"); got != ToolSetPlanning {
		t.Errorf("routed to %q, want planning — order must break the spend/safe-to-spend overlap", got)
	}
}
