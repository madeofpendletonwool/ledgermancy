package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/apex42group/ledgermancy/backend/internal/config"
)

func TestMonthlySummaryDisabled(t *testing.T) {
	c := New(config.AIConfig{Model: "glm-4.6"})
	if _, err := c.MonthlySummary(context.Background(), MonthlySummaryInput{Month: "June 2026"}); err != ErrDisabled {
		t.Fatalf("want ErrDisabled, got %v", err)
	}
}

// The prompt must carry the exact figures (so the model narrates real numbers)
// and the response text is returned trimmed.
func TestMonthlySummaryPromptAndResult(t *testing.T) {
	var userText string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req wireRequest
		_ = json.Unmarshal(raw, &req)
		if len(req.Messages) > 0 && len(req.Messages[0].Content) > 0 {
			userText = req.Messages[0].Content[0].Text
		}
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(Response{
			Role: RoleAssistant, StopReason: "end_turn",
			Content: []Block{TextBlock("  A calm month. Groceries led at $612.  ")},
		})
	}))
	defer srv.Close()

	c := New(config.AIConfig{BaseURL: srv.URL, APIKey: "k", Model: "glm-4.6"})
	out, err := c.MonthlySummary(context.Background(), MonthlySummaryInput{
		Month:         "June 2026",
		Income:        "5000.00",
		Spending:      "3200.00",
		Leftover:      "1800.00",
		TopCategories: []CategoryLine{{Name: "Groceries", Total: "612.00"}},
		PriorSpending: "2900.00",
	})
	if err != nil {
		t.Fatalf("MonthlySummary: %v", err)
	}
	if out != "A calm month. Groceries led at $612." {
		t.Errorf("result not trimmed/returned: %q", out)
	}
	for _, want := range []string{"June 2026", "5000.00", "3200.00", "Groceries", "612.00", "2900.00"} {
		if !strings.Contains(userText, want) {
			t.Errorf("prompt missing %q\nprompt was:\n%s", want, userText)
		}
	}
}
