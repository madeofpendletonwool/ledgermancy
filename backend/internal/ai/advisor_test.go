package ai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/config"
)

// A truncated narration is REFUSED, so the caller renders the complete ranked
// list instead of prose that silently drops the options past the cut.
//
// This prompt spends a paragraph forbidding exactly that outcome — never
// reorder, never leave one out, "a published rule the app is accountable for" —
// and truncation breaks the rule while the claim stays on the page. In
// production this was firing on EVERY request: 700 tokens in, stop_reason
// max_tokens out, a paragraph naming the first few options served as though it
// named them all.
//
// Falling back is not a degraded mode here. narrateAdvice already renders the
// full ranked list when narration is unavailable — that path exists for the
// no-API-key case — and a complete correctly-ordered list beats a truncated
// paragraph every time.
func TestAdvisorNarrationRefusesTruncatedOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(Response{
			Role: RoleAssistant, StopReason: StopMaxTokens,
			Content: []Block{TextBlock("Option one puts $500 toward the card. Option two put")},
		})
	}))
	defer srv.Close()

	c := New(config.AIConfig{BaseURL: srv.URL, APIKey: "k", Model: "glm-4.6"})
	out, err := c.AdvisorNarration(context.Background(), AdvisorInput{
		Slack: "$500.00", Hurdle: "6%",
		Options: []AdvisorOption{{Rank: 1, Label: "Pay the card", Amount: "$500.00"}},
	})
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("err = %v, want ErrTruncated", err)
	}
	// And the prefix must not leak out beside the error: a caller that checks
	// the string before the error would ship the very fragment this rejects.
	if out != "" {
		t.Errorf("narration = %q, want empty so the caller falls back to the plain list", out)
	}
}
