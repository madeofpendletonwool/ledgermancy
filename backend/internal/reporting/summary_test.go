package reporting

import (
	"testing"

	"github.com/shopspring/decimal"
)

// formatUSD is what keeps the model from ever seeing a bare "20256.87": every
// figure it quotes must already carry the "$" and thousands separators. The
// grouping is hand-rolled, so pin the edges.
func TestFormatUSD(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"0", "$0.00"},
		{"0.5", "$0.50"},
		{"12", "$12.00"},
		{"123", "$123.00"},
		{"1000", "$1,000.00"},
		{"1234.5", "$1,234.50"},
		{"20256.87", "$20,256.87"},
		{"1234567.89", "$1,234,567.89"},
		{"-1240", "-$1,240.00"},
		{"-0.01", "-$0.01"},
	}
	for _, c := range cases {
		got := formatUSD(decimal.RequireFromString(c.in))
		if got != c.want {
			t.Errorf("formatUSD(%s) = %q, want %q", c.in, got, c.want)
		}
	}
}
