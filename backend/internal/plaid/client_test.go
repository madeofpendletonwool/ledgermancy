package plaid

import (
	"testing"

	"github.com/shopspring/decimal"

	plaidapi "github.com/plaid/plaid-go/v40/plaid"
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

// confidenceLevel is the exact failure mode behind the masked-BP-charge-became-
// "Runn" bug: Plaid returned a cleansed fragment with LOW confidence and the
// ingest path trusted it. Per Plaid's own docs, LOW means "we didn't find a
// matching counterparty", so the merchant_name must be dropped there and fall
// back to the raw description. MEDIUM and above are real matches and kept.
func TestMerchantNameIfConfident(t *testing.T) {
	cases := []struct {
		name            string
		merchant        string
		confidences     []string // counterparty confidence_level values
		wantTrusted     bool
	}{
		{"empty merchant never trusted", "", []string{"VERY_HIGH"}, false},
		{"very high trusted", "Amazon", []string{"VERY_HIGH"}, true},
		{"high trusted", "Target", []string{"HIGH"}, true},
		{"medium trusted", "Trader Joe's", []string{"MEDIUM"}, true},
		{"low dropped (the BP/Runn case)", "Runn", []string{"LOW"}, false},
		{"unknown dropped", "Mystery", []string{"UNKNOWN"}, false},
		{"no counterparties dropped", "Alone", nil, false},
		{"best of several wins", "Amazon", []string{"LOW", "VERY_HIGH", "LOW"}, true},
		{"best is only low", "Runn", []string{"LOW", "UNKNOWN"}, false},
		{"whitespace merchant treated as empty", "   ", []string{"HIGH"}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cps := make([]plaidapi.TransactionCounterparty, 0, len(tc.confidences))
			for _, c := range tc.confidences {
				cp := plaidapi.NewTransactionCounterpartyWithDefaults()
				cp.SetConfidenceLevel(c)
				cps = append(cps, *cp)
			}
			got := merchantNameIfConfident(tc.merchant, cps)
			if tc.wantTrusted && got == nil {
				t.Fatalf("expected merchant_name %q to be trusted, got nil", tc.merchant)
			}
			if tc.wantTrusted && *got != tc.merchant {
				t.Errorf("trusted merchant_name = %q, want %q", *got, tc.merchant)
			}
			if !tc.wantTrusted && got != nil {
				t.Errorf("expected merchant_name %q to be dropped, got %q", tc.merchant, *got)
			}
		})
	}
}

func TestConfidenceRank(t *testing.T) {
	cases := map[string]int{
		"VERY_HIGH": 4,
		"very_high": 4, // case-insensitive
		"HIGH":      3,
		"MEDIUM":    2,
		"LOW":       1,
		"UNKNOWN":   0,
		"":          -1,
		"BOGUS":     -1,
	}
	for level, want := range cases {
		if got := confidenceRank(level); got != want {
			t.Errorf("confidenceRank(%q) = %d, want %d", level, got, want)
		}
	}
}
