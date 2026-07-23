package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// MerchantInput is one merchant the model is asked to categorise. SampleName is
// a representative raw transaction description; the PFC hints are Plaid's own
// guess, which is often close even when it did not map to one of our slugs.
type MerchantInput struct {
	MerchantKey  string
	MerchantName string
	SampleName   string
	PFCPrimary   string
	PFCDetailed  string
}

// CategoryOption is a category the model may choose, identified by slug.
type CategoryOption struct {
	Slug string
	Name string
}

// MerchantCategory is the model's verdict for one merchant. Slug is empty when
// the model could not confidently place the merchant; the caller then leaves it
// in the fallback category rather than guessing.
type MerchantCategory struct {
	MerchantKey string
	Slug        string
	Confidence  float64
}

// categoriseSystemPrompt is deliberately strict: the model must pick from the
// supplied slugs or abstain, and must answer as bare JSON so the reply parses
// without coaxing. "Never invent a slug" is load-bearing — an invented slug
// would fail the slug→id lookup and silently drop the answer.
const categoriseSystemPrompt = `You categorise bank and credit-card transactions for a household budgeting app.
You are given a list of allowed categories (each with a slug) and a list of merchants.
For each merchant, choose the single best-fitting category slug, or use null if none fits well.

Rules:
- Only ever use a slug from the allowed list. Never invent a slug.
- Prefer a specific category over a generic one.
- If you are unsure, return null rather than guessing.
- Respond with ONLY a JSON array, no prose and no markdown fences. Each element:
  {"merchant_key": "<the key given>", "slug": "<allowed slug or null>", "confidence": <0.0-1.0>}`

// CategoriseMerchants asks the model to place each merchant into one of the
// allowed categories. Results are keyed back to merchant_key; any answer whose
// slug is not in the allowed set is dropped, so a hallucinated category can
// never be applied. Returns an empty slice (not an error) when the model
// abstains on everything.
func (c *Client) CategoriseMerchants(ctx context.Context, merchants []MerchantInput, cats []CategoryOption) ([]MerchantCategory, error) {
	if !c.Enabled() {
		return nil, ErrDisabled
	}
	if len(merchants) == 0 || len(cats) == 0 {
		return nil, nil
	}

	allowed := make(map[string]struct{}, len(cats))
	var catLines strings.Builder
	for _, cat := range cats {
		allowed[cat.Slug] = struct{}{}
		fmt.Fprintf(&catLines, "- %s (%s)\n", cat.Slug, cat.Name)
	}

	var merchLines strings.Builder
	for _, m := range merchants {
		name := m.MerchantName
		if name == "" {
			name = m.SampleName
		}
		fmt.Fprintf(&merchLines, "- merchant_key=%q name=%q raw=%q plaid=%q/%q\n",
			m.MerchantKey, name, m.SampleName, m.PFCPrimary, m.PFCDetailed)
	}

	user := fmt.Sprintf("Allowed categories:\n%s\nMerchants to categorise:\n%s",
		catLines.String(), merchLines.String())

	resp, err := c.Complete(ctx, Request{
		System:   categoriseSystemPrompt,
		Messages: []Message{UserText(user)},
		// A verdict per merchant is small; give headroom for a large batch.
		MaxTokens: 2048,
	})
	if err != nil {
		return nil, err
	}

	parsed, err := parseMerchantVerdicts(resp.Text())
	if err != nil {
		return nil, fmt.Errorf("ai: parse categorisation: %w", err)
	}

	out := make([]MerchantCategory, 0, len(parsed))
	for _, v := range parsed {
		if v.Slug == "" {
			continue
		}
		if _, ok := allowed[v.Slug]; !ok {
			// A slug the model made up despite instructions. Drop it rather
			// than apply a category that does not exist.
			continue
		}
		out = append(out, MerchantCategory{
			MerchantKey: v.MerchantKey,
			Slug:        v.Slug,
			Confidence:  v.Confidence,
		})
	}
	return out, nil
}

// merchantVerdict is the raw shape the model returns. Slug is a pointer so a
// JSON null decodes cleanly to "abstain".
type merchantVerdict struct {
	MerchantKey string  `json:"merchant_key"`
	Slug        string  `json:"slug"`
	Confidence  float64 `json:"confidence"`
}

// parseMerchantVerdicts tolerates the common ways a model wraps JSON: leading
// prose, a ```json fence, or a bare array. It extracts the outermost array and
// decodes it.
func parseMerchantVerdicts(text string) ([]merchantVerdict, error) {
	start := strings.IndexByte(text, '[')
	end := strings.LastIndexByte(text, ']')
	if start < 0 || end < 0 || end < start {
		return nil, fmt.Errorf("no JSON array in response: %q", truncate(text, 200))
	}

	var verdicts []merchantVerdict
	if err := json.Unmarshal([]byte(text[start:end+1]), &verdicts); err != nil {
		return nil, err
	}
	return verdicts, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
