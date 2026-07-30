package plaid

import (
	"context"
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

// Every skip must say why.
//
// A module that returned Skipped with nothing attached is how a household's
// liabilities never landed on any sweep, in total silence — no error, no warning,
// nothing in the sync summary — and the only way to discover it was to read
// SyncLiabilities. The reason is what turns that into a log line.
//
// Both config-gated branches return before touching s.Queries, so a Syncer with
// a nil Queries exercises them safely.
func TestSkippedModulesAlwaysGiveAReason(t *testing.T) {
	ctx := context.Background()

	disabled := &Syncer{Client: newTestClient(t, []string{"transactions"}, nil)}
	enabled := &Syncer{Client: newTestClient(t, []string{"transactions"},
		[]string{"investments", "liabilities"})}

	debt := itemAccountKinds{debt: true, investment: true}
	nothing := itemAccountKinds{}

	for _, tc := range []struct {
		name string
		run  func() (ModuleResult, error)
	}{
		{"liabilities, product disabled", func() (ModuleResult, error) {
			return disabled.SyncLiabilities(ctx, "token", nil, debt)
		}},
		{"liabilities, no debt accounts", func() (ModuleResult, error) {
			return enabled.SyncLiabilities(ctx, "token", nil, nothing)
		}},
		{"investments, product disabled", func() (ModuleResult, error) {
			return disabled.SyncInvestments(ctx, "token", nil, debt)
		}},
		{"investments, no investment accounts", func() (ModuleResult, error) {
			return enabled.SyncInvestments(ctx, "token", nil, nothing)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := tc.run()
			if err != nil {
				t.Fatalf("a gated skip is not an error: %v", err)
			}
			if !result.Skipped {
				t.Fatal("expected the module to skip")
			}
			if result.SkipReason == "" {
				t.Error("Skipped with no SkipReason is the silent failure this guards against")
			}
		})
	}

	// The two liabilities causes must be distinguishable — one is an operator's
	// decision, the other is the shape of the item, and they send whoever is
	// debugging to different files.
	off, _ := disabled.SyncLiabilities(ctx, "token", nil, debt)
	empty, _ := enabled.SyncLiabilities(ctx, "token", nil, nothing)
	if off.SkipReason == empty.SkipReason {
		t.Errorf("both reasons are %q; the causes are different", off.SkipReason)
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
