package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// CategoryRef is the minimal category shape the parser needs: a display name and
// the real slug it must map a budget intent onto. The model is given the real
// list so it never invents a category.
type CategoryRef struct {
	Name string
	Slug string
}

// ParsedRule is the model's structured reading of a natural-language request. It
// is a proposal only — the caller re-validates every field (ValidateConfig,
// category resolution, decimal parsing) before it can be saved, so a malformed
// or hallucinated parse degrades to "unsupported" rather than persisting.
type ParsedRule struct {
	// Kind is "alert", "budget", or "unsupported".
	Kind string `json:"kind"`
	// AlertType and Config are set when Kind == "alert".
	AlertType string          `json:"alert_type"`
	Config    json.RawMessage `json:"config"`
	// Category (a name or slug) and Amount are set when Kind == "budget".
	Category string `json:"category"`
	Amount   string `json:"amount"`
	// Reason carries the "why unsupported" text, or a caveat the user should see
	// even for a supported-but-imperfect mapping.
	Reason string `json:"reason"`
}

const parseRuleSystemPrompt = `You translate a user's sentence into exactly one of these budgeting rules for a household finance app. You may ONLY use the listed types.

Alert types (kind "alert"), with their config fields:
- big_spend: {"threshold": "<decimal string>"} — flags any single purchase over the threshold, engine-wide, over a fixed 30-day look-back. It has NO category and NO time-window field.
- budget_threshold: {"percent": <integer 1-1000>} — warns when a category's spending reaches that percent of its monthly budget.
- unusual_merchant: {"recent_days": <integer>, "min_amount": "<decimal string>"} — flags a newly-appearing merchant's charge.
- low_leftover: {"floor": "<decimal string>"} — warns when money left this month drops below the floor.

Budget intent (kind "budget"): {"category": "<a category from the provided list>", "amount": "<decimal string>"} — a monthly spending budget for one category.

Rules:
- If the request needs a capability not listed (a per-category OR per-window spend limit, a per-merchant cap, a time-of-day rule, etc.), return kind "unsupported" with a short reason. Do NOT force it into a rule that doesn't match.
- If a request maps to the closest supported rule but loses detail (e.g. "over $200 on dining weekly" → big_spend, which ignores the category and the week), still return the rule, and put the lost detail in "reason" as a caveat.
- Amounts are decimal strings like "200.00". Never invent a category — choose from the provided list or return unsupported.
- Answer only by calling the propose_rule tool.`

var proposeRuleTool = Tool{
	Name:        "propose_rule",
	Description: "Return the single budgeting rule that best matches the user's sentence.",
	InputSchema: json.RawMessage(`{
		"type": "object",
		"properties": {
			"kind": {"type": "string", "enum": ["alert", "budget", "unsupported"]},
			"alert_type": {"type": "string", "enum": ["big_spend", "budget_threshold", "unusual_merchant", "low_leftover"], "description": "Set when kind is alert"},
			"config": {"type": "object", "description": "Set when kind is alert: the type-specific config fields"},
			"category": {"type": "string", "description": "Set when kind is budget: a category name or slug from the provided list"},
			"amount": {"type": "string", "description": "Set when kind is budget: monthly amount as a decimal string"},
			"reason": {"type": "string", "description": "Why unsupported, or a caveat for an imperfect mapping"}
		},
		"required": ["kind"]
	}`),
}

// ParseRule turns one sentence into a structured rule proposal. today is injected
// (e.g. "Monday, 2 January 2006") so relative phrases resolve; categories is the
// real household list so a budget intent maps to an existing slug. Returns
// ErrDisabled when no key is configured.
func (c *Client) ParseRule(ctx context.Context, text string, categories []CategoryRef, today string) (ParsedRule, error) {
	if !c.Enabled() {
		return ParsedRule{}, ErrDisabled
	}

	system := parseRuleSystemPrompt + "\n\nToday's date is " + today + "."
	if len(categories) > 0 {
		var b strings.Builder
		b.WriteString("\n\nThe household's categories (name → slug):\n")
		for _, cat := range categories {
			fmt.Fprintf(&b, "- %s → %s\n", cat.Name, cat.Slug)
		}
		system += b.String()
	}

	resp, err := c.Complete(ctx, Request{
		System:     system,
		Messages:   []Message{UserText(text)},
		Tools:      []Tool{proposeRuleTool},
		ToolChoice: map[string]string{"type": "tool", "name": proposeRuleTool.Name},
		MaxTokens:  512,
	})
	if err != nil {
		return ParsedRule{}, err
	}

	uses := resp.ToolUses()
	if len(uses) == 0 {
		return ParsedRule{}, fmt.Errorf("ai: model did not call propose_rule")
	}
	var parsed ParsedRule
	if err := json.Unmarshal(uses[0].Input, &parsed); err != nil {
		return ParsedRule{}, fmt.Errorf("ai: decode rule proposal: %w", err)
	}
	return parsed, nil
}

// ParsedGoal is the model's structured reading of a savings-goal sentence. It is
// a proposal only: the caller re-validates the amount (positive decimal) and the
// date (a real future date) before it can be saved.
type ParsedGoal struct {
	Name         string `json:"name"`
	TargetAmount string `json:"target_amount"`
	// TargetDate is YYYY-MM-DD, or empty for an open-ended goal.
	TargetDate string `json:"target_date"`
	Kind       string `json:"kind"`
}

const parseGoalSystemPrompt = `You extract a savings goal from a user's sentence for a household finance app.
Pull out:
- name: a short label for what they're saving for (e.g. "Trip to Japan", "Emergency fund").
- target_amount: the money target as a decimal string, e.g. "10000.00". Expand shorthand like "$10k" to "10000.00".
- target_date: the deadline as YYYY-MM-DD if one is given or implied ("by December" → the next December's last day). Leave empty for an open-ended goal.
- kind: always "savings" for now.
Answer only by calling the propose_goal tool. Do not invent a target the user did not state.`

var proposeGoalTool = Tool{
	Name:        "propose_goal",
	Description: "Return the savings goal described by the user.",
	InputSchema: json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {"type": "string", "description": "Short label for the goal"},
			"target_amount": {"type": "string", "description": "Money target as a decimal string, e.g. \"10000.00\""},
			"target_date": {"type": "string", "description": "Deadline as YYYY-MM-DD, or empty for open-ended"},
			"kind": {"type": "string", "enum": ["savings"]}
		},
		"required": ["name", "target_amount", "kind"]
	}`),
}

// ParseGoal turns one sentence into a savings-goal proposal. today is injected
// (e.g. "Monday, 2 January 2006") so "by December" resolves to a concrete date.
// Returns ErrDisabled when no key is configured.
func (c *Client) ParseGoal(ctx context.Context, text, today string) (ParsedGoal, error) {
	if !c.Enabled() {
		return ParsedGoal{}, ErrDisabled
	}

	system := parseGoalSystemPrompt + "\n\nToday's date is " + today + "."
	resp, err := c.Complete(ctx, Request{
		System:     system,
		Messages:   []Message{UserText(text)},
		Tools:      []Tool{proposeGoalTool},
		ToolChoice: map[string]string{"type": "tool", "name": proposeGoalTool.Name},
		MaxTokens:  512,
	})
	if err != nil {
		return ParsedGoal{}, err
	}

	uses := resp.ToolUses()
	if len(uses) == 0 {
		return ParsedGoal{}, fmt.Errorf("ai: model did not call propose_goal")
	}
	var parsed ParsedGoal
	if err := json.Unmarshal(uses[0].Input, &parsed); err != nil {
		return ParsedGoal{}, fmt.Errorf("ai: decode goal proposal: %w", err)
	}
	return parsed, nil
}
