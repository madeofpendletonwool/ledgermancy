package ai

import (
	"context"
	"fmt"
	"strings"
)

// CategoryLine is one category's spend for the summary prompt.
type CategoryLine struct {
	Name  string
	Total string // decimal string, already computed in SQL
}

// MonthlySummaryInput is the pre-computed month, straight from the reporting
// layer. Every figure is a decimal string — the model only narrates them, it
// never does the arithmetic, so the numbers it quotes are the real ones.
type MonthlySummaryInput struct {
	Month                 string // e.g. "June 2026"
	Income                string
	Spending              string
	Leftover              string
	FixedSpending         string
	DiscretionarySpending string
	TopCategories         []CategoryLine
	// PriorSpending is last month's spend for a one-line comparison; empty to
	// skip it.
	PriorSpending string
}

const summarySystemPrompt = `You write a short monthly money recap for a household budgeting app.
Use only the figures provided — do not invent numbers or categories.
Two or three sentences, plain and warm, not corporate. No bullet points, no headings.
Mention the biggest category or two and whether the month was ahead or behind. Quote amounts as given.`

// MonthlySummary asks the model for a plain-English recap of one month. It
// returns ErrDisabled when no key is configured, so the caller can present the
// feature as unavailable rather than broken.
func (c *Client) MonthlySummary(ctx context.Context, in MonthlySummaryInput) (string, error) {
	if !c.Enabled() {
		return "", ErrDisabled
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Month: %s\n", in.Month)
	fmt.Fprintf(&b, "Income: %s\n", in.Income)
	fmt.Fprintf(&b, "Spending: %s\n", in.Spending)
	fmt.Fprintf(&b, "Leftover (income minus spending): %s\n", in.Leftover)
	fmt.Fprintf(&b, "Fixed spending: %s\n", in.FixedSpending)
	fmt.Fprintf(&b, "Discretionary spending: %s\n", in.DiscretionarySpending)
	if in.PriorSpending != "" {
		fmt.Fprintf(&b, "Previous month spending (for comparison): %s\n", in.PriorSpending)
	}
	if len(in.TopCategories) > 0 {
		b.WriteString("Top categories:\n")
		for _, cat := range in.TopCategories {
			fmt.Fprintf(&b, "- %s: %s\n", cat.Name, cat.Total)
		}
	}

	resp, err := c.Complete(ctx, Request{
		System:    summarySystemPrompt,
		Messages:  []Message{UserText(b.String())},
		MaxTokens: 400,
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.Text()), nil
}
