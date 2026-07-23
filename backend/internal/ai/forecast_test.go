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

// A disabled client must report ErrDisabled from ForecastNarration, so the
// projection endpoint can omit the narrative and still return the numbers.
func TestForecastNarrationDisabled(t *testing.T) {
	c := New(config.AIConfig{BaseURL: "http://unreachable.invalid", Model: "glm-4.6"})
	if _, err := c.ForecastNarration(context.Background(), ForecastInput{}); err != ErrDisabled {
		t.Fatalf("want ErrDisabled, got %v", err)
	}
}

// The narrator must hand the model the finished figures verbatim (milestones,
// surplus, lever, caveat) and return the model's text — it never computes.
func TestForecastNarrationWireShape(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m","role":"assistant","stop_reason":"end_turn",
			"content":[{"type":"text","text":"You're on track. This is an estimate."}],
			"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	c := New(config.AIConfig{BaseURL: srv.URL, APIKey: "k", Model: "glm-4.6"})
	text, err := c.ForecastNarration(context.Background(), ForecastInput{
		MonthlySurplus:   "1450.00",
		AnnualReturnRate: "0.06",
		Milestones: []ForecastMilestone{
			{Months: 60, Month: "2031-07", NetWorth: "132900.00"},
		},
		Levers: []ForecastLever{
			{Label: "Dining", MonthlySavings: "180.00", Basis: "your 6-month average"},
		},
		Basis: "Not a market forecast.",
	})
	if err != nil {
		t.Fatalf("ForecastNarration: %v", err)
	}
	if text != "You're on track. This is an estimate." {
		t.Errorf("text = %q", text)
	}

	// Every finished figure must reach the model exactly as given.
	for _, want := range []string{"132900.00", "2031-07", "1450.00", "180.00", "Dining", "Not a market forecast."} {
		if !strings.Contains(body, want) {
			t.Errorf("prompt missing %q; body was:\n%s", want, body)
		}
	}
}
