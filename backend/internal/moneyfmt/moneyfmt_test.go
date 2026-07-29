package moneyfmt

import (
	"testing"

	"github.com/shopspring/decimal"
)

// USD is what keeps a four-figure amount out of an insight body as "$1234.56".
// The boundary cases are the group boundaries themselves and the sign, which is
// the one place a grouping helper usually gets it wrong.
func TestUSD(t *testing.T) {
	cases := []struct{ in, want string }{
		{"0", "$0.00"},
		{"0.5", "$0.50"},
		{"999.99", "$999.99"},
		{"1000", "$1,000.00"},
		{"1234.56", "$1,234.56"},
		{"12345.6", "$12,345.60"},
		{"999999.99", "$999,999.99"},
		{"1234567.89", "$1,234,567.89"},
		{"-1234.56", "-$1,234.56"},
		{"-999.99", "-$999.99"},
		{"-0.01", "-$0.01"},
		// Rounded to the cent, not truncated.
		{"1234.565", "$1,234.57"},
	}
	for _, c := range cases {
		if got := USD(decimal.RequireFromString(c.in)); got != c.want {
			t.Errorf("USD(%s) = %q, want %q", c.in, got, c.want)
		}
	}
}
