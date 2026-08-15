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

// A model cut off at MaxTokens has written a PREFIX, and runChat used to return
// it as the answer.
//
// This is the "the advisor just died" bug, and the reason it was invisible is
// that nothing about a truncated completion looks like a failure: Content is
// populated, error is nil, HTTP is 200. The user asked how much liquid they
// needed to hold their emergency fund at all times, the model made its tool
// calls, started a long answer, hit 1024 output tokens and stopped — and the
// app served the fragment as a finished reply.
//
// chatMaxTokens was raised to 6000 so the common case now fits in one turn, but
// that is a budget, not a guarantee — a long answer can still run past it, and
// continuation is what covers the remainder without another round of the whole
// timeout chain having to grow.
func TestRunChatContinuesAfterTruncation(t *testing.T) {
	var bodies []string
	turn := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &req)
		// Force the non-streaming path so the fake stays a plain JSON endpoint.
		if req.Stream {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		bodies = append(bodies, string(body))

		turn++
		w.Header().Set("content-type", "application/json")
		if turn == 1 {
			// Cut off mid-sentence, exactly as the real one was.
			_, _ = w.Write([]byte(`{"id":"m1","role":"assistant","stop_reason":"max_tokens",` +
				`"content":[{"type":"text","text":"You need $12,400 liquid to hold the"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"m2","role":"assistant","stop_reason":"end_turn",` +
			`"content":[{"type":"text","text":" target at all times."}]}`))
	}))
	defer srv.Close()

	s := &Server{AI: ai.New(config.AIConfig{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model"})}

	var streamed strings.Builder
	answer, err := s.runChat(
		context.Background(), auth.Identity{}, ToolSetLikelihood, "",
		[]ai.Message{ai.UserText("how much liquid to hold the emergency fund at all times?")},
		func(s string) { streamed.WriteString(s) }, nil, nil,
	)
	if err != nil {
		t.Fatalf("runChat: %v", err)
	}

	// The whole answer, both halves, in order — not just the tail.
	const want = "You need $12,400 liquid to hold the target at all times."
	if answer != want {
		t.Errorf("answer = %q, want %q", answer, want)
	}
	// And what the user watched arrive matches what gets saved to the thread. A
	// continuation that streamed one half and persisted the other would be a
	// subtler version of the same bug.
	if streamed.String() != want {
		t.Errorf("streamed %q, want %q", streamed.String(), want)
	}
	if len(bodies) != 2 {
		t.Fatalf("expected 2 round-trips (the cut-off answer and its continuation), got %d", len(bodies))
	}
	// The second call must actually ask for the rest, and must carry the partial
	// answer back so the model knows where it stopped.
	if !strings.Contains(bodies[1], "cut off at the output limit") {
		t.Error("the continuation request did not carry the continue instruction")
	}
	if !strings.Contains(bodies[1], "liquid to hold the") {
		t.Error("the continuation request did not carry the partial answer back to the model")
	}
}

// A model that never stops being truncated must not have its work thrown away.
//
// The old exhaustion path returned a flat apology, which for a truncation loop
// would replace an answer the user had already watched stream in with "I wasn't
// able to work that out". Keeping the text and labelling it as unfinished is the
// only honest option: a reader who knows the answer stops early can use it, and
// one who thinks a half-answer is complete is exactly who this app must not
// produce.
func TestRunChatKeepsPartialAnswerWhenContinuationsRunOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &req)
		if req.Stream {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		// Never finishes.
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m","role":"assistant","stop_reason":"max_tokens",` +
			`"content":[{"type":"text","text":"chunk. "}]}`))
	}))
	defer srv.Close()

	s := &Server{AI: ai.New(config.AIConfig{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model"})}

	answer, err := s.runChat(
		context.Background(), auth.Identity{}, ToolSetLikelihood, "",
		[]ai.Message{ai.UserText("something very long")},
		nil, nil, nil,
	)
	if err != nil {
		t.Fatalf("runChat: %v", err)
	}

	if strings.Contains(answer, "try asking in a simpler way") {
		t.Error("a partially-written answer was replaced by the generic apology")
	}
	if !strings.Contains(answer, "chunk.") {
		t.Errorf("the model's actual text was dropped: %q", answer)
	}
	// And it must SAY it is unfinished.
	if !strings.Contains(answer, "cut off") {
		t.Errorf("a truncated answer was not labelled as one: %q", answer)
	}
	// Every iteration should have been spent trying to finish.
	if n := strings.Count(answer, "chunk."); n != maxToolIterations {
		t.Errorf("accumulated %d chunks, want %d (one per iteration)", n, maxToolIterations)
	}
}

// With nothing written at all, the generic apology is still the right answer —
// there is no partial work to preserve, and inventing reassurance would be
// worse. This pins that the change above did not swallow the empty case.
func TestRunChatStillApologisesWhenNothingWasWritten(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &req)
		if req.Stream {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		// Spins on tool calls forever, never writing prose.
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m","role":"assistant","stop_reason":"tool_use","content":[` +
			`{"type":"tool_use","id":"tu","name":"nonexistent_tool","input":{}}]}`))
	}))
	defer srv.Close()

	s := &Server{AI: ai.New(config.AIConfig{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model"})}

	answer, err := s.runChat(
		context.Background(), auth.Identity{}, ToolSetLikelihood, "",
		[]ai.Message{ai.UserText("spin")}, nil, nil, nil,
	)
	if err != nil {
		t.Fatalf("runChat: %v", err)
	}
	if !strings.Contains(answer, "try asking in a simpler way") {
		t.Errorf("answer = %q, want the generic apology when no text was written", answer)
	}
}

// A thinking-only turn must not be echoed back, because that is what produced
// the 422 in production.
//
// The endpoint rejected `messages[5].content: null` — an assistant message with
// no blocks, which is what a reasoning-only response becomes once thinking is
// stripped. The continuation path fed it back verbatim, so a model that spent
// its whole budget reasoning turned a blank answer into a failed request.
//
// There is nothing to continue FROM in that case, so the loop must stop rather
// than continue, and it must never put an empty assistant turn on the wire.
func TestRunChatDoesNotEchoAThinkingOnlyTurn(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &req)
		if req.Stream {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		bodies = append(bodies, string(body))
		// Spent the whole budget thinking: max_tokens, no text, no tool use.
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m","role":"assistant","stop_reason":"max_tokens",` +
			`"content":[{"type":"thinking","thinking":"reasoning at length"}]}`))
	}))
	defer srv.Close()

	s := &Server{AI: ai.New(config.AIConfig{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model"})}

	answer, err := s.runChat(
		context.Background(), auth.Identity{}, ToolSetLikelihood, "",
		[]ai.Message{ai.UserText("how much liquid to hold the emergency fund at all times?")},
		nil, nil, nil,
	)
	if err != nil {
		t.Fatalf("runChat returned an error instead of handling the empty turn: %v", err)
	}
	// One attempt, then a stop: continuing would only buy another turn of
	// thinking against the same budget.
	if len(bodies) != 1 {
		t.Fatalf("made %d round-trips, want 1 (nothing to continue from)", len(bodies))
	}
	// THE REGRESSION: never send an assistant message with null/empty content.
	for i, b := range bodies {
		if strings.Contains(b, `"content":null`) {
			t.Errorf("request %d carried content:null — this is the 422:\n%s", i, b)
		}
	}
	// The user is told the model ran out of room, not handed a blank reply.
	if strings.TrimSpace(answer) == "" {
		t.Error("returned an empty answer; the user must be told something happened")
	}
}
