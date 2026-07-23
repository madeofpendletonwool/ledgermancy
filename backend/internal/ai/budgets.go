package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// BudgetSuggestionInput is one category handed to the model for a target. The
// average is already finished (StringFixed(2)) in SQL — the model reads it, it
// never recomputes it. IsFixed lets the model lean toward the actual amount for
// a fixed cost rather than a rounded planning figure.
type BudgetSuggestionInput struct {
	Name           string
	Slug           string
	MonthlyAverage string
	IsFixed        bool
}

// BudgetSuggestion is the model's proposal for one category: a round target and
// a one-line reason. Both are advisory — the handler re-validates Target against
// a band + roundness guardrail before it can populate a response, and swaps in
// deterministic rounding on failure.
type BudgetSuggestion struct {
	Slug      string `json:"slug"`
	Target    string `json:"target"`
	Rationale string `json:"rationale"`
}

const suggestBudgetsSystemPrompt = `You suggest round, memorable monthly budget targets for a household budgeting app.
You are given each spending category's true average monthly spend, already computed — never recompute it, never invent a different figure.
For each category:
- Pick a round target at or slightly above the average that a person can hold in their head. Round to sensible steps by size: nearest $10 under $200, $25 under $500, $50 under $1000, $100 above.
- Never pick a target below the average.
- Write one short, warm sentence that cites the average and explains the target.
Return your answer only by calling the propose_budgets tool. Use the exact slug given for each category.`

// suggestBudgetsTool forces structured output: parsing is a JSON unmarshal of the
// tool input rather than scraping prose. The schema mirrors BudgetSuggestion.
var suggestBudgetsTool = Tool{
	Name:        "propose_budgets",
	Description: "Return a round monthly budget target and a one-line rationale for each category.",
	InputSchema: json.RawMessage(`{
		"type": "object",
		"properties": {
			"proposals": {
				"type": "array",
				"items": {
					"type": "object",
					"properties": {
						"slug": {"type": "string", "description": "The exact category slug given"},
						"target": {"type": "string", "description": "Round monthly target as a decimal string, e.g. \"500.00\""},
						"rationale": {"type": "string", "description": "One warm sentence citing the average"}
					},
					"required": ["slug", "target", "rationale"]
				}
			}
		},
		"required": ["proposals"]
	}`),
}

// SuggestBudgets asks the model for a round target and rationale per category,
// keyed back by slug. It returns ErrDisabled when no key is configured so the
// caller falls back to deterministic rounding, and an empty map (not an error)
// when the model proposes nothing usable. The handler is authoritative: it
// re-validates every target and always echoes the SQL average, so a bad figure
// here can never reach the write path.
func (c *Client) SuggestBudgets(ctx context.Context, inputs []BudgetSuggestionInput) (map[string]BudgetSuggestion, error) {
	if !c.Enabled() {
		return nil, ErrDisabled
	}
	if len(inputs) == 0 {
		return nil, nil
	}

	var lines strings.Builder
	lines.WriteString("Categories (name, slug, average monthly spend):\n")
	for _, in := range inputs {
		fixed := ""
		if in.IsFixed {
			fixed = " (fixed cost)"
		}
		fmt.Fprintf(&lines, "- %s [slug=%s]: $%s/mo%s\n", in.Name, in.Slug, in.MonthlyAverage, fixed)
	}

	resp, err := c.Complete(ctx, Request{
		System:     suggestBudgetsSystemPrompt,
		Messages:   []Message{UserText(lines.String())},
		Tools:      []Tool{suggestBudgetsTool},
		ToolChoice: map[string]string{"type": "tool", "name": suggestBudgetsTool.Name},
		MaxTokens:  1024,
	})
	if err != nil {
		return nil, err
	}

	uses := resp.ToolUses()
	if len(uses) == 0 {
		return nil, fmt.Errorf("ai: model did not call propose_budgets")
	}

	var parsed struct {
		Proposals []BudgetSuggestion `json:"proposals"`
	}
	if err := json.Unmarshal(uses[0].Input, &parsed); err != nil {
		return nil, fmt.Errorf("ai: decode budget proposals: %w", err)
	}

	out := make(map[string]BudgetSuggestion, len(parsed.Proposals))
	for _, p := range parsed.Proposals {
		if p.Slug == "" {
			continue
		}
		out[p.Slug] = p
	}
	return out, nil
}
