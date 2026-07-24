package ai

import (
	"context"
	"fmt"
	"strings"
)

// CategoryLine is one category's spend for the summary prompt. Total is a
// display-ready money string ("$612.00") — the reporting layer formats it, the
// model only quotes it.
type CategoryLine struct {
	Name  string
	Total string
}

// CategoryDelta is one category measured against its own trailing baseline — the
// "you're running hotter than usual here" signal. All three amounts are
// pre-formatted money strings; Over is ThisMonth − Typical (always positive by
// the time a delta reaches the prompt).
type CategoryDelta struct {
	Name      string
	ThisMonth string
	Typical   string
	Over      string
}

// MerchantLine is one merchant's total spend for the period.
type MerchantLine struct {
	Name  string
	Total string
	Count int64
}

// TxnLine is one standout individual purchase.
type TxnLine struct {
	Merchant string
	Amount   string
	Date     string // short, e.g. "Jul 12"
	Category string
}

// MonthlySummaryInput is the pre-computed month, straight from the reporting
// layer. Every figure is a display-ready string — the model only narrates them,
// it never does the arithmetic, so the numbers it quotes are the real ones.
type MonthlySummaryInput struct {
	Month string // e.g. "June 2026"

	// InProgress marks a month that has not finished yet: the recap is written in
	// the present/continuous tense ("so far this month…") rather than the past.
	// AsOf is the day the figures were taken, and is only meaningful (and only
	// rendered) when InProgress.
	InProgress bool
	AsOf       string

	Income                string
	Spending              string
	Leftover              string
	FixedSpending         string
	DiscretionarySpending string
	// SavingsRate is leftover as a share of income, e.g. "36%"; empty when income
	// was zero and there is nothing to divide by.
	SavingsRate string
	// RecurringTotal is the estimated monthly cost of the household's active
	// recurring charges; empty when none were detected.
	RecurringTotal string
	// TransactionCount is how many spending transactions the figures cover.
	TransactionCount int

	TopCategories []CategoryLine
	// AboveBaseline lists the categories running most above their trailing
	// average this period, biggest overage first — the real "what changed" signal.
	AboveBaseline []CategoryDelta
	// TopMerchants and BiggestTransactions give the model concrete specifics to
	// name instead of only category totals.
	TopMerchants        []MerchantLine
	BiggestTransactions []TxnLine

	// PriorSpending is last month's spend for a one-line comparison; empty to
	// skip it.
	PriorSpending string
}

// summarySystemPrompt is assembled per call because the tense instruction
// depends on whether the month is complete. Everything else is constant.
const summarySystemPromptBase = `You write a short monthly money recap for a household budgeting app.
Use only the figures provided — do not invent numbers, categories, or merchants. Quote amounts exactly as written; they already include the $ and thousands separators, so do not reformat or round them.
Write a warm, plain paragraph of three to five sentences — not corporate, no bullet points, no headings.
Lead with the headline (money in versus money out, and whether the household is ahead or behind), then work in the most useful specifics from the breakdown: the biggest categories, anything running notably above its usual level, a standout purchase or merchant, and the savings rate when it stands out. Prefer the "above usual" signals over a bare list of the largest categories — they are what actually changed.`

const summaryTenseCompleted = "\nThis month is complete: write in the past tense (\"in %s you spent…\")."

const summaryTenseInProgress = "\nThis month is still in progress as of %s: write in the present/continuous tense (\"so far this month you've spent…\"). Describe only what has happened so far — do not project, extrapolate, or guess where the month will end up."

// MonthlySummary asks the model for a plain-English recap of one month. It
// returns ErrDisabled when no key is configured, so the caller can present the
// feature as unavailable rather than broken.
func (c *Client) MonthlySummary(ctx context.Context, in MonthlySummaryInput) (string, error) {
	if !c.Enabled() {
		return "", ErrDisabled
	}

	system := summarySystemPromptBase
	if in.InProgress {
		system += fmt.Sprintf(summaryTenseInProgress, in.AsOf)
	} else {
		system += fmt.Sprintf(summaryTenseCompleted, in.Month)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Month: %s\n", in.Month)
	if in.InProgress && in.AsOf != "" {
		fmt.Fprintf(&b, "Status: in progress, figures as of %s\n", in.AsOf)
	} else {
		b.WriteString("Status: month complete\n")
	}
	fmt.Fprintf(&b, "Income: %s\n", in.Income)
	fmt.Fprintf(&b, "Spending: %s\n", in.Spending)
	fmt.Fprintf(&b, "Leftover (income minus spending): %s\n", in.Leftover)
	fmt.Fprintf(&b, "Fixed spending: %s\n", in.FixedSpending)
	fmt.Fprintf(&b, "Discretionary spending: %s\n", in.DiscretionarySpending)
	if in.SavingsRate != "" {
		fmt.Fprintf(&b, "Savings rate (leftover / income): %s\n", in.SavingsRate)
	}
	if in.RecurringTotal != "" {
		fmt.Fprintf(&b, "Estimated recurring/subscription cost per month: %s\n", in.RecurringTotal)
	}
	if in.TransactionCount > 0 {
		fmt.Fprintf(&b, "Spending transactions: %d\n", in.TransactionCount)
	}
	if in.PriorSpending != "" {
		fmt.Fprintf(&b, "Previous month spending (for comparison): %s\n", in.PriorSpending)
	}

	if len(in.TopCategories) > 0 {
		b.WriteString("Top spending categories:\n")
		for _, cat := range in.TopCategories {
			fmt.Fprintf(&b, "- %s: %s\n", cat.Name, cat.Total)
		}
	}

	if len(in.AboveBaseline) > 0 {
		b.WriteString("Categories running above their usual (trailing-average) level:\n")
		for _, d := range in.AboveBaseline {
			fmt.Fprintf(&b, "- %s: %s vs a typical %s (%s more than usual)\n",
				d.Name, d.ThisMonth, d.Typical, d.Over)
		}
	}

	if len(in.TopMerchants) > 0 {
		b.WriteString("Top merchants:\n")
		for _, m := range in.TopMerchants {
			fmt.Fprintf(&b, "- %s: %s across %d charges\n", m.Name, m.Total, m.Count)
		}
	}

	if len(in.BiggestTransactions) > 0 {
		b.WriteString("Biggest individual purchases:\n")
		for _, t := range in.BiggestTransactions {
			line := fmt.Sprintf("- %s: %s on %s", t.Merchant, t.Amount, t.Date)
			if t.Category != "" {
				line += fmt.Sprintf(" (%s)", t.Category)
			}
			b.WriteString(line + "\n")
		}
	}

	resp, err := c.Complete(ctx, Request{
		System:    system,
		Messages:  []Message{UserText(b.String())},
		MaxTokens: 500,
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.Text()), nil
}
