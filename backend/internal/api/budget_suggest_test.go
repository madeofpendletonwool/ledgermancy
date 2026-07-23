package api

import (
	"testing"

	"github.com/shopspring/decimal"
)

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func TestRoundBudgetTarget(t *testing.T) {
	cases := []struct{ avg, want string }{
		{"487.63", "500"},   // <500 → step 25, rounds up
		{"150.00", "150"},   // <200 → step 10, already round
		{"42.10", "50"},     // <200 → step 10
		{"1234.56", "1300"}, // >=1000 → step 100
		{"640.00", "650"},   // <1000 → step 50
	}
	for _, c := range cases {
		got := roundBudgetTarget(dec(c.avg))
		if !got.Equal(dec(c.want)) {
			t.Errorf("roundBudgetTarget(%s) = %s, want %s", c.avg, got, c.want)
		}
		// Round-up invariant: a target is never below its average.
		if got.LessThan(dec(c.avg)) {
			t.Errorf("roundBudgetTarget(%s) = %s is below the average", c.avg, got)
		}
	}
}

func TestAcceptableTarget(t *testing.T) {
	avg := dec("487.63")
	cases := []struct {
		target string
		want   bool
		why    string
	}{
		{"500", true, "in band and round"},
		{"490", false, "not a multiple of 25"},
		{"5000", false, "far above the band"},
		{"100", false, "far below the band"},
		{"-500", false, "negative"},
		{"550", true, "in band (up to 1.3x) and round for step 50"},
	}
	for _, c := range cases {
		got := acceptableTarget(dec(c.target), avg)
		if got != c.want {
			t.Errorf("acceptableTarget(%s, %s) = %v, want %v (%s)", c.target, avg, got, c.want, c.why)
		}
	}
}
