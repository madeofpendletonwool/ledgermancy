package plaid

import (
	"testing"

	"github.com/shopspring/decimal"
)

// The Plaid SDK hands us amounts as float64, so this conversion is the single
// float in the entire money path. This test pins the property the rest of the
// system depends on: the decimal we store is the literal amount Plaid sent,
// not a binary-floating-point approximation of it.
func TestAmountToDecimalIsExact(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{12.34, "12.34"},
		{0.1, "0.1"},
		{0.2, "0.2"},
		{0.3, "0.3"},
		{1.005, "1.005"},
		{19.99, "19.99"},
		{-45.67, "-45.67"}, // a refund / income
		{2999.99, "2999.99"},
		{123456.78, "123456.78"},
		{0.01, "0.01"},
		{0, "0"},
	}

	for _, tc := range cases {
		got := amountToDecimal(tc.in)
		want := decimal.RequireFromString(tc.want)
		if !got.Equal(want) {
			t.Errorf("amountToDecimal(%v) = %s, want %s", tc.in, got, want)
		}
	}
}

// Summing converted amounts must stay exact — this is the 0.1+0.2 trap that
// would silently corrupt every monthly total if amounts stayed float64.
func TestConvertedAmountsSumExactly(t *testing.T) {
	sum := decimal.Zero
	for _, f := range []float64{0.1, 0.2, 19.99, 0.01} {
		sum = sum.Add(amountToDecimal(f))
	}

	want := decimal.RequireFromString("20.30")
	if !sum.Equal(want) {
		t.Errorf("sum = %s, want %s", sum, want)
	}

	// The same arithmetic in float64, for contrast: this is what we avoid.
	var floatSum float64
	for _, f := range []float64{0.1, 0.2, 19.99, 0.01} {
		floatSum += f
	}
	if floatSum == 20.30 {
		t.Log("note: float64 happened to land exactly here, but is not guaranteed to")
	}
}

func TestMoneyHandlesNil(t *testing.T) {
	if got := money(nil); got.Valid {
		t.Error("nil balance should produce an invalid NullDecimal")
	}

	v := 250.75
	got := money(&v)
	if !got.Valid {
		t.Fatal("non-nil balance should be valid")
	}
	if !got.Decimal.Equal(decimal.RequireFromString("250.75")) {
		t.Errorf("money(250.75) = %s", got.Decimal)
	}
}

func TestParseProducts(t *testing.T) {
	got, err := parseProducts([]string{"transactions", "investments", "liabilities"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("expected 3 products, got %d", len(got))
	}

	if _, err := parseProducts([]string{"nonsense"}); err == nil {
		t.Error("expected an error for an unsupported product")
	}
	if _, err := parseProducts(nil); err == nil {
		t.Error("expected an error when no products are configured")
	}
}

func TestEnvironmentRejectsRetiredDevelopment(t *testing.T) {
	if _, err := environment("sandbox"); err != nil {
		t.Errorf("sandbox: %v", err)
	}
	if _, err := environment("production"); err != nil {
		t.Errorf("production: %v", err)
	}
	// Plaid retired this environment; accepting it would fail confusingly later.
	if _, err := environment("development"); err == nil {
		t.Error("expected 'development' to be rejected")
	}
}
