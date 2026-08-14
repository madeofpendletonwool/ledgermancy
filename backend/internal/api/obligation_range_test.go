package api

import "testing"

// The expected-range half of the obligation form's validation (MAD-120). These
// rules are also a CHECK on the table; validating here is what turns a constraint
// violation into a sentence the form can show beside the field.
func TestValidateObligationAmountRange(t *testing.T) {
	base := upsertObligationRequest{
		Label:         "Phone bill",
		Amount:        "50.00",
		IntervalCount: 1,
		IntervalUnit:  "month",
		AnchorDate:    "2026-01-15",
	}

	cases := []struct {
		name     string
		min, max string
		wantErr  string
		wantSet  bool
	}{
		{name: "no range", wantSet: false},
		{name: "both bounds", min: "40.00", max: "60.00", wantSet: true},
		{name: "equal bounds", min: "50.00", max: "50.00", wantSet: true},
		{
			name: "low bound only", min: "40.00",
			wantErr: "an expected range needs both a low and a high amount",
		},
		{
			name: "high bound only", max: "60.00",
			wantErr: "an expected range needs both a low and a high amount",
		},
		{
			name: "inverted", min: "60.00", max: "40.00",
			wantErr: "amount_min must not be greater than amount_max",
		},
		{
			name: "zero low bound", min: "0", max: "60.00",
			wantErr: "amount_min must be greater than zero",
		},
		{
			name: "unparseable low bound", min: "forty", max: "60.00",
			wantErr: `amount_min must be a decimal number, e.g. "40.00"`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := base
			req.AmountMin, req.AmountMax = c.min, c.max

			v, err := validateObligationBody(req)
			if c.wantErr != "" {
				if err == nil {
					t.Fatalf("expected %q, got no error", c.wantErr)
				}
				if err.Error() != c.wantErr {
					t.Fatalf("error = %q, want %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if v.amountMin.Valid != c.wantSet || v.amountMax.Valid != c.wantSet {
				t.Fatalf("range set = (%v, %v), want both %v",
					v.amountMin.Valid, v.amountMax.Valid, c.wantSet)
			}
			if c.wantSet && v.amountMin.Decimal.StringFixed(2) != c.min {
				t.Errorf("amount_min = %s, want %s", v.amountMin.Decimal.StringFixed(2), c.min)
			}
		})
	}
}
