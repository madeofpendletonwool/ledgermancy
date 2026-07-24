package categorize

import "testing"

func TestDetectTransfer(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// Real examples from production that Plaid returned as OTHER_OTHER.
		{"capital one mobile payment", "ach capital one - mobile pmt", "credit-card-payment"},
		{"capital one account transfer", "ach capital one - transfer", "transfer-out"},

		// Other clear card-payment phrasings.
		{"autopay", "chase autopay", "credit-card-payment"},
		{"payment thank you", "payment thank you - discover", "credit-card-payment"},
		{"card payment", "amex card payment", "credit-card-payment"},

		// Clear self-transfers.
		{"online transfer", "online transfer to savings", "transfer-out"},
		{"xfer", "mobile xfer", "transfer-out"},

		// Must NOT match — these are real spending and a false positive would
		// hide them from every spending figure.
		{"restaurant", "little tea shop deli", ""},
		{"fast food", "taco bell", ""},
		{"union dues", "ach wisconsin educat - neaefttran", ""},
		{"plain merchant", "menards", ""},
		{"payroll not a card", "payroll transfer", ""},
		{"grocery", "aldi", ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := detectTransfer(c.in); got != c.want {
				t.Errorf("detectTransfer(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
