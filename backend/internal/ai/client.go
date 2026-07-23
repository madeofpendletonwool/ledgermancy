// Package ai is a small client for an Anthropic Messages API-compatible
// endpoint. Ledgermancy does not run Claude itself — the base URL is
// configurable so the same wire format reaches GLM, a self-hosted proxy, or
// Anthropic directly. Nothing here imports a vendor SDK, because a vendor SDK
// would assume api.anthropic.com and defeat the point.
//
// Every AI feature is optional: when no API key is configured the client is
// still constructed (so callers never branch on config), but Enabled() reports
// false and Complete returns ErrDisabled. Callers gate on Enabled() and skip
// the feature, leaving the app's deterministic behaviour untouched.
package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/apex42group/ledgermancy/backend/internal/config"
)

// anthropicVersion is the Messages API version header. GLM and other
// compatible endpoints accept the same value.
const anthropicVersion = "2023-06-01"

// defaultMaxTokens bounds a single completion. Categorisation and summaries are
// short; the chatbot sets its own higher limit per request when it needs one.
const defaultMaxTokens = 1024

// ErrDisabled is returned by Complete when no API key is configured. It lets a
// caller that forgot to check Enabled() fail loudly rather than silently.
var ErrDisabled = errors.New("ai: no API key configured")

// Client talks to an Anthropic Messages wire-compatible endpoint.
type Client struct {
	http    *http.Client
	baseURL string
	apiKey  string
	model   string
}

// New builds a client from AIConfig. It never fails and never returns nil: a
// blank API key yields a disabled client, so the rest of the app can hold a
// client unconditionally and gate features on Enabled().
func New(cfg config.AIConfig) *Client {
	return &Client{
		// A generous timeout: a chatbot turn that fans out to several tools can
		// take a while, and cutting it off mid-answer is worse than waiting.
		http:    &http.Client{Timeout: 120 * time.Second},
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:  cfg.APIKey,
		model:   cfg.Model,
	}
}

// Enabled reports whether AI calls will be attempted.
func (c *Client) Enabled() bool { return c != nil && c.apiKey != "" }

// Model is the configured model id, exposed for logging and diagnostics.
func (c *Client) Model() string { return c.model }

// Role identifies who authored a message.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is one turn in the conversation. Content is always a list of blocks
// even for plain text, because tool use and tool results share the same shape.
type Message struct {
	Role    Role    `json:"role"`
	Content []Block `json:"content"`
}

// Block is a single content block. Only the fields relevant to Type are set;
// omitempty keeps the wire payload to exactly what each block kind requires.
//
// Kinds used here:
//   - "text":        Text
//   - "tool_use":    ID, Name, Input   (from the model)
//   - "tool_result": ToolUseID, Content, IsError   (from us, answering a call)
type Block struct {
	Type string `json:"type"`

	// text
	Text string `json:"text,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

// TextBlock builds a plain text content block.
func TextBlock(text string) Block { return Block{Type: "text", Text: text} }

// ToolResultBlock answers a tool_use with the query result (or an error string
// so the model can recover rather than the whole turn failing).
func ToolResultBlock(toolUseID, content string, isError bool) Block {
	return Block{Type: "tool_result", ToolUseID: toolUseID, Content: content, IsError: isError}
}

// UserText is the common case: a user message containing one text block.
func UserText(text string) Message {
	return Message{Role: RoleUser, Content: []Block{TextBlock(text)}}
}

// Tool describes a callable the model may invoke. InputSchema is a raw JSON
// Schema object so callers can author it inline without a wrapper type.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// Request is one call to the Messages API.
type Request struct {
	System   string
	Messages []Message
	Tools    []Tool
	// ToolChoice is optional (e.g. {"type":"auto"} or {"type":"tool","name":..}).
	// Left nil, the endpoint decides.
	ToolChoice any
	// MaxTokens overrides defaultMaxTokens when non-zero.
	MaxTokens int
}

// Usage reports token counts for a completion, for cost logging.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Response is a Messages API completion.
type Response struct {
	ID         string  `json:"id"`
	Role       Role    `json:"role"`
	Content    []Block `json:"content"`
	StopReason string  `json:"stop_reason"`
	Usage      Usage   `json:"usage"`
}

// Text concatenates every text block, ignoring tool_use blocks. This is the
// answer for a plain completion.
func (r *Response) Text() string {
	var b strings.Builder
	for _, block := range r.Content {
		if block.Type == "text" {
			b.WriteString(block.Text)
		}
	}
	return b.String()
}

// ToolUses returns the tool_use blocks the model emitted, if any.
func (r *Response) ToolUses() []Block {
	var out []Block
	for _, block := range r.Content {
		if block.Type == "tool_use" {
			out = append(out, block)
		}
	}
	return out
}

// AsMessage turns the assistant response into a message ready to append to the
// conversation before feeding back tool results — the tool loop needs the
// assistant's tool_use blocks echoed back verbatim.
func (r *Response) AsMessage() Message {
	return Message{Role: RoleAssistant, Content: r.Content}
}

// APIError is a non-2xx response from the endpoint.
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("ai: endpoint returned %d: %s", e.StatusCode, e.Body)
}

// wireRequest is the exact JSON the Messages API expects. It is kept separate
// from Request so the caller-facing type can carry conveniences (a zero
// MaxTokens meaning "default") without leaking them onto the wire.
type wireRequest struct {
	Model      string    `json:"model"`
	MaxTokens  int       `json:"max_tokens"`
	System     string    `json:"system,omitempty"`
	Messages   []Message `json:"messages"`
	Tools      []Tool    `json:"tools,omitempty"`
	ToolChoice any       `json:"tool_choice,omitempty"`
	Stream     bool      `json:"stream,omitempty"`
}

// marshalWire builds the exact JSON body for one call. stream toggles SSE.
func (c *Client) marshalWire(req Request, stream bool) ([]byte, error) {
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	body, err := json.Marshal(wireRequest{
		Model:      c.model,
		MaxTokens:  maxTokens,
		System:     req.System,
		Messages:   req.Messages,
		Tools:      req.Tools,
		ToolChoice: req.ToolChoice,
		Stream:     stream,
	})
	if err != nil {
		return nil, fmt.Errorf("ai: marshal request: %w", err)
	}
	return body, nil
}

// newMessagesRequest builds the HTTP request with the shared auth headers.
func (c *Client) newMessagesRequest(ctx context.Context, body []byte) (*http.Request, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ai: build request: %w", err)
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)
	return httpReq, nil
}

// Complete performs one Messages API call. It returns ErrDisabled when no key
// is configured so the caller can treat "AI off" and "AI failed" distinctly.
func (c *Client) Complete(ctx context.Context, req Request) (*Response, error) {
	if !c.Enabled() {
		return nil, ErrDisabled
	}

	body, err := c.marshalWire(req, false)
	if err != nil {
		return nil, err
	}
	httpReq, err := c.newMessagesRequest(ctx, body)
	if err != nil {
		return nil, err
	}

	start := time.Now()
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("ai: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ai: read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &APIError{StatusCode: resp.StatusCode, Body: string(respBody)}
	}

	var out Response
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("ai: decode response: %w", err)
	}

	slog.Info("ai completion",
		"model", c.model,
		"stop_reason", out.StopReason,
		"input_tokens", out.Usage.InputTokens,
		"output_tokens", out.Usage.OutputTokens,
		"duration_ms", time.Since(start).Milliseconds(),
	)
	return &out, nil
}

// streamEvent is one Messages API SSE event. The `data:` payload carries its
// own `type`, so the `event:` line is ignored and this is switched on Type.
type streamEvent struct {
	Type         string `json:"type"`
	Index        int    `json:"index"`
	ContentBlock *struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage   *Usage `json:"usage"`
	Message *struct {
		Usage Usage `json:"usage"`
	} `json:"message"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// CompleteStream performs a streaming Messages API call. Assistant text is
// forwarded to onText as it arrives; the fully assembled Response is returned
// once the stream ends. That Response is identical in shape to Complete's — the
// same text and tool_use blocks — so the tool-calling loop can treat streaming
// and non-streaming calls the same. onText sees only user-visible text; tool_use
// blocks are reassembled silently and surface in the returned Content.
func (c *Client) CompleteStream(ctx context.Context, req Request, onText func(string)) (*Response, error) {
	if !c.Enabled() {
		return nil, ErrDisabled
	}

	body, err := c.marshalWire(req, true)
	if err != nil {
		return nil, err
	}
	httpReq, err := c.newMessagesRequest(ctx, body)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("accept", "text/event-stream")

	start := time.Now()
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("ai: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return nil, &APIError{StatusCode: resp.StatusCode, Body: string(b)}
	}

	// One accumulator per content block, keyed by the stream's block index.
	// Text builds up as text_delta arrives; tool_use input builds up from
	// input_json_delta fragments.
	type blockAcc struct {
		typ   string
		id    string
		name  string
		text  strings.Builder
		input strings.Builder
	}
	var blocks []*blockAcc
	ensure := func(i int) *blockAcc {
		for len(blocks) <= i {
			blocks = append(blocks, &blockAcc{})
		}
		return blocks[i]
	}

	out := Response{Role: RoleAssistant}

	scanner := bufio.NewScanner(resp.Body)
	// SSE lines can be large (a whole content block's JSON); raise the cap well
	// above the default 64KB so a big tool input is never truncated.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}

		var ev streamEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			// A frame we cannot parse is skipped rather than failing the turn.
			continue
		}

		switch ev.Type {
		case "message_start":
			if ev.Message != nil {
				out.Usage.InputTokens = ev.Message.Usage.InputTokens
			}
		case "content_block_start":
			b := ensure(ev.Index)
			if ev.ContentBlock != nil {
				b.typ = ev.ContentBlock.Type
				b.id = ev.ContentBlock.ID
				b.name = ev.ContentBlock.Name
			}
		case "content_block_delta":
			b := ensure(ev.Index)
			if ev.Delta != nil {
				switch ev.Delta.Type {
				case "text_delta":
					b.text.WriteString(ev.Delta.Text)
					if onText != nil && ev.Delta.Text != "" {
						onText(ev.Delta.Text)
					}
				case "input_json_delta":
					b.input.WriteString(ev.Delta.PartialJSON)
				}
			}
		case "message_delta":
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				out.StopReason = ev.Delta.StopReason
			}
			if ev.Usage != nil {
				out.Usage.OutputTokens = ev.Usage.OutputTokens
			}
		case "error":
			if ev.Error != nil {
				return nil, &APIError{StatusCode: resp.StatusCode, Body: ev.Error.Message}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("ai: read stream: %w", err)
	}

	for _, b := range blocks {
		switch b.typ {
		case "text":
			out.Content = append(out.Content, Block{Type: "text", Text: b.text.String()})
		case "tool_use":
			raw := b.input.String()
			if strings.TrimSpace(raw) == "" {
				// A tool call with no arguments still needs valid JSON input.
				raw = "{}"
			}
			out.Content = append(out.Content, Block{
				Type: "tool_use", ID: b.id, Name: b.name, Input: json.RawMessage(raw),
			})
		}
	}

	slog.Info("ai stream",
		"model", c.model,
		"stop_reason", out.StopReason,
		"input_tokens", out.Usage.InputTokens,
		"output_tokens", out.Usage.OutputTokens,
		"duration_ms", time.Since(start).Milliseconds(),
	)
	return &out, nil
}
