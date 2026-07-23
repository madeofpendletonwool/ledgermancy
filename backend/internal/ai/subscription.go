package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// subscriptionCategories is the fixed label set the model must choose from.
// "other" is the catch-all; any answer outside this set is rejected, so a
// hallucinated label can never be applied.
var subscriptionCategories = []string{
	"streaming", "music", "utility", "insurance",
	"phone", "gym", "software", "news", "other",
}

// SubscriptionInput is one recurring merchant to label. The model sees only the
// name and cadence — never an amount — so it classifies, it never computes.
type SubscriptionInput struct {
	MerchantKey string
	Merchant    string
	Cadence     string
}

const classifySubscriptionsSystemPrompt = `You label recurring charges for a household budgeting app.
You are given a list of merchants that charge on a regular cadence.
For each, choose the single best category label from this fixed set:
streaming, music, utility, insurance, phone, gym, software, news, other.

Rules:
- Only ever use a label from that set. Never invent a label. Use "other" if none fit.
- Judge only from the merchant name and cadence — do not reason about amounts.
- Respond with ONLY a JSON array, no prose and no markdown fences. Each element:
  {"merchant_key": "<the key given>", "category": "<one of the allowed labels>"}`

// ClassifySubscriptions labels each recurring merchant with one category from the
// fixed set, keyed back by merchant_key. Any label outside the set is dropped.
// Returns ErrDisabled when no key is configured, and an empty map (not an error)
// when the model labels nothing usefully.
func (c *Client) ClassifySubscriptions(ctx context.Context, subs []SubscriptionInput) (map[string]string, error) {
	if !c.Enabled() {
		return nil, ErrDisabled
	}
	if len(subs) == 0 {
		return nil, nil
	}

	allowed := make(map[string]struct{}, len(subscriptionCategories))
	for _, l := range subscriptionCategories {
		allowed[l] = struct{}{}
	}

	var lines strings.Builder
	for _, s := range subs {
		fmt.Fprintf(&lines, "- merchant_key=%q name=%q cadence=%q\n", s.MerchantKey, s.Merchant, s.Cadence)
	}
	user := fmt.Sprintf("Merchants to label:\n%s", lines.String())

	resp, err := c.Complete(ctx, Request{
		System:    classifySubscriptionsSystemPrompt,
		Messages:  []Message{UserText(user)},
		MaxTokens: 1024,
	})
	if err != nil {
		return nil, err
	}

	parsed, err := parseSubscriptionVerdicts(resp.Text())
	if err != nil {
		return nil, fmt.Errorf("ai: parse subscription classification: %w", err)
	}

	out := make(map[string]string, len(parsed))
	for _, v := range parsed {
		if v.Category == "" {
			continue
		}
		if _, ok := allowed[v.Category]; !ok {
			continue // a label the model made up despite instructions — drop it
		}
		out[v.MerchantKey] = v.Category
	}
	return out, nil
}

type subscriptionVerdict struct {
	MerchantKey string `json:"merchant_key"`
	Category    string `json:"category"`
}

// parseSubscriptionVerdicts extracts the outermost JSON array, tolerating the
// prose/fence wrappers a model sometimes adds — same approach as the merchant
// categoriser.
func parseSubscriptionVerdicts(text string) ([]subscriptionVerdict, error) {
	start := strings.IndexByte(text, '[')
	end := strings.LastIndexByte(text, ']')
	if start < 0 || end < 0 || end < start {
		return nil, fmt.Errorf("no JSON array in response: %q", truncate(text, 200))
	}
	var verdicts []subscriptionVerdict
	if err := json.Unmarshal([]byte(text[start:end+1]), &verdicts); err != nil {
		return nil, err
	}
	return verdicts, nil
}
