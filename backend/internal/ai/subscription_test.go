package ai

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/apex42group/ledgermancy/backend/internal/config"
)

func TestClassifySubscriptionsDisabled(t *testing.T) {
	c := New(config.AIConfig{BaseURL: "http://unreachable.invalid", Model: "glm-4.6"})
	if _, err := c.ClassifySubscriptions(context.Background(), []SubscriptionInput{{MerchantKey: "a"}}); err != ErrDisabled {
		t.Fatalf("want ErrDisabled, got %v", err)
	}
}

// The classifier must key results by merchant_key, pass the name and cadence to
// the model, and drop any label outside the fixed set.
func TestClassifySubscriptionsWireShape(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
		w.Header().Set("content-type", "application/json")
		// netflix → valid label; gymzo → invalid label (must be dropped);
		// mystery → "other".
		_, _ = w.Write([]byte(`{"id":"m","role":"assistant","stop_reason":"end_turn",
			"content":[{"type":"text","text":"[{\"merchant_key\":\"netflix\",\"category\":\"streaming\"},{\"merchant_key\":\"gymzo\",\"category\":\"bogus\"},{\"merchant_key\":\"mystery\",\"category\":\"other\"}]"}],
			"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	c := New(config.AIConfig{BaseURL: srv.URL, APIKey: "k", Model: "glm-4.6"})
	labels, err := c.ClassifySubscriptions(context.Background(), []SubscriptionInput{
		{MerchantKey: "netflix", Merchant: "Netflix", Cadence: "monthly"},
		{MerchantKey: "gymzo", Merchant: "Gymzo", Cadence: "monthly"},
		{MerchantKey: "mystery", Merchant: "Mystery Co", Cadence: "monthly"},
	})
	if err != nil {
		t.Fatalf("ClassifySubscriptions: %v", err)
	}

	if labels["netflix"] != "streaming" {
		t.Errorf("netflix = %q, want streaming", labels["netflix"])
	}
	if labels["mystery"] != "other" {
		t.Errorf("mystery = %q, want other", labels["mystery"])
	}
	if _, ok := labels["gymzo"]; ok {
		t.Errorf("gymzo got label %q, want dropped (invalid)", labels["gymzo"])
	}

	// The model saw the name and cadence it needs to judge.
	for _, want := range []string{"Netflix", "monthly", "merchant_key"} {
		if !strings.Contains(body, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}
