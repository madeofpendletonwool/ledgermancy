package ai

import (
	"context"
	"fmt"
	"strings"
)

// AdvisorOption is one already-ranked, already-computed option. Every money
// figure arrives as a finished string.
type AdvisorOption struct {
	Rank   int
	Tier   int
	Label  string
	Amount string
	Value  string
	// ValueKind names the unit of Value ("interest_avoided", "match_captured",
	// "months_earlier", …) so the prompt can say what the figure IS without the
	// model having to infer it from context and guess wrong.
	ValueKind string
	Detail    string
	// Note carries a missing-input caveat ("APR unknown — add it to compare
	// this"). When set the model must repeat it rather than quietly ranking the
	// option as though nothing were missing.
	Note string
	// Tradeoff marks the two-sided tier: options the app deliberately declines
	// to order against each other.
	Tradeoff bool
	Unranked bool
}

// AdvisorInput is a finished advisor run. The model narrates it and does nothing
// else.
type AdvisorInput struct {
	Slack string
	// SlackBasis says which safe-to-spend figure this is, in words the model may
	// repeat.
	SlackBasis string
	Hurdle     string
	// HurdleBasis explains where the hurdle came from ("your assumed real
	// return").
	HurdleBasis string
	Options     []AdvisorOption
}

// advisorSystemPrompt EXTENDS the prohibition the chat handlers already enforce
// rather than inventing a second set of rules.
//
// The additions specific to this feature are the last three lines, and they
// exist because the failure mode here is not a wrong number — the numbers are
// finished before the model sees them — it is the model quietly turning a
// computed tradeoff into advice, or reordering a published ranking because a
// different order reads better in prose.
const advisorSystemPrompt = `You narrate a ranked list of options for a household's spare cash in a budgeting app.

Every figure below is already computed. Use only these figures — never invent, adjust, re-derive or recompute a number, a date, a rate or a percentage. Quote every amount exactly as given, including the currency symbol and both decimal places. If a figure is not provided, do not mention it.

Keep the options in EXACTLY the order given. Never reorder them, never promote one, never leave one out, and never add an option that is not in the list. The order is a published rule the app is accountable for, not a matter of taste.

Present these as computed tradeoffs, never as advice. Do not say "you should", "I recommend", "the best option is", or "the smartest move". Say what each option would DO.

The cash is described conditionally: "if you don't spend this". Never say the household "has" this money to spend or that it is "spare" or "available" — the same figure is what the budgets page calls free-to-spend, and this list is what it would do instead.

Where two options are marked as a tradeoff, present them side by side and state plainly that the app does not rank one above the other, because one return is guaranteed and the other is assumed.

Where an option carries a note about a missing input, repeat that note.

Write short, warm, plain prose — two or three sentences of introduction, then one sentence per option. No headings, no bullet points, no markdown.`

// AdvisorNarration turns a finished, ranked list into prose. It returns
// ErrDisabled when no key is configured, so the caller renders the options as a
// plain list instead — THE FEATURE MUST WORK WITH NO KEY CONFIGURED, like
// everything else in this app.
func (c *Client) AdvisorNarration(ctx context.Context, in AdvisorInput) (string, error) {
	if !c.Enabled() {
		return "", ErrDisabled
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Money left over in a typical month if it is not spent: %s (basis: %s)\n", in.Slack, in.SlackBasis)
	fmt.Fprintf(&b, "Rate above which a guaranteed return beats this household's assumed one: %s (%s)\n", in.Hurdle, in.HurdleBasis)
	b.WriteString("\nThe ranked options, in the order they must stay in:\n")

	for _, o := range in.Options {
		fmt.Fprintf(&b, "%d. %s — put in %s", o.Rank, o.Label, o.Amount)
		if o.Value != "" {
			fmt.Fprintf(&b, "; %s: %s", strings.ReplaceAll(o.ValueKind, "_", " "), o.Value)
		}
		b.WriteString("\n")
		fmt.Fprintf(&b, "   Already-written explanation to reword (do not change any figure in it): %s\n", o.Detail)
		if o.Tradeoff {
			b.WriteString("   This option is one side of a tradeoff the app deliberately does not rank.\n")
		}
		if o.Unranked {
			b.WriteString("   This option could not be ranked; it is listed last on purpose.\n")
		}
		if o.Note != "" {
			fmt.Fprintf(&b, "   Note to repeat: %s\n", o.Note)
		}
	}

	resp, err := c.Complete(ctx, Request{
		System:    advisorSystemPrompt,
		Messages:  []Message{UserText(b.String())},
		MaxTokens: 700,
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.Text()), nil
}
