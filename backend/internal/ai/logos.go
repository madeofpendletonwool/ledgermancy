package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// MerchantForDomain is one canonical merchant offered to the model for domain
// resolution.
type MerchantForDomain struct {
	// ID is a small integer the caller assigns for this call only, and the model
	// must echo it back. Deliberately not the merchant key: a resolved key is
	// often an entity UUID, and a model asked to copy 36 hex characters
	// verbatim will eventually get one wrong. An integer under a hundred is
	// something it cannot mangle unnoticed, and the caller holds the mapping.
	ID int
	// Name is the canonical display name — "Blue Bottle Coffee", not
	// "SQ *BLUE BOTTLE #4471". The raw descriptor is deliberately withheld:
	// descriptors carry the payment processor's domain, and a model shown one
	// answers with the processor rather than the shop.
	Name string
}

// MerchantDomain is one resolution the model was confident enough to make.
type MerchantDomain struct {
	ID     int    `json:"id"`
	Domain string `json:"domain"`
}

const resolveDomainsSystemPrompt = `You map a business's ordinary name onto its primary website domain, so a logo can be looked up by domain.

Rules:
- Answer ONLY for businesses you actually recognise. A domain you inferred by gluing the name together ("joes-corner-diner.com") is a wrong answer, not a good guess.
- Omit anything you are unsure about. Omitting costs nothing: the app falls back to a lettered avatar, which is a perfectly good outcome. A confidently wrong domain shows the user another company's logo.
- Give the business's own primary domain, never a payment processor, marketplace, delivery app, franchisor or parent conglomerate. If a name only tells you which processor was used, omit it.
- Local, regional and one-off merchants almost never have a domain you know. Omit them.
- A domain is bare, lowercase, and registrable: "amazon.com", "traderjoes.com", "marks-and-spencer.co.uk". No scheme, no "www.", no path, no port.
- Use the domain for the country the name suggests when a brand has several.
- Echo each id EXACTLY as given. Never invent an id.

Answer only by calling the resolve_merchant_domains tool. If you recognise none of them, call it with an empty list.`

var resolveDomainsTool = Tool{
	Name:        "resolve_merchant_domains",
	Description: "Return the website domain for each merchant you confidently recognise.",
	InputSchema: json.RawMessage(`{
		"type": "object",
		"properties": {
			"domains": {
				"type": "array",
				"description": "One entry per merchant you recognise. Omit the rest; an empty list is a valid answer.",
				"items": {
					"type": "object",
					"properties": {
						"id": {"type": "integer", "description": "The id given for this merchant, copied exactly"},
						"domain": {"type": "string", "description": "Bare lowercase registrable domain, e.g. \"amazon.com\""}
					},
					"required": ["id", "domain"]
				}
			}
		},
		"required": ["domains"]
	}`),
}

type resolveDomainsResult struct {
	Domains []MerchantDomain `json:"domains"`
}

// ResolveMerchantDomains asks the model which website each merchant belongs to.
//
// This is the step that makes the logo fetcher free: Logo.dev charges credits
// for name→domain resolution but not for the logo image itself, and the AI
// provider is a host the deployment has already sanctioned — it sees these same
// merchant names during categorisation, so nothing new leaves the host here.
//
// The answer is a guess and is treated as one. The caller validates every id
// and domain it gets back, and a domain that turns out to have no logo is
// recorded as "none" and never asked about again, so a wrong guess costs one
// request and then self-heals.
//
// Returns ErrDisabled when no key is configured.
func (c *Client) ResolveMerchantDomains(ctx context.Context, merchants []MerchantForDomain) ([]MerchantDomain, error) {
	if !c.Enabled() {
		return nil, ErrDisabled
	}
	if len(merchants) == 0 {
		return nil, nil
	}

	var b strings.Builder
	b.WriteString("Merchants (id — name):\n")
	for _, m := range merchants {
		fmt.Fprintf(&b, "- %d — %q\n", m.ID, m.Name)
	}

	resp, err := c.Complete(ctx, Request{
		System:     resolveDomainsSystemPrompt,
		Messages:   []Message{UserText(b.String())},
		Tools:      []Tool{resolveDomainsTool},
		ToolChoice: map[string]string{"type": "tool", "name": resolveDomainsTool.Name},
		MaxTokens:  2048,
	})
	if err != nil {
		return nil, err
	}

	uses := resp.ToolUses()
	if len(uses) == 0 {
		return nil, fmt.Errorf("ai: model did not call resolve_merchant_domains")
	}
	var out resolveDomainsResult
	if err := json.Unmarshal(uses[0].Input, &out); err != nil {
		return nil, fmt.Errorf("ai: decode merchant domains: %w", err)
	}

	// Tidy only. Whether a domain is *acceptable* is the caller's judgement —
	// this package knows the wire format, not the policy.
	cleaned := make([]MerchantDomain, 0, len(out.Domains))
	for _, d := range out.Domains {
		d.Domain = strings.ToLower(strings.TrimSpace(d.Domain))
		if d.Domain == "" {
			continue
		}
		cleaned = append(cleaned, d)
	}
	return cleaned, nil
}
