package api

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/networth"
)

// Doc 27's default-unchanged assertion.
//
// The rule: with `real` unset, every endpoint that gained the parameter returns
// exactly what it returned before doc 27. That is enforced here at the only
// place it can actually be guaranteed — the response structs — because the
// mechanism is `omitempty` on a nil pointer and a single missing tag would
// break it silently, adding a `"real_net_worth": null` to every payload every
// client has ever parsed.

func TestNominalResponsesCarryNoRealFields(t *testing.T) {
	cases := []struct {
		name    string
		payload any
	}{
		{
			"net worth snapshot",
			snapshotResponse{
				AsOf:             "2026-06-01",
				AssetsTotal:      decimal.NewFromInt(100),
				LiabilitiesTotal: decimal.NewFromInt(40),
				NetWorth:         decimal.NewFromInt(60),
				Breakdown:        &networth.Breakdown{},
			},
		},
		{
			"monthly trend point",
			trendPoint{
				Month:    "2026-06",
				Income:   decimal.NewFromInt(500),
				Spending: decimal.NewFromInt(300),
				Leftover: decimal.NewFromInt(200),
			},
		},
		{
			"investment performance",
			performanceResponse{Period: "1y", Computable: true},
		},
		{
			"projection assumptions",
			assumptionsResponse{
				RealReturnRate: decimal.RequireFromString("0.05"),
				InflationRate:  decimal.RequireFromString("0.03"),
				WithdrawalRate: decimal.RequireFromString("0.04"),
			},
		},
	}

	// Every key doc 27 added. Checked by exact name rather than by prefix,
	// because `real_return_rate` on the assumptions response predates all of
	// this — it is doc 15's REAL RETURN, an entirely different sense of the word,
	// and a prefix match would flag it forever.
	addedByDoc27 := []string{
		"real_assets_total", "real_liabilities_total", "real_net_worth",
		"real_income", "real_spending", "real_leftover",
		"real",
		"measured_inflation", "measured_inflation_years", "measured_inflation_note",
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.payload)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}

			var decoded map[string]json.RawMessage
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			for _, key := range addedByDoc27 {
				if _, present := decoded[key]; present {
					t.Errorf("nominal response carries %q — every doc 27 field must be omitempty\n%s",
						key, raw)
				}
			}
		})
	}
}

// The opt-in parameter. Anything unrecognised resolves to nominal, which is the
// safe direction: a typo must not silently change what a figure means.
func TestRealRequested(t *testing.T) {
	cases := map[string]bool{
		"":      false,
		"1":     true,
		"true":  true,
		"0":     false,
		"false": false,
		"yes":   false,
		"TRUE":  false, // deliberately strict; the clients send "true" or "1"
	}

	for value, want := range cases {
		r := httptest.NewRequest("GET", "/api/reports/trend?real="+value, nil)
		if got := realRequested(r); got != want {
			t.Errorf("realRequested(real=%q) = %v, want %v", value, got, want)
		}
	}

	// Absent entirely is nominal.
	if realRequested(httptest.NewRequest("GET", "/api/reports/trend", nil)) {
		t.Error("a request with no `real` parameter must be nominal")
	}
}
