package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/ai"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/config"
)

// validateChatModel is the front door on the Advisor chat's model selection:
// the primary and the operator's AI_ADDITIONAL_MODELS entries pass, and nothing
// else does — a stray id must be turned away HERE rather than at the provider,
// where it would otherwise be a billable request nobody configured.
func TestValidateChatModel(t *testing.T) {
	s := &Server{Config: config.Config{AI: config.AIConfig{
		Model:            "glm-4.6",
		AdditionalModels: []string{"claude-sonnet-4-5", "glm-4.5-air"},
	}}}

	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		// Absent means the primary — returned as empty, the client's own
		// "configured model" default, so the request needs no special case.
		{in: "", want: ""},
		{in: "   ", want: ""},
		// The primary named explicitly is the same turn as not naming it.
		{in: "glm-4.6", want: ""},
		// An operator-listed alternate is passed through verbatim, and
		// surrounding whitespace is tolerated rather than treated as a
		// different id — model ids do not carry meaningful spaces.
		{in: "claude-sonnet-4-5", want: "claude-sonnet-4-5"},
		{in: "  glm-4.5-air  ", want: "glm-4.5-air"},
		// A typo, a casing difference, a model the operator never listed —
		// all refused, all without leaving the process.
		{in: "gpt-9", wantErr: true},
		{in: "GLM-4.6", wantErr: true},
	}
	for _, tc := range cases {
		got, err := s.validateChatModel(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("validateChatModel(%q) = %q, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("validateChatModel(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("validateChatModel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A selected model must reach the WIRE — the whole feature is one plumb from
// the request body to the endpoint's model field, and a turn that validated
// the id and then silently ran on the primary would look identical to a user
// comparing two models' answers.
func TestRunChatSendsSelectedModel(t *testing.T) {
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Stream bool   `json:"stream"`
			Model  string `json:"model"`
		}
		_ = json.Unmarshal(body, &req)
		// Force the non-streaming path so the fake stays a plain JSON endpoint.
		if req.Stream {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		gotModel = req.Model
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m1","role":"assistant","stop_reason":"end_turn",` +
			`"content":[{"type":"text","text":"done"}]}`))
	}))
	defer srv.Close()

	s := &Server{AI: ai.New(config.AIConfig{
		BaseURL: srv.URL, APIKey: "test-key", Model: "primary-model",
		AdditionalModels: []string{"alternate-model"},
	})}

	if _, err := s.runChat(
		context.Background(), auth.Identity{}, ToolSetLikelihood, "alternate-model",
		[]ai.Message{ai.UserText("a question")},
		func(string) {}, nil, nil,
	); err != nil {
		t.Fatalf("runChat: %v", err)
	}
	if gotModel != "alternate-model" {
		t.Errorf("endpoint saw model %q, want %q", gotModel, "alternate-model")
	}

	// And the no-selection case: the turn runs on the configured primary, not
	// on an empty model id the endpoint would reject.
	if _, err := s.runChat(
		context.Background(), auth.Identity{}, ToolSetLikelihood, "",
		[]ai.Message{ai.UserText("another question")},
		func(string) {}, nil, nil,
	); err != nil {
		t.Fatalf("runChat: %v", err)
	}
	if gotModel != "primary-model" {
		t.Errorf("endpoint saw model %q, want the configured primary %q", gotModel, "primary-model")
	}
}
