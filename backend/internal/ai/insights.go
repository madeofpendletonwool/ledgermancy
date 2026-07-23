package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// InsightFact is one deterministic figure backing an insight. Value is already
// finished (a decimal string, a date, a category name) — the model narrates it,
// it never recomputes it.
type InsightFact struct {
	Label string
	Value string
}

// InsightPhraseInput is a detected insight handed to the model for nicer
// wording. Kind and the template Title/Body give it context; Facts are the
// authoritative numbers it must quote and must not invent around.
type InsightPhraseInput struct {
	Kind  string
	Title string // template headline, as a fallback and for context
	Body  string // template narrative
	Facts []InsightFact
}

// InsightText is the model's rephrased headline and narrative. It maps straight
// to an insight's title/body.
type InsightText struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

const insightSystemPrompt = `You rephrase a single detected financial fact into a short, warm feed insight for a household budgeting app.
Use ONLY the numbers, dates, and names provided — never invent, add, average, or round figures; quote amounts exactly as given.
Return a JSON object with two fields: "title" (a short headline, no trailing punctuation) and "body" (one to three plain, friendly sentences).
No markdown, no bullet points, no headings, no extra keys. If you are unsure, restate the provided template faithfully.`

// PhraseInsight asks the model to reword one insight's template into warmer
// prose, quoting the supplied facts verbatim. It returns ErrDisabled when no
// key is configured so the caller falls back to the template text. Any other
// error (timeout, malformed JSON, blank fields) is returned too, and callers
// MUST treat every error as "keep the template" — the feed is fully populated
// with AI off.
func (c *Client) PhraseInsight(ctx context.Context, in InsightPhraseInput) (InsightText, error) {
	if !c.Enabled() {
		return InsightText{}, ErrDisabled
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Insight kind: %s\n", in.Kind)
	fmt.Fprintf(&b, "Template title: %s\n", in.Title)
	fmt.Fprintf(&b, "Template body: %s\n", in.Body)
	if len(in.Facts) > 0 {
		b.WriteString("Facts (quote exactly):\n")
		for _, f := range in.Facts {
			fmt.Fprintf(&b, "- %s: %s\n", f.Label, f.Value)
		}
	}

	resp, err := c.Complete(ctx, Request{
		System:    insightSystemPrompt,
		Messages:  []Message{UserText(b.String())},
		MaxTokens: 300,
	})
	if err != nil {
		return InsightText{}, err
	}

	// The model is asked for a bare JSON object, but tolerate it wrapping the
	// object in prose or a code fence by extracting the outermost braces.
	raw := strings.TrimSpace(resp.Text())
	if i, j := strings.Index(raw, "{"), strings.LastIndex(raw, "}"); i >= 0 && j > i {
		raw = raw[i : j+1]
	}
	var out InsightText
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return InsightText{}, fmt.Errorf("ai: decode insight text: %w", err)
	}
	out.Title = strings.TrimSpace(out.Title)
	out.Body = strings.TrimSpace(out.Body)
	if out.Title == "" || out.Body == "" {
		return InsightText{}, fmt.Errorf("ai: insight text missing title or body")
	}
	return out, nil
}
