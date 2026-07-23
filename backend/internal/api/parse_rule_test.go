package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/apex42group/ledgermancy/backend/internal/ai"
	"github.com/apex42group/ledgermancy/backend/internal/alerts"
	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
)

// TestResolveRuleProposal drives the deterministic half of NL parsing: given a
// model parse (as if returned by ParseRule), the resolver must validate it,
// canonicalise money, and produce the right kind — with every alert config
// passing alerts.ValidateConfig, and a bad parse degrading to unsupported.
func TestResolveRuleProposal(t *testing.T) {
	dining := dbgen.Category{ID: uuid.New(), Name: "Dining", Slug: "dining"}
	income := dbgen.Category{ID: uuid.New(), Name: "Paycheck", Slug: "paycheck", IsIncome: true}
	cats := []dbgen.Category{dining, income}

	cfg := func(v any) json.RawMessage {
		b, _ := json.Marshal(v)
		return b
	}

	cases := []struct {
		name        string
		parsed      ai.ParsedRule
		wantKind    string
		wantSummary string // substring
	}{
		{
			name:        "big_spend",
			parsed:      ai.ParsedRule{Kind: "alert", AlertType: alerts.TypeBigSpend, Config: cfg(map[string]string{"threshold": "300"})},
			wantKind:    "alert",
			wantSummary: "over $300.00",
		},
		{
			name:        "budget_threshold",
			parsed:      ai.ParsedRule{Kind: "alert", AlertType: alerts.TypeBudgetThreshold, Config: cfg(map[string]int{"percent": 90})},
			wantKind:    "alert",
			wantSummary: "90%",
		},
		{
			name:        "unusual_merchant defaults recent_days",
			parsed:      ai.ParsedRule{Kind: "alert", AlertType: alerts.TypeUnusualMerchant, Config: cfg(map[string]any{"min_amount": "25"})},
			wantKind:    "alert",
			wantSummary: "last 7 days",
		},
		{
			name:        "low_leftover",
			parsed:      ai.ParsedRule{Kind: "alert", AlertType: alerts.TypeLowLeftover, Config: cfg(map[string]string{"floor": "500"})},
			wantKind:    "alert",
			wantSummary: "below $500.00",
		},
		{
			name:        "budget maps to slug",
			parsed:      ai.ParsedRule{Kind: "budget", Category: "Dining", Amount: "600"},
			wantKind:    "budget",
			wantSummary: "for Dining",
		},
		{
			name:     "budget with unknown category is unsupported",
			parsed:   ai.ParsedRule{Kind: "budget", Category: "Yacht", Amount: "600"},
			wantKind: "unsupported",
		},
		{
			name:     "budget onto income category is unsupported",
			parsed:   ai.ParsedRule{Kind: "budget", Category: "Paycheck", Amount: "600"},
			wantKind: "unsupported",
		},
		{
			name:     "invalid config is unsupported",
			parsed:   ai.ParsedRule{Kind: "alert", AlertType: alerts.TypeBigSpend, Config: cfg(map[string]string{"threshold": "0"})},
			wantKind: "unsupported",
		},
		{
			name:     "explicit unsupported",
			parsed:   ai.ParsedRule{Kind: "unsupported", Reason: "per-merchant weekly caps aren't supported"},
			wantKind: "unsupported",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resolveRuleProposal(c.parsed, cats)
			if got.Kind != c.wantKind {
				t.Fatalf("kind = %q, want %q (reason=%q)", got.Kind, c.wantKind, got.Reason)
			}
			if c.wantSummary != "" && !strings.Contains(got.Summary, c.wantSummary) {
				t.Errorf("summary = %q, want substring %q", got.Summary, c.wantSummary)
			}
			// Every returned alert config must pass the same validator the write
			// path uses — proving the confirmation UI never shows an unenforceable rule.
			if got.Kind == "alert" {
				if err := alerts.ValidateConfig(got.Alert.Type, got.Alert.Config); err != nil {
					t.Errorf("returned alert config fails ValidateConfig: %v", err)
				}
			}
			if got.Kind == "budget" && got.Budget.Amount != "600.00" {
				t.Errorf("budget amount = %q, want canonical 600.00", got.Budget.Amount)
			}
		})
	}
}
