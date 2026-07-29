package api

import (
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Split arithmetic. Shares must sum to the transaction EXACTLY — a split that
// does not sum is a silent reporting error, so every case here asserts the
// total as well as the individual amounts.

func TestResolveSharesEqualSplitSumsExactly(t *testing.T) {
	people := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}

	cases := []struct {
		name  string
		total string
		n     int
		want  []string
	}{
		{
			// The canonical case from the doc. $100 three ways cannot divide
			// evenly, and the extra cent has to land somewhere deterministic.
			name:  "100 three ways",
			total: "100.00",
			n:     3,
			want:  []string{"33.34", "33.33", "33.33"},
		},
		{
			// A single indivisible cent. Two of the three get nothing, which is
			// correct: there is nothing to give them.
			name:  "one cent three ways",
			total: "0.01",
			n:     3,
			want:  []string{"0.01", "0.00", "0.00"},
		},
		{
			name:  "even split needs no remainder",
			total: "90.00",
			n:     3,
			want:  []string{"30.00", "30.00", "30.00"},
		},
		{
			name:  "two ways with an odd cent",
			total: "10.01",
			n:     2,
			want:  []string{"5.01", "5.00"},
		},
		{
			// A refund is a negative transaction, and the remainder has to
			// distribute in the same direction or the shares overshoot.
			name:  "negative amount",
			total: "-100.00",
			n:     3,
			want:  []string{"-33.34", "-33.33", "-33.33"},
		},
		{
			name:  "everything to one person",
			total: "42.42",
			n:     1,
			want:  []string{"42.42"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			total := decimal.RequireFromString(tc.total)
			got, err := resolveShares(setSplitsRequest{Equal: people[:tc.n]}, total)
			if err != nil {
				t.Fatalf("resolveShares: %v", err)
			}
			if len(got) != tc.n {
				t.Fatalf("got %d shares, want %d", len(got), tc.n)
			}

			sum := decimal.Zero
			for i, sh := range got {
				if sh.amount.StringFixed(2) != tc.want[i] {
					t.Errorf("share %d = %s, want %s", i, sh.amount.StringFixed(2), tc.want[i])
				}
				sum = sum.Add(sh.amount)
			}
			// The invariant that matters more than the individual amounts.
			if !sum.Equal(total) {
				t.Errorf("shares sum to %s, want exactly %s",
					sum.StringFixed(2), total.StringFixed(2))
			}
		})
	}
}

func TestResolveSharesRejectsDuplicatePeople(t *testing.T) {
	id := uuid.New()
	total := decimal.RequireFromString("10.00")

	if _, err := resolveShares(setSplitsRequest{Equal: []uuid.UUID{id, id}}, total); err == nil {
		t.Error("expected an error for a duplicate person in equal")
	}
	if _, err := resolveShares(setSplitsRequest{Shares: []splitShareRequest{
		{PersonID: id, Amount: "5.00"},
		{PersonID: id, Amount: "5.00"},
	}}, total); err == nil {
		t.Error("expected an error for a duplicate person in shares")
	}
}

func TestResolveSharesExplicitAmountsPassThrough(t *testing.T) {
	a, b := uuid.New(), uuid.New()

	got, err := resolveShares(setSplitsRequest{Shares: []splitShareRequest{
		{PersonID: a, Amount: "60.00"},
		{PersonID: b, Amount: "40.00"},
	}}, decimal.RequireFromString("100.00"))
	if err != nil {
		t.Fatalf("resolveShares: %v", err)
	}

	// Explicit amounts are stored as given. The handler asserts the sum
	// separately — resolveShares does not silently adjust them to fit, because
	// adjusting somebody's stated share is worse than refusing the request.
	if got[0].amount.StringFixed(2) != "60.00" || got[1].amount.StringFixed(2) != "40.00" {
		t.Errorf("explicit shares were altered: %s / %s",
			got[0].amount.StringFixed(2), got[1].amount.StringFixed(2))
	}
}

func TestResolveSharesRejectsUnparseableAmount(t *testing.T) {
	if _, err := resolveShares(setSplitsRequest{Shares: []splitShareRequest{
		{PersonID: uuid.New(), Amount: "not money"},
	}}, decimal.RequireFromString("1.00")); err == nil {
		t.Error("expected an error for an unparseable amount")
	}
}
