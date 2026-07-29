package plaid

import (
	"errors"
	"testing"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/config"
)

func newTestClient(t *testing.T, products, optional []string) *Client {
	t.Helper()
	c, err := New(config.PlaidConfig{
		Env:              "sandbox",
		ClientID:         "test-client",
		Secret:           "test-secret",
		Products:         products,
		OptionalProducts: optional,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return c
}

// The optional modules must gate on CONFIG, never on plaid_items.products.
//
// Gating on the stored column was a real bug: it records what was requested at
// link time, so an operator who linked with transactions and later enabled
// liabilities got nothing forever — and the only way to change that column is a
// relink, which orphans transaction history and cannot re-widen the history
// window. Plaid serves these products on an existing token regardless.
func TestSyncProductsComesFromConfigNotTheItem(t *testing.T) {
	c := newTestClient(t, []string{"transactions"}, []string{"investments", "liabilities"})

	sync := c.SyncProducts()
	for _, p := range []string{ProductTransactions, ProductInvestments, ProductLiabilities} {
		if !HasProduct(sync, p) {
			t.Errorf("SyncProducts() = %v, missing %s", sync, p)
		}
	}

	// An item linked long before those products were enabled must still sync
	// them — that is the whole point.
	legacyItemProducts := []string{"transactions"}
	if HasProduct(legacyItemProducts, ProductLiabilities) {
		t.Fatal("test premise is wrong: the legacy item does list liabilities")
	}
}

// Required products filter the institution list; optional ones must not. Keeping
// them in separate arrays is what stops enabling Liabilities from hiding every
// bank that has no loan products from a user who only wanted their chequing
// account.
func TestOptionalProductsAreSeparateFromRequired(t *testing.T) {
	c := newTestClient(t, []string{"transactions"}, []string{"investments", "liabilities"})

	if got := c.Products(); len(got) != 1 || got[0] != ProductTransactions {
		t.Errorf("Products() = %v, want only transactions — anything more narrows Link", got)
	}
	if got := c.OptionalProducts(); len(got) != 2 {
		t.Errorf("OptionalProducts() = %v, want investments and liabilities", got)
	}

	// Plaid rejects a product listed in both arrays, so an overlapping config
	// must be de-duplicated rather than passed through.
	dup := newTestClient(t, []string{"transactions", "liabilities"}, []string{"liabilities"})
	if HasProduct(dup.OptionalProducts(), ProductLiabilities) {
		t.Errorf("OptionalProducts() = %v, must not repeat a required product", dup.OptionalProducts())
	}
	if !HasProduct(dup.SyncProducts(), ProductLiabilities) {
		t.Error("SyncProducts() must still include a product that was required")
	}
}

func TestOptionalProductsMayBeEmpty(t *testing.T) {
	c := newTestClient(t, []string{"transactions"}, nil)
	if len(c.OptionalProducts()) != 0 {
		t.Errorf("OptionalProducts() = %v, want empty", c.OptionalProducts())
	}
	if HasProduct(c.SyncProducts(), ProductLiabilities) {
		t.Error("opting out must actually disable the module")
	}
}

func TestNoteAccountType(t *testing.T) {
	var k itemAccountKinds
	for _, at := range []string{"depository", "credit"} {
		k.noteAccountType(at)
	}
	if !k.debt {
		t.Error("a credit account means the item has debt")
	}
	if k.investment {
		t.Error("no investment account was seen")
	}

	var loans itemAccountKinds
	loans.noteAccountType("loan")
	if !loans.debt {
		t.Error("a loan account means the item has debt")
	}

	// A chequing-only item must not be asked about its mortgages.
	var cashOnly itemAccountKinds
	cashOnly.noteAccountType("depository")
	if cashOnly.debt || cashOnly.investment {
		t.Error("a depository-only item feeds neither optional module")
	}
}

// A per-item refusal is data, not failure: it must not log an error every sweep
// for every institution that simply doesn't offer the product.
func TestProductUnavailable(t *testing.T) {
	quiet := []string{
		"get liabilities: plaid INVALID_REQUEST/PRODUCTS_NOT_SUPPORTED: not supported",
		"get holdings: plaid ITEM_ERROR/NO_INVESTMENT_ACCOUNTS: none found",
		"get liabilities: plaid ITEM_ERROR/ADDITIONAL_CONSENT_REQUIRED: consent needed",
		"get liabilities: plaid ITEM_ERROR/PRODUCT_NOT_READY: still pulling",
	}
	for _, msg := range quiet {
		if !productUnavailable(errors.New(msg)) {
			t.Errorf("%q should be a quiet skip", msg)
		}
	}

	loud := []string{
		"get liabilities: plaid ITEM_ERROR/ITEM_LOGIN_REQUIRED: reauth",
		"get liabilities: plaid API_ERROR/INTERNAL_SERVER_ERROR: try again",
		"get liabilities: connection refused",
	}
	for _, msg := range loud {
		if productUnavailable(errors.New(msg)) {
			t.Errorf("%q is a real failure and must surface", msg)
		}
	}
}
