package plaid

import (
	"testing"

	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/config"
)

// testClient builds a client without touching the network; only request
// construction is under test.
func testClient(t *testing.T) *Client {
	t.Helper()
	c, err := New(config.PlaidConfig{
		Env:        "sandbox",
		ClientID:   "test-client",
		Secret:     "test-secret",
		Products:   []string{"transactions"},
		WebhookURL: "https://example.test/webhooks/plaid",
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return c
}

// Create mode must ask for the full history window, because it can never be
// widened afterwards.
func TestNewLinkTokenRequest(t *testing.T) {
	req := testClient(t).newLinkTokenRequest("user-1", "Tester")

	if req.HasAccessToken() {
		t.Error("create mode must not set an access token")
	}
	if !req.HasProducts() {
		t.Fatal("create mode must request products")
	}
	if !req.HasTransactions() {
		t.Fatal("create mode must request a transactions history window")
	}
	transactions := req.GetTransactions()
	if got := transactions.GetDaysRequested(); got != maxTransactionHistoryDays {
		t.Errorf("days_requested = %d, want %d", got, maxTransactionHistoryDays)
	}
	if got := req.GetUser().ClientUserId; got != "user-1" {
		t.Errorf("client_user_id = %q", got)
	}
	if got := req.GetWebhook(); got != "https://example.test/webhooks/plaid" {
		t.Errorf("webhook = %q", got)
	}
}

// Update mode is defined as much by what it omits as by what it sets. Plaid
// rejects a request carrying both an access token and products, and the history
// window cannot be widened by an update — so both must be absent. These are the
// two assertions that catch a regression here.
func TestNewUpdateLinkTokenRequest(t *testing.T) {
	req := testClient(t).newUpdateLinkTokenRequest("user-1", "Tester", "access-sandbox-abc")

	if !req.HasAccessToken() {
		t.Fatal("update mode must set the access token; it is what selects update mode")
	}
	if got := req.GetAccessToken(); got != "access-sandbox-abc" {
		t.Errorf("access_token = %q", got)
	}
	if req.HasProducts() {
		t.Error("update mode must omit products; Plaid rejects access_token + products")
	}
	if req.HasTransactions() {
		t.Error("update mode must omit transactions/days_requested; the window is fixed at link time")
	}

	// The shared setup still applies.
	if got := req.GetUser().ClientUserId; got != "user-1" {
		t.Errorf("client_user_id = %q", got)
	}
	if got := req.GetWebhook(); got != "https://example.test/webhooks/plaid" {
		t.Errorf("webhook = %q", got)
	}
}

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
