package reporting

import (
	"testing"

	"github.com/shopspring/decimal"
)

// dec lives in returns_test.go.

func decs(ss ...string) []decimal.Decimal {
	out := make([]decimal.Decimal, 0, len(ss))
	for _, s := range ss {
		out = append(out, dec(s))
	}
	return out
}

func TestMedianOf(t *testing.T) {
	cases := []struct {
		name string
		in   []decimal.Decimal
		want string
	}{
		{"empty is zero", nil, "0"},
		{"single value", decs("540.22"), "540.22"},
		{"odd count takes the middle", decs("3", "1", "2"), "2"},
		{"even count averages the two middles", decs("4", "1", "3", "2"), "2.5"},
		{"already sorted", decs("1", "2", "3", "4", "5"), "3"},
		{"reverse sorted", decs("5", "4", "3", "2", "1"), "3"},
		{"all equal", decs("100", "100", "100"), "100"},
		{"zeros count", decs("0", "0", "900"), "0"},
		{
			// The case this whole change exists for: five ordinary months of a
			// $540 car payment plus the month it was paid off. The mean is
			// $2,833.55 — a figure the household never paid once.
			"a payoff does not move the median",
			decs("540.22", "540.22", "540.22", "540.22", "540.22", "14835.76"),
			"540.22",
		},
		{
			// Symmetric: a bonus month must not raise expected income.
			"a bonus does not move the median",
			decs("8974.53", "9163.38", "9849.46", "10778.46", "11394.87", "24000.00"),
			"10313.96",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := medianOf(tc.in); !got.Equal(dec(tc.want)) {
				t.Errorf("medianOf(%v) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

// TestMedianOfDoesNotReorderInput pins the defensive copy. The caller builds its
// slice in month order from a query result and other code reads it that way; a
// median that sorted in place would silently shuffle months under it.
func TestMedianOfDoesNotReorderInput(t *testing.T) {
	in := decs("300", "100", "200")
	_ = medianOf(in)

	for i, want := range []string{"300", "100", "200"} {
		if !in[i].Equal(dec(want)) {
			t.Fatalf("input reordered: got %v, want [300 100 200]", in)
		}
	}
}
