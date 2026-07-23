package insights

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/apex42group/ledgermancy/backend/internal/ai"
	"github.com/apex42group/ledgermancy/backend/internal/config"
)

// TestSubscriptionClassify checks the producer maps AI labels back onto the
// right candidates by merchant_key, and leaves a candidate the model did not
// label with its empty category. No DB — Classify operates on candidates
// directly against a stub model.
func TestSubscriptionClassify(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		// Only streamy is labelled; gymzo is omitted by the model.
		_, _ = w.Write([]byte(`{"id":"m","role":"assistant","stop_reason":"end_turn",
			"content":[{"type":"text","text":"[{\"merchant_key\":\"streamy\",\"category\":\"streaming\"}]"}],
			"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	client := ai.New(config.AIConfig{BaseURL: srv.URL, APIKey: "k", Model: "glm-4.6"})

	candidates := []Candidate{
		{Kind: "subscription", Data: map[string]any{"merchant_key": "streamy", "merchant": "Streamy", "cadence": "monthly", "category": ""}},
		{Kind: "subscription", Data: map[string]any{"merchant_key": "gymzo", "merchant": "Gymzo", "cadence": "monthly", "category": ""}},
	}

	if err := (subscriptionProducer{}).Classify(context.Background(), client, candidates); err != nil {
		t.Fatalf("classify: %v", err)
	}

	if got := candidates[0].Data["category"]; got != "streaming" {
		t.Errorf("streamy category = %v, want streaming", got)
	}
	if got := candidates[1].Data["category"]; got != "" {
		t.Errorf("gymzo category = %v, want empty (model did not label it)", got)
	}
}
