package ai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/config"
)

// A reasoning model's thinking blocks must SURVIVE parsing.
//
// The stream parser handled "text" and "tool_use" and silently dropped
// everything else, which was fine until the deployment moved to a model that
// emits "thinking" first. A turn that spent its whole output budget reasoning
// then parsed as a Response with no content and no error — the shape most
// easily mistaken for a successful empty answer, and the reason the advisor
// appeared to die rather than to fail.
func TestStreamKeepsThinkingBlocks(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":10}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let me work"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":" through this."}}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text"}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"You need $12,400."}}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2000}}`,
		"data: [DONE]",
	}, "\n\n")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	c := New(config.AIConfig{BaseURL: srv.URL, APIKey: "k", Model: "glm"})

	var streamed strings.Builder
	resp, err := c.CompleteStream(context.Background(), Request{
		Messages: []Message{UserText("how much liquid?")},
	}, func(s string) { streamed.WriteString(s) })
	if err != nil {
		t.Fatalf("CompleteStream: %v", err)
	}

	if len(resp.Content) != 2 {
		t.Fatalf("content blocks = %d, want 2 (thinking + text): %+v", len(resp.Content), resp.Content)
	}
	if resp.Content[0].Type != "thinking" || resp.Content[0].Thinking != "Let me work through this." {
		t.Errorf("thinking block = %+v", resp.Content[0])
	}
	// The answer is the answer; the scratch work is not part of it.
	if got := resp.Text(); got != "You need $12,400." {
		t.Errorf("Text() = %q — thinking must not leak into the answer", got)
	}
	// And it must never have been streamed to the user.
	if strings.Contains(streamed.String(), "Let me work") {
		t.Errorf("thinking was streamed to the user: %q", streamed.String())
	}
	if resp.OnlyThinking() {
		t.Error("OnlyThinking() true for a response that reached a text block")
	}
}

// The exact production failure: the model spends its whole budget thinking, so
// the response has no text and no tool use.
//
// It must be recognisable (OnlyThinking) rather than looking like an empty
// success, and AsMessage must not produce `content: null` — the endpoint rejects
// that with a 422 naming only the message index, which is what turned a blank
// answer into a failed request when the chat loop echoed the turn back.
func TestThinkingOnlyResponseIsRecognisedAndNeverMarshalsNull(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Still reasoning"}}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":6000}}`,
		"data: [DONE]",
	}, "\n\n")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	c := New(config.AIConfig{BaseURL: srv.URL, APIKey: "k", Model: "glm"})
	resp, err := c.CompleteStream(context.Background(), Request{
		Messages: []Message{UserText("hard question")},
	}, nil)
	if err != nil {
		t.Fatalf("CompleteStream: %v", err)
	}

	if !resp.Truncated() {
		t.Error("Truncated() false for a max_tokens response")
	}
	if !resp.OnlyThinking() {
		t.Fatalf("OnlyThinking() false for a thinking-only response: %+v", resp.Content)
	}
	if resp.Text() != "" {
		t.Errorf("Text() = %q, want empty", resp.Text())
	}

	// The 422: a nil Content marshals to `null`, which the endpoint rejects.
	msg := resp.AsMessage()
	if msg.Content == nil {
		t.Fatal("AsMessage().Content is nil — it will marshal to `null` and 422 the next request")
	}
	if len(msg.Content) != 0 {
		t.Errorf("AsMessage() carried %d blocks; thinking must not be echoed back", len(msg.Content))
	}
	body, err := (&Client{}).marshalWire(Request{Messages: []Message{msg}}, false)
	if err != nil {
		t.Fatalf("marshalWire: %v", err)
	}
	if strings.Contains(string(body), `"content":null`) {
		t.Errorf("marshalled to content:null — the exact 422 body:\n%s", body)
	}
}

// AsMessage strips thinking but keeps the tool_use blocks the loop depends on.
// Echoing thousands of tokens of scratch work back every iteration would bloat
// the prompt for no benefit; dropping the tool_use would break the loop.
func TestAsMessageKeepsToolUseAndDropsThinking(t *testing.T) {
	r := &Response{Content: []Block{
		{Type: "thinking", Thinking: "long scratch work"},
		{Type: "text", Text: "checking"},
		{Type: "tool_use", ID: "tu_1", Name: "safe_to_spend"},
	}}
	msg := r.AsMessage()
	if len(msg.Content) != 2 {
		t.Fatalf("content = %+v, want text + tool_use", msg.Content)
	}
	for _, b := range msg.Content {
		if b.Type == "thinking" {
			t.Error("thinking was echoed back to the endpoint")
		}
	}
	if msg.Content[1].Type != "tool_use" || msg.Content[1].ID != "tu_1" {
		t.Errorf("tool_use block did not survive: %+v", msg.Content[1])
	}
}
