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
