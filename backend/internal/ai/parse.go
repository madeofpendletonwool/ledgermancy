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
- predicted_low_balance: {"floor": "<decimal string>", "days": <integer>} — looks FORWARD: warns when the cash balance is projected to fall below the floor within that many days, once known upcoming bills clear. Use this, not low_leftover, when the request is about running out of money before a bill or a payday.

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
			"alert_type": {"type": "string", "enum": ["big_spend", "budget_threshold", "unusual_merchant", "low_leftover", "predicted_low_balance"], "description": "Set when kind is alert"},
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

// ParsedGoal is the model's structured reading of a goal sentence. It is a
// proposal only: the caller re-validates the amount (positive decimal), the date
// (a real future date) and the named debt (one the household actually has)
// before it can be saved.
type ParsedGoal struct {
	Name string `json:"name"`
	// TargetAmount is empty for a payoff goal — the balance to eliminate is read
	// from the account, never from the sentence.
	TargetAmount string `json:"target_amount"`
	// TargetDate is YYYY-MM-DD, or empty for an open-ended goal.
	TargetDate string `json:"target_date"`
	// Kind is "savings", "debt_payoff", or "unsupported".
	Kind string `json:"kind"`
	// Debt is set when Kind == "debt_payoff": the label, copied verbatim from the
	// list the caller supplied, of the debt being paid off. The caller resolves
	// it back to an account id; the model never sees or emits an id.
	Debt string `json:"debt"`
	// Reason carries the "why unsupported" text.
	Reason string `json:"reason"`
}

const parseGoalSystemPrompt = `You extract one financial goal from a user's sentence for a household finance app. There are exactly two kinds.

kind "savings" — putting money aside toward a target ("save $10k for a trip by December"):
- name: a short label for what they're saving for (e.g. "Trip to Japan", "Emergency fund").
- target_amount: the money target as a decimal string, e.g. "10000.00". Expand shorthand like "$10k" to "10000.00".
- target_date: the deadline as YYYY-MM-DD if one is given or implied ("by December" → the next December's last day). Leave empty for an open-ended goal.

kind "debt_payoff" — clearing a debt the household already owes ("pay off my credit card by December", "get the car loan gone next year"):
- name: a short label, e.g. "Pay off the Chase card".
- debt: which debt, copied EXACTLY from the household's debts listed below.
- target_date: the deadline, same rules as above.
- target_amount: leave EMPTY. The balance to clear is read from the account, not from the sentence.

kind "unsupported" — with a short reason, when you cannot map the sentence:
- The user names a debt the household does not have (a card, loan or mortgage missing from the list below). Say which one you could not find. Do NOT substitute a different debt.
- The household has no debts at all and the sentence is about paying one off.
- The sentence is not about a goal.

Answer only by calling the propose_goal tool. Do not invent a target, a deadline, or a debt the user did not state.`

var proposeGoalTool = Tool{
	Name:        "propose_goal",
	Description: "Return the savings or debt-payoff goal described by the user.",
	InputSchema: json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {"type": "string", "description": "Short label for the goal"},
			"target_amount": {"type": "string", "description": "Money target as a decimal string, e.g. \"10000.00\". Empty for a debt_payoff goal"},
			"target_date": {"type": "string", "description": "Deadline as YYYY-MM-DD, or empty for open-ended"},
			"kind": {"type": "string", "enum": ["savings", "debt_payoff", "unsupported"]},
			"debt": {"type": "string", "description": "Set when kind is debt_payoff: the debt's label, copied exactly from the household's debts"},
			"reason": {"type": "string", "description": "Set when kind is unsupported: why this sentence could not be mapped"}
		},
		"required": ["name", "kind"]
	}`),
}

// ParseGoal turns one sentence into a goal proposal. today is injected (e.g.
// "Monday, 2 January 2006") so "by December" resolves to a concrete date. debts
// are the household's debt labels; a payoff sentence naming anything outside
// this list comes back unsupported rather than pointed at a debt the user does
// not have. Returns ErrDisabled when no key is configured.
func (c *Client) ParseGoal(ctx context.Context, text, today string, debts []string) (ParsedGoal, error) {
	if !c.Enabled() {
		return ParsedGoal{}, ErrDisabled
	}

	system := parseGoalSystemPrompt + "\n\nToday's date is " + today + "."
	if len(debts) == 0 {
		system += "\n\nThis household has no debt accounts linked, so no debt_payoff goal is possible."
	} else {
		system += "\n\nThe household's debts, to copy verbatim into \"debt\":\n- " + strings.Join(debts, "\n- ")
	}
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
