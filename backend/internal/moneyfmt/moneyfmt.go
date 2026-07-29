// Package moneyfmt is the one place server-generated prose turns a decimal into
// a dollar figure. It exists because there were two: `reporting.formatUSD`
// grouped thousands correctly while `insights.money` did not, so an insight body
// quoting a four-figure amount rendered "$1234.56" while the monthly recap
// rendered "$1,234.56" for the same number. Every call site now routes here.
//
// Nothing in this package computes. The decimal arrives finished — rounded by
// whoever did the arithmetic — and is only decorated.
package moneyfmt

import (
	"strings"

	"github.com/shopspring/decimal"
)

// USD renders a decimal as a display-ready dollar figure with a leading "$" and
// thousands separators: "$0.00", "$1,234.56", "-$1,234.56". The sign leads the
// symbol, matching Intl.NumberFormat on the frontend, so the same amount reads
// identically whether the string was built in Go or in the browser.
//
// The AI layer quotes these verbatim, so the model never sees a bare "1234.56"
// to mangle.
func USD(d decimal.Decimal) string {
	neg := d.IsNegative()
	s := d.Abs().StringFixed(2)

	dot := strings.IndexByte(s, '.')
	intPart, frac := s[:dot], s[dot:]

	var grouped strings.Builder
	n := len(intPart)
	for i := 0; i < n; i++ {
		if i > 0 && (n-i)%3 == 0 {
			grouped.WriteByte(',')
		}
		grouped.WriteByte(intPart[i])
	}

	out := "$" + grouped.String() + frac
	if neg {
		out = "-" + out
	}
	return out
}
