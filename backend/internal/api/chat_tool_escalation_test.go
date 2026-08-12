package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/ai"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/config"
)

// The escape hatch's guards. Every failure they catch has the same shape as the
// two bugs that produced it: the app can do the thing, the model was not shown
// the tool, and the answer that came back was a refusal or an invented number.

// THE PROPERTY THAT MAKES THIS TOTAL: every tool is reachable from every set.
//
// This is the assertion the previous two fixes could not make. Duplicating
// college_projection into planning, then monthly_trend into planning, each
// closed one route for one tool; neither could say anything about the tool that
// has not been misrouted yet. If this passes, no question can be permanently
// unanswerable because of where the classifier sent it.
func TestEveryToolIsReachableFromEverySet(t *testing.T) {
	for _, set := range ToolSets() {
		available := map[string]bool{}
		for _, d := range toolSetDefs(set) {
			available[d.Name] = true
		}
		for _, want := range chatToolCatalogue() {
			if available[want.Name] {
				continue
			}
			in := mustJSON(t, toolFinderQuery{Names: []string{want.Name}})
			_, granted, err := grantTools(in, available, 0)
			if err != nil {
				t.Fatalf("set %q: find_tools(%q): %v", set, want.Name, err)
			}
			if len(granted) != 1 || granted[0].Name != want.Name {
				t.Errorf("set %q: find_tools could not reach %q (got %v)",
					set, want.Name, toolNames(granted))
			}
		}
	}
}

// The reported bug, end to end at the classifier + escalation seam.
//
// The transcript is the real one from the issue: a cashflow question that
// worked, then a follow-up naming a loan payoff. The follow-up genuinely IS a
// planning cue, so it genuinely routes to planning — the route is not the bug
// and this test asserts it stays. What must not happen again is the turn ending
// with "I don't have a per-month breakdown tool" while monthly_trend sits in
// the catalogue.
func TestLoanPayoffFollowUpCanStillReachMonthlyTrend(t *testing.T) {
	transcript := []chatMessage{
		{Role: "user", Content: "Average money leftover at the end of each month over the course of the last year."},
		{Role: "assistant", Content: "Over the last 12 months, your average monthly leftover is $1,111.09."},
		{Role: "user", Content: "Can you do the math without this month - 2026-08 and last month - 2026-07 because those are skewed because of a big loan payoff and this month income not coming in yet."},
	}

	set := classifyFromMessages(transcript)
	if set != ToolSetPlanning {
		t.Fatalf("classifier routed the follow-up to %q; \"loan payoff\" is a planning cue and should still route there", set)
	}

	// Planning carries monthly_trend directly today, which is the fast path.
	// The point of this test is the SLOW path underneath it: prove the turn
	// recovers even from a set that does not carry the tool, because the next
	// misroute will be to a set nobody has patched.
	for _, from := range ToolSets() {
		available := map[string]bool{}
		for _, d := range toolSetDefs(from) {
			available[d.Name] = true
		}
		delete(available, "monthly_trend")

		out, granted, err := grantTools(
			mustJSON(t, toolFinderQuery{Need: "per-month income and spending totals over a custom date range, excluding some months"}),
			available, 0,
		)
		if err != nil {
			t.Fatalf("set %q: %v", from, err)
		}
		if !contains(toolNames(granted), "monthly_trend") {
			t.Errorf("set %q: a plain-language description of the reported question did not surface monthly_trend; got %v",
				from, toolNames(granted))
		}
		// And even if the search had missed, the catalogue in the same result
		// names it — a miss costs a round trip, never the answer.
		if !strings.Contains(out, "monthly_trend") {
			t.Errorf("set %q: monthly_trend appears nowhere in the find_tools result", from)
		}
	}
}

// A find_tools call NEVER dead-ends. Whatever it is asked, the result names
// tools the model has not got — so "there is no tool for that" is a claim it
// can only make after reading the list, never a guess.
func TestFindToolsAlwaysReturnsACatalogue(t *testing.T) {
	available := map[string]bool{}
	for _, d := range toolSetDefs(ToolSetSpending) {
		available[d.Name] = true
	}

	for _, need := range []string{
		"",
		"asdfgh qwerty zxcvbn",
		"something about my money",
		"the thing that tells me if I can retire",
	} {
		out, _, err := grantTools(mustJSON(t, toolFinderQuery{Need: need}), available, 0)
		if err != nil {
			t.Fatalf("need %q: %v", need, err)
		}
		var res toolFinderResult
		if err := json.Unmarshal([]byte(out), &res); err != nil {
			t.Fatalf("need %q: result is not valid JSON: %v", need, err)
		}
		if len(res.Catalogue) == 0 {
			t.Errorf("need %q: empty catalogue — this is the dead end the escape hatch exists to remove", need)
		}
		if res.Note == "" {
			t.Errorf("need %q: no note; a result that only reports data leaves the next move to inference, "+
				"and the inference this model made unprompted was to give up", need)
		}
		for _, b := range res.Catalogue {
			if available[b.Name] {
				t.Errorf("need %q: catalogue offers %q, which the turn already has", need, b.Name)
			}
			if b.Purpose == "" {
				t.Errorf("need %q: catalogue entry %q has no purpose", need, b.Name)
			}
		}
	}
}

// The escape hatch must never offer itself. A find_tools that can load
// find_tools is a loop with nothing at the end of it.
func TestFindToolsNeverOffersItself(t *testing.T) {
	out, granted, err := grantTools(
		mustJSON(t, toolFinderQuery{Need: "find tools", Names: []string{toolFinderName}}),
		map[string]bool{}, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if contains(toolNames(granted), toolFinderName) {
		t.Error("find_tools granted itself")
	}
	var res toolFinderResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	for _, b := range res.Catalogue {
		if b.Name == toolFinderName {
			t.Error("find_tools lists itself in its own catalogue")
		}
	}
}

// The cap bounds the TURN, not the call — otherwise a model could escalate its
// way to all thirty-five definitions three at a time and rebuild, inside one
// turn, the retrieval problem the sets exist to prevent.
func TestGrantedToolsAreCappedAcrossTheTurn(t *testing.T) {
	available := map[string]bool{}
	granted := 0
	for i := 0; i < 10; i++ {
		_, added, err := grantTools(
			mustJSON(t, toolFinderQuery{Need: "spending income debt goal retirement balance merchant category transactions"}),
			available, granted,
		)
		if err != nil {
			t.Fatal(err)
		}
		for _, tool := range added {
			available[tool.Name] = true
		}
		granted += len(added)
	}
	if granted > maxGrantedTools {
		t.Errorf("granted %d tools across the turn, over the cap of %d", granted, maxGrantedTools)
	}
	if granted != maxGrantedTools {
		t.Errorf("granted %d of an available %d — the cap should be reachable, or a real cross-set question stalls short of it",
			granted, maxGrantedTools)
	}
}

// A turn with no room left must SAY so. Silently granting nothing reads to the
// model exactly like the tool not existing, which is the failure being fixed.
func TestExhaustedGrantBudgetExplainsItself(t *testing.T) {
	out, added, err := grantTools(
		mustJSON(t, toolFinderQuery{Names: []string{"monthly_trend"}}),
		map[string]bool{}, maxGrantedTools,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 0 {
		t.Fatalf("granted %v past the cap", toolNames(added))
	}
	if !strings.Contains(out, "No room left") {
		t.Errorf("an exhausted budget did not explain itself: %s", out)
	}
}

// Determinism, the same contract the classifier is held to: the same need must
// load the same tools in the same order, twice and forever.
func TestFindToolsIsDeterministic(t *testing.T) {
	need := "compare income against spending for every month in a range"
	first, _, err := grantTools(mustJSON(t, toolFinderQuery{Need: need}), map[string]bool{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 25; i++ {
		again, _, err := grantTools(mustJSON(t, toolFinderQuery{Need: need}), map[string]bool{}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if again != first {
			t.Fatalf("find_tools is not deterministic:\n first: %s\n again: %s", first, again)
		}
	}
}

// An explicitly named tool outranks the fuzzy score. A model that names a tool
// has already decided, and the search second-guessing an exact request is how a
// correct choice gets silently swapped for a plausible one.
func TestExplicitNamesWinOverTheSearch(t *testing.T) {
	_, granted, err := grantTools(
		mustJSON(t, toolFinderQuery{
			Need:  "spending by category for a month",
			Names: []string{"debt_payoff"},
		}),
		map[string]bool{}, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(granted) == 0 || granted[0].Name != "debt_payoff" {
		t.Errorf("explicitly named tool did not come first: %v", toolNames(granted))
	}
}

// A hallucinated tool name must come back as a CORRECTION, not a dead end.
// "unknown tool" alone reads, to the model, exactly like the capability not
// existing — the failure this whole mechanism exists to end.
func TestUnknownToolNameSuggestsRealOnes(t *testing.T) {
	s := &Server{}
	_, err := s.executeChatTool(context.Background(), auth.Identity{}, "monthly_breakdown", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected an error for an unknown tool")
	}
	msg := err.Error()
	if !strings.Contains(msg, toolFinderName) {
		t.Errorf("unknown-tool error does not point at %s: %s", toolFinderName, msg)
	}
	if !strings.Contains(msg, "monthly_trend") {
		t.Errorf("unknown-tool error does not name the obvious near match: %s", msg)
	}
}

// The escape hatch has to be in every set, or it closes exactly the doors it
// was added to open.
func TestFindToolsIsInEverySet(t *testing.T) {
	for _, set := range ToolSets() {
		found := false
		for _, d := range toolSetDefs(set) {
			if d.Name == toolFinderName {
				found = true
				if d.Description == "" || len(d.InputSchema) == 0 {
					t.Errorf("set %q: %s is present but underdefined", set, toolFinderName)
				}
			}
		}
		if !found {
			t.Errorf("set %q has no %s — a misroute into it is a dead end again", set, toolFinderName)
		}
	}
}

// The system prompt has to TELL the model the hatch exists. The observed
// failure was not a missing mechanism, it was a model that apologised and
// stopped; a tool it is never told to reach for is one it will not reach for.
func TestSystemPromptTeachesTheEscapeHatch(t *testing.T) {
	for _, want := range []string{
		toolFinderName,
		"SUBSET",
	} {
		if !strings.Contains(chatSystemPrompt, want) {
			t.Errorf("system prompt does not mention %q", want)
		}
	}
}

// THE END-TO-END PROOF: a granted definition actually reaches the model.
//
// Every test above checks the bookkeeping. This one checks the thing that
// matters — that runChat appends the granted tool to the list it sends on the
// NEXT round trip. Bookkeeping that never reaches the wire would pass all of
// them and fix nothing.
func TestRunChatSendsGrantedToolsOnTheNextRequest(t *testing.T) {
	var sentTools [][]string
	turn := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Stream bool `json:"stream"`
			Tools  []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}
		_ = json.Unmarshal(body, &req)
		// Force the non-streaming path so the fake stays a plain JSON endpoint.
		if req.Stream {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		names := make([]string, 0, len(req.Tools))
		for _, tool := range req.Tools {
			names = append(names, tool.Name)
		}
		sentTools = append(sentTools, names)

		turn++
		w.Header().Set("content-type", "application/json")
		if turn == 1 {
			// The model, missing the tool it needs, asks for it.
			_, _ = w.Write([]byte(`{"id":"m1","role":"assistant","stop_reason":"tool_use","content":[` +
				`{"type":"tool_use","id":"tu_1","name":"` + toolFinderName + `",` +
				`"input":{"names":["monthly_trend"]}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"m2","role":"assistant","stop_reason":"end_turn",` +
			`"content":[{"type":"text","text":"done"}]}`))
	}))
	defer srv.Close()

	s := &Server{AI: ai.New(config.AIConfig{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model"})}

	var announced []string
	answer, err := s.runChat(
		context.Background(), auth.Identity{}, ToolSetLikelihood,
		[]ai.Message{{Role: ai.RoleUser, Content: []ai.Block{ai.TextBlock("what are the odds, and what did each month leave over?")}}},
		nil, nil,
		func(names []string) { announced = append(announced, names...) },
	)
	if err != nil {
		t.Fatalf("runChat: %v", err)
	}
	if answer != "done" {
		t.Fatalf("answer = %q, want %q", answer, "done")
	}
	if len(sentTools) != 2 {
		t.Fatalf("expected 2 model round-trips, got %d", len(sentTools))
	}
	if contains(sentTools[0], "monthly_trend") {
		t.Fatal("the likelihood set already carried monthly_trend; this test no longer proves anything")
	}
	if !contains(sentTools[1], "monthly_trend") {
		t.Errorf("the granted tool never reached the model: second request carried %v", sentTools[1])
	}
	if !contains(announced, "monthly_trend") {
		t.Errorf("the escalation was not announced to the client; announced %v", announced)
	}
}

func toolNames(tools []ai.Tool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name)
	}
	return out
}

func contains(haystack []string, want string) bool {
	for _, h := range haystack {
		if h == want {
			return true
		}
	}
	return false
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
