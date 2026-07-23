package ai

import (
	"context"
	"fmt"
	"strings"
)

// ForecastMilestone is one horizon of a projection, already computed. Month is a
// label like "2031-07"; NetWorth is a decimal string.
type ForecastMilestone struct {
	Months   int
	Month    string
	NetWorth string
}

// ForecastLever is a discretionary saving the user could act on, already
// computed in SQL/decimal (e.g. this month's dining minus its 6-month average).
type ForecastLever struct {
	Label          string
	MonthlySavings string // decimal string
	Basis          string // how it was derived, quoted to the reader
}

// ForecastInput is the finished projection, straight from networth.Project and
// the reporting layer. Every figure is a decimal string or a month label — the
// model narrates them, it never compounds, picks a milestone, or computes a
// lever.
type ForecastInput struct {
	Milestones       []ForecastMilestone
	MonthlySurplus   string
	AnnualReturnRate string // fraction, e.g. "0.06"
	Levers           []ForecastLever
	// Basis is the "not a forecast" caveat, copied verbatim from the projection
	// response so the model can restate it rather than invent one.
	Basis string
}

const forecastSystemPrompt = `You narrate a personal net-worth projection for a household budgeting app.
Use only the figures provided — never invent a number, a date, or a market prediction.
These are illustrative projections, not predictions: never imply certainty about markets or the future.
Quote every amount and month exactly as given. If a figure is not provided, do not mention it.
Write two or three warm, plain sentences — no bullet points, no headings.
End by reminding the reader this is an estimate that assumes things carry on as they are.`

// ForecastNarration turns a finished projection into two or three sentences. It
// returns ErrDisabled when no key is configured, so the caller can omit the
// narrative and still render the numbers.
func (c *Client) ForecastNarration(ctx context.Context, in ForecastInput) (string, error) {
	if !c.Enabled() {
		return "", ErrDisabled
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Monthly amount saved (surplus): %s\n", in.MonthlySurplus)
	fmt.Fprintf(&b, "Assumed annual return on invested assets (fraction): %s\n", in.AnnualReturnRate)
	b.WriteString("Projected net worth at each horizon:\n")
	for _, m := range in.Milestones {
		years := m.Months / 12
		fmt.Fprintf(&b, "- in %d years (%s): %s\n", years, m.Month, m.NetWorth)
	}
	if len(in.Levers) > 0 {
		b.WriteString("Ways to free up more each month (already computed):\n")
		for _, l := range in.Levers {
			fmt.Fprintf(&b, "- trimming %s to %s: about %s per month\n", l.Label, l.Basis, l.MonthlySavings)
		}
	}
	fmt.Fprintf(&b, "Caveat to restate: %s\n", in.Basis)

	resp, err := c.Complete(ctx, Request{
		System:    forecastSystemPrompt,
		Messages:  []Message{UserText(b.String())},
		MaxTokens: 400,
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.Text()), nil
}
