package obligations

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
)

// TestAmountRangeContains pins the boundaries, which are the whole point of a
// stated range: a bill that lands exactly on a bound is INSIDE it. Anything else
// makes "$40 to $60" a lie to anyone whose phone bill is reliably $60.
func TestAmountRangeContains(t *testing.T) {
	r := AmountRange{
		Min: decimal.RequireFromString("40.00"),
		Max: decimal.RequireFromString("60.00"),
	}

	cases := []struct {
		amount string
		want   bool
	}{
		{"39.99", false},
		{"40.00", true}, // exactly the low bound
		{"50.00", true},
		{"60.00", true}, // exactly the high bound
		{"60.01", false},
		{"90.00", false},
	}
	for _, c := range cases {
		got := r.Contains(decimal.RequireFromString(c.amount))
		if got != c.want {
			t.Errorf("Contains(%s) = %v, want %v", c.amount, got, c.want)
		}
	}
}

func TestAmountRangeLabel(t *testing.T) {
	r := AmountRange{
		Min: decimal.RequireFromString("40"),
		Max: decimal.RequireFromString("60.5"),
	}
	if got, want := r.Label(), "40.00–60.50"; got != want {
		t.Errorf("Label() = %q, want %q", got, want)
	}
}

// TestListUpcomingCarriesRange checks the expansion hands the stated range
// through to its callers, and leaves it nil for the ordinary un-ranged bill.
// Without this the Schedule page silently renders every bill as un-ranged.
func TestListUpcomingCarriesRange(t *testing.T) {
	f := newFixture(t, "Range Expansion")
	ctx := context.Background()

	ranged := f.addObligation(t, "Phone", "50.00", 1, "month", "2026-01-15", nil)
	f.exec(`UPDATE recurring_obligations SET amount_min = '40.00', amount_max = '60.00'
	        WHERE id = $1`, ranged)
	plain := f.addObligation(t, "Rent", "1800.00", 1, "month", "2026-01-01", nil)

	occurrences, err := ListUpcoming(ctx, f.q, f.householdID, f.userID,
		mustDate(t, "2026-01-01"), mustDate(t, "2026-01-31"))
	if err != nil {
		t.Fatalf("list upcoming: %v", err)
	}

	seen := map[string]*AmountRange{}
	for _, o := range occurrences {
		switch o.ObligationID {
		case ranged:
			seen["phone"] = o.Range
		case plain:
			seen["rent"] = o.Range
		}
	}

	got, ok := seen["phone"]
	if !ok {
		t.Fatal("expected the ranged obligation to expand into the window")
	}
	if got == nil {
		t.Fatal("expected the phone bill to carry its stated range, got nil")
	}
	if got.Label() != "40.00–60.00" {
		t.Errorf("range = %s, want 40.00–60.00", got.Label())
	}

	if r, ok := seen["rent"]; !ok {
		t.Fatal("expected the un-ranged obligation to expand into the window")
	} else if r != nil {
		t.Errorf("an obligation with no stated range should carry nil, got %s", r.Label())
	}
}

// TestAmountRangeConstraint pins the both-or-neither rule at the table, which is
// what lets every layer above treat a non-nil range as complete.
func TestAmountRangeConstraint(t *testing.T) {
	f := newFixture(t, "Range Constraint")
	ctx := context.Background()

	id := f.addObligation(t, "Phone", "50.00", 1, "month", "2026-01-15", nil)

	cases := []struct {
		name     string
		min, max any
		wantErr  bool
	}{
		{"both set", "40.00", "60.00", false},
		{"neither set", nil, nil, false},
		{"low bound only", "40.00", nil, true},
		{"high bound only", nil, "60.00", true},
		{"inverted", "60.00", "40.00", true},
		{"zero low bound", "0", "60.00", true},
		{"equal bounds", "50.00", "50.00", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := f.pool.Exec(ctx,
				`UPDATE recurring_obligations SET amount_min = $2, amount_max = $3 WHERE id = $1`,
				id, c.min, c.max)
			if c.wantErr && err == nil {
				t.Errorf("expected the CHECK to reject min=%v max=%v", c.min, c.max)
			}
			if !c.wantErr && err != nil {
				t.Errorf("expected min=%v max=%v to be accepted: %v", c.min, c.max, err)
			}
		})
	}
}
