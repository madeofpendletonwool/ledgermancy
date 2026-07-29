package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// MerchantDescriptor is one raw bank descriptor offered to the model for
// grouping, with just enough context to tell a chain apart from a coincidence.
// Amounts are pre-formatted strings: the model is never asked to do arithmetic.
type MerchantDescriptor struct {
	// Key is the normalised merchant key. It is what the model must echo back —
	// grouping by an invented string is unusable, so the caller discards any key
	// it did not send.
	Key string
	// Sample is the most recent raw descriptor, which often carries the noise the
	// key stripped and is the strongest hint about what the business is.
	Sample string
	// Count is how many transactions carry this key, and Total their sum,
	// already formatted.
	Count int64
	Total string
}

// MerchantGroup is one proposed canonical merchant.
type MerchantGroup struct {
	// Name is the model's suggested display name ("Amazon"). Language is exactly
	// what a model should be doing here.
	Name string `json:"name"`
	// Keys are the descriptor keys the model believes are one business. The
	// caller validates every one against what it sent.
	Keys []string `json:"keys"`
	// Confidence is the model's own 0–1 rating. It is recorded and shown; it is
	// never a threshold for merging without review.
	Confidence float64 `json:"confidence"`
	// Reason is a one-line justification shown to the user in the review queue.
	Reason string `json:"reason"`
}

const groupMerchantsSystemPrompt = `You group raw bank transaction descriptors that belong to the SAME business.

Banks fragment one merchant across many descriptors: different payment processors, abbreviations, billing subdomains, and regional entities. Your job is to spot those, and only those.

Rules:
- Only group descriptors you are confident name the same company. Two businesses in the same industry, or in the same shopping centre, or sharing a common word, are NOT the same business.
- A shared generic word is not evidence. "BLUE BOTTLE" and "BLUE RIDGE" are different businesses. "CITY MARKET" and "CITY OPTICAL" are different businesses.
- A group must contain at least two of the provided keys. Do not return groups of one.
- Every key you return MUST be copied exactly from the list you were given. Never invent, correct, or reformat a key.
- A key may appear in at most one group.
- Leave a descriptor out entirely if you are unsure. Omitting it costs nothing; a wrong grouping corrupts the user's spending history.
- name is the business's ordinary public name, properly capitalised ("Amazon", "Trader Joe's"), not the raw descriptor.
- reason is one short clause a non-technical person can check, e.g. "AMZN is Amazon's billing descriptor".

Answer only by calling the group_merchants tool. If nothing should be grouped, call it with an empty list.`

var groupMerchantsTool = Tool{
	Name:        "group_merchants",
	Description: "Return the sets of descriptors that name the same business.",
	InputSchema: json.RawMessage(`{
		"type": "object",
		"properties": {
			"groups": {
				"type": "array",
				"description": "One entry per canonical business found. Empty if none.",
				"items": {
					"type": "object",
					"properties": {
						"name": {"type": "string", "description": "The business's ordinary public name, properly capitalised"},
						"keys": {"type": "array", "items": {"type": "string"}, "description": "Two or more keys copied exactly from the provided list"},
						"confidence": {"type": "number", "description": "0-1 confidence that these are one business"},
						"reason": {"type": "string", "description": "One short clause justifying the grouping"}
					},
					"required": ["name", "keys", "confidence"]
				}
			}
		},
		"required": ["groups"]
	}`),
}

type groupMerchantsResult struct {
	Groups []MerchantGroup `json:"groups"`
}

// GroupMerchants asks the model which of these descriptors name the same
// business. It is the long-tail pass: the deterministic rules in the merchants
// package run first and this only ever sees what they could not decide.
//
// The result is a proposal. Every group it returns becomes a *suggested* alias
// that changes no report until a person confirms it, which is why a wrong answer
// here is recoverable and why the model is allowed to guess at all.
//
// Returns ErrDisabled when no key is configured.
func (c *Client) GroupMerchants(ctx context.Context, descriptors []MerchantDescriptor) ([]MerchantGroup, error) {
	if !c.Enabled() {
		return nil, ErrDisabled
	}
	if len(descriptors) < 2 {
		return nil, nil
	}

	var b strings.Builder
	b.WriteString("Descriptors (key — most recent raw text — how many charges — total spend):\n")
	for _, d := range descriptors {
		fmt.Fprintf(&b, "- %s — %q — %d charges — $%s\n", d.Key, d.Sample, d.Count, d.Total)
	}

	resp, err := c.Complete(ctx, Request{
		System:     groupMerchantsSystemPrompt,
		Messages:   []Message{UserText(b.String())},
		Tools:      []Tool{groupMerchantsTool},
		ToolChoice: map[string]string{"type": "tool", "name": groupMerchantsTool.Name},
		MaxTokens:  2048,
	})
	if err != nil {
		return nil, err
	}

	uses := resp.ToolUses()
	if len(uses) == 0 {
		return nil, fmt.Errorf("ai: model did not call group_merchants")
	}
	var out groupMerchantsResult
	if err := json.Unmarshal(uses[0].Input, &out); err != nil {
		return nil, fmt.Errorf("ai: decode merchant groups: %w", err)
	}
	return out.Groups, nil
}
