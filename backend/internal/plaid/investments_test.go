package plaid

import (
	"testing"
	"time"

	plaidapi "github.com/plaid/plaid-go/v40/plaid"
	"github.com/shopspring/decimal"
)

// TestConvertInvestmentTransactionPreservesSign is the guard on the input every
// return calculation depends on.
//
// Plaid's convention is "positive means cash was DEBITED": a buy or a withdrawal
// is positive, a sale or a deposit is negative. Flipping it here to something
// that reads more naturally would silently invert every TWR and IRR the app
// reports, and the flip that IS wanted happens once, in reporting.ExternalFlows.
func TestConvertInvestmentTransactionPreservesSign(t *testing.T) {
	// A deposit: cash credited, so Plaid sends a negative amount.
	deposit := plaidapi.NewInvestmentTransaction(
		"txn-deposit", "acct-1",
		*plaidapi.NewNullableString(strPtr("sec-1")),
		"2026-03-15", "ACH Deposit",
		0,        // quantity
		-1000.00, // amount
		0,        // price
		*plaidapi.NewNullableFloat64(nil),
		plaidapi.INVESTMENTTRANSACTIONTYPE_CASH,
		plaidapi.INVESTMENTTRANSACTIONSUBTYPE_CONTRIBUTION,
		*plaidapi.NewNullableString(strPtr("USD")),
		*plaidapi.NewNullableString(nil),
	)

	got := convertInvestmentTransaction(*deposit)

	if !got.Amount.Equal(decimal.RequireFromString("-1000")) {
		t.Errorf("deposit amount = %s, want -1000 (Plaid's sign, unmodified)", got.Amount)
	}
	if got.Type != "cash" || got.Subtype != "contribution" {
		t.Errorf("type/subtype = %q/%q, want cash/contribution", got.Type, got.Subtype)
	}
	if got.Date.Format(time.DateOnly) != "2026-03-15" {
		t.Errorf("date = %s, want 2026-03-15", got.Date.Format(time.DateOnly))
	}
	if got.PlaidSecurityID == nil || *got.PlaidSecurityID != "sec-1" {
		t.Errorf("security id = %v, want sec-1", got.PlaidSecurityID)
	}

	// A buy: cash debited, so positive.
	buy := plaidapi.NewInvestmentTransaction(
		"txn-buy", "acct-1",
		*plaidapi.NewNullableString(strPtr("sec-1")),
		"2026-03-16", "Buy VTI",
		2.5, 688.75, 275.50,
		*plaidapi.NewNullableFloat64(nil),
		plaidapi.INVESTMENTTRANSACTIONTYPE_BUY,
		plaidapi.INVESTMENTTRANSACTIONSUBTYPE_BUY,
		*plaidapi.NewNullableString(strPtr("USD")),
		*plaidapi.NewNullableString(nil),
	)

	gotBuy := convertInvestmentTransaction(*buy)
	if !gotBuy.Amount.Equal(decimal.RequireFromString("688.75")) {
		t.Errorf("buy amount = %s, want 688.75", gotBuy.Amount)
	}
	// Exactness matters: 275.50 must not arrive as 275.49999999999997.
	if !gotBuy.Price.Valid || !gotBuy.Price.Decimal.Equal(decimal.RequireFromString("275.5")) {
		t.Errorf("price = %v, want exactly 275.5", gotBuy.Price)
	}
	if !gotBuy.Quantity.Valid || !gotBuy.Quantity.Decimal.Equal(decimal.RequireFromString("2.5")) {
		t.Errorf("quantity = %v, want 2.5", gotBuy.Quantity)
	}
}

// TestConvertInvestmentTransactionUnreadableDate: a date that will not parse
// leaves the zero value, which the syncer drops. Storing it would place the row
// before every real flow and distort the return series it feeds.
func TestConvertInvestmentTransactionUnreadableDate(t *testing.T) {
	txn := plaidapi.NewInvestmentTransaction(
		"txn-bad", "acct-1",
		*plaidapi.NewNullableString(nil),
		"not-a-date", "Mystery",
		0, 10.00, 0,
		*plaidapi.NewNullableFloat64(nil),
		plaidapi.INVESTMENTTRANSACTIONTYPE_CASH,
		plaidapi.INVESTMENTTRANSACTIONSUBTYPE_DEPOSIT,
		*plaidapi.NewNullableString(nil),
		*plaidapi.NewNullableString(nil),
	)

	got := convertInvestmentTransaction(*txn)
	if !got.Date.IsZero() {
		t.Errorf("date = %v, want the zero value so the syncer drops the row", got.Date)
	}
	// A missing currency still defaults, so the row is storable if a caller
	// chooses to keep it.
	if got.Currency != "USD" {
		t.Errorf("currency = %q, want the USD default", got.Currency)
	}
	if got.PlaidSecurityID != nil {
		t.Errorf("security id = %v, want nil for a cash movement", got.PlaidSecurityID)
	}
}

func strPtr(s string) *string { return &s }
