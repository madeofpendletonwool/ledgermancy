package ai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/apex42group/ledgermancy/backend/internal/config"
)

// A disabled client (blank key) must never make a request and must report the
// distinct ErrDisabled, so callers can tell "AI off" from "AI failed".
func TestDisabledClient(t *testing.T) {
	c := New(config.AIConfig{BaseURL: "http://unreachable.invalid", Model: "glm-4.6"})
	if c.Enabled() {
		t.Fatal("client with blank key reported Enabled")
	}
	if _, err := c.Complete(context.Background(), Request{Messages: []Message{UserText("hi")}}); err != ErrDisabled {
		t.Fatalf("want ErrDisabled, got %v", err)
	}
}

// Complete must send the Anthropic Messages wire shape: correct path, auth and
// version headers, the configured model, and any tools serialised as the API
// expects. The response is decoded into text and tool_use blocks.
func TestCompleteWireShape(t *testing.T) {
	var gotBody wireRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %q, want /v1/messages", r.URL.Path)
		}
		if got := r.Header.Get("x-api-key"); got != "secret-key" {
			t.Errorf("x-api-key = %q", got)
		}
		if got := r.Header.Get("anthropic-version"); got != anthropicVersion {
			t.Errorf("anthropic-version = %q", got)
		}
		if got := r.Header.Get("content-type"); got != "application/json" {
			t.Errorf("content-type = %q", got)
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "msg_1",
			"role": "assistant",
			"stop_reason": "tool_use",
			"content": [
				{"type": "text", "text": "let me check"},
				{"type": "tool_use", "id": "tu_1", "name": "spend_by_category", "input": {"month": "2026-06"}}
			],
			"usage": {"input_tokens": 10, "output_tokens": 5}
		}`))
	}))
	defer srv.Close()

	c := New(config.AIConfig{BaseURL: srv.URL, APIKey: "secret-key", Model: "glm-4.6"})
	if !c.Enabled() {
		t.Fatal("client with key reported not Enabled")
	}

	resp, err := c.Complete(context.Background(), Request{
		System:   "you are a ledger",
		Messages: []Message{UserText("how much did I spend?")},
		Tools: []Tool{{
			Name:        "spend_by_category",
			Description: "spend for a month",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"month":{"type":"string"}}}`),
		}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if gotBody.Model != "glm-4.6" {
		t.Errorf("model = %q, want glm-4.6", gotBody.Model)
	}
	if gotBody.MaxTokens != defaultMaxTokens {
		t.Errorf("max_tokens = %d, want default %d", gotBody.MaxTokens, defaultMaxTokens)
	}
	if gotBody.System != "you are a ledger" {
		t.Errorf("system = %q", gotBody.System)
	}
	if len(gotBody.Tools) != 1 || gotBody.Tools[0].Name != "spend_by_category" {
		t.Errorf("tools not serialised: %+v", gotBody.Tools)
	}

	if resp.Text() != "let me check" {
		t.Errorf("Text() = %q", resp.Text())
	}
	uses := resp.ToolUses()
	if len(uses) != 1 || uses[0].Name != "spend_by_category" {
		t.Fatalf("ToolUses() = %+v", uses)
	}
	if resp.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q", resp.StopReason)
	}
}

// A non-2xx response must surface as an APIError carrying the body, not a
// generic decode failure.
func TestCompleteAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate_limited"}`))
	}))
	defer srv.Close()

	c := New(config.AIConfig{BaseURL: srv.URL, APIKey: "k", Model: "m"})
	_, err := c.Complete(context.Background(), Request{Messages: []Message{UserText("hi")}})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d", apiErr.StatusCode)
	}
}
