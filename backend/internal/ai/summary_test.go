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

// An in-progress month must steer the model to the present tense and carry the
// as-of date and the richer breakdown; a completed month must steer it to the
// past tense. The tense instruction lives in the system prompt, the figures in
// the user message.
func TestMonthlySummaryTenseAndDetail(t *testing.T) {
	var system, userText string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req wireRequest
		_ = json.Unmarshal(raw, &req)
		system = req.System
		if len(req.Messages) > 0 && len(req.Messages[0].Content) > 0 {
			userText = req.Messages[0].Content[0].Text
		}
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(Response{
			Role: RoleAssistant, StopReason: "end_turn",
			Content: []Block{TextBlock("ok")},
		})
	}))
	defer srv.Close()

	c := New(config.AIConfig{BaseURL: srv.URL, APIKey: "k", Model: "glm-4.6"})

	// In-progress: present tense, as-of date, and the new detail sections.
	if _, err := c.MonthlySummary(context.Background(), MonthlySummaryInput{
		Month:          "July 2026",
		InProgress:     true,
		AsOf:           "July 23, 2026",
		Income:         "$5,000.00",
		Spending:       "$3,200.00",
		SavingsRate:    "36%",
		RecurringTotal: "$240.00",
		AboveBaseline:  []CategoryDelta{{Name: "Dining", ThisMonth: "$400.00", Typical: "$250.00", Over: "$150.00"}},
		TopMerchants:   []MerchantLine{{Name: "Costco", Total: "$180.00", Count: 2}},
		BiggestTransactions: []TxnLine{{Merchant: "Delta", Amount: "$620.00", Date: "Jul 12", Category: "Travel"}},
	}); err != nil {
		t.Fatalf("MonthlySummary in-progress: %v", err)
	}
	if !strings.Contains(system, "present") || strings.Contains(system, "past tense") {
		t.Errorf("in-progress system prompt should steer present tense, got:\n%s", system)
	}
	for _, want := range []string{"July 23, 2026", "$5,000.00", "36%", "$240.00", "Dining", "typical $250.00", "Costco", "Delta"} {
		if !strings.Contains(userText, want) {
			t.Errorf("in-progress prompt missing %q\nprompt was:\n%s", want, userText)
		}
	}

	// Completed: past tense.
	if _, err := c.MonthlySummary(context.Background(), MonthlySummaryInput{
		Month: "June 2026", InProgress: false, Income: "$5,000.00", Spending: "$3,200.00",
	}); err != nil {
		t.Fatalf("MonthlySummary completed: %v", err)
	}
	if !strings.Contains(system, "past tense") {
		t.Errorf("completed system prompt should steer past tense, got:\n%s", system)
	}
	if !strings.Contains(userText, "month complete") {
		t.Errorf("completed prompt should mark the month complete, got:\n%s", userText)
	}
}
