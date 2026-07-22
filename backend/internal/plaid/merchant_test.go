package plaid

import "testing"

func TestMerchantKeyNormalizes(t *testing.T) {
	cases := []struct {
		name     string
		merchant string
		txnName  string
		want     string
	}{
		{"plaid merchant name preferred", "Starbucks", "SQ *STARBUCKS #1234 SEATTLE WA", "starbucks"},
		{"square prefix stripped", "", "SQ *BLUE BOTTLE COFFEE", "blue bottle coffee"},
		{"toast prefix stripped", "", "TST* LOCAL DINER", "local diner"},
		{"store number stripped", "", "TARGET #1234", "target"},
		{"trailing city and state stripped", "", "WHOLE FOODS MKT AUSTIN TX", "whole foods mkt"},
		{"reference ids stripped", "", "AMAZON MKTPL 8H47DK2 123456789", "amazon mktpl"},
		{"case normalized", "NETFLIX.COM", "", "netflix com"},
		{"whitespace collapsed", "  Trader   Joe's  ", "", "trader joe's"},
		{"falls back to txn name", "", "COSTCO WHSE #0455", "costco whse"},
		{"single digit in name survives", "", "7ELEVEN", "7eleven"},
		{"two-word city leaves stable remainder", "", "PEETS COFFEE SAN FRANCISCO CA", "peets coffee san"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MerchantKey(tc.merchant, tc.txnName); got != tc.want {
				t.Errorf("MerchantKey(%q, %q) = %q, want %q",
					tc.merchant, tc.txnName, got, tc.want)
			}
		})
	}
}

// The whole point of the key is that store-to-store variations of the same
// merchant collapse together, so one categorization covers all of them.
func TestMerchantKeyCollapsesStoreVariants(t *testing.T) {
	variants := []string{
		"SQ *BLUE BOTTLE #4412 OAKLAND CA",
		"SQ *BLUE BOTTLE #0087 BERKELEY CA",
		"SQ *BLUE BOTTLE",
	}

	first := MerchantKey("", variants[0])
	if first == "" {
		t.Fatal("expected a non-empty key")
	}
	for _, v := range variants[1:] {
		if got := MerchantKey("", v); got != first {
			t.Errorf("MerchantKey(%q) = %q, want %q (same merchant should collapse)", v, got, first)
		}
	}
}

func TestMerchantKeyRejectsNoise(t *testing.T) {
	for _, in := range []string{"", "   ", "#1", "42", "**"} {
		if got := MerchantKey("", in); got != "" {
			t.Errorf("MerchantKey(%q) = %q, want \"\" (too little signal to cache on)", in, got)
		}
	}
}
