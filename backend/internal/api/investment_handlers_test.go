package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/config"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// The Investments HTTP surface against a real Postgres.
//
// The cases that matter are the honesty ones: a thin history must refuse to
// report a return, a partial cost basis must be disclosed rather than averaged
// away, and a tax treatment must never be written from a suggestion.
//
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' go test ./internal/api/
func TestInvestmentEndpoints(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	srv := &Server{
		Pool:    pool,
		Queries: dbgen.New(pool),
		// Benchmarks off, matching the default. The chart must still serve the
		// portfolio line rather than erroring or 404ing.
		Config: config.Config{Benchmarks: config.BenchmarkConfig{Enabled: false}},
	}

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, sql)
		}
	}

	householdID, userID := uuid.New(), uuid.New()
	otherHousehold, otherUser := uuid.New(), uuid.New()
	itemID, otherItemID := uuid.New(), uuid.New()
	brokerageID, iraID, foreignAccountID := uuid.New(), uuid.New(), uuid.New()
	securityID := uuid.New()

	seedHousehold := func(hh, u, item uuid.UUID, name string) {
		exec(`INSERT INTO households (id, name) VALUES ($1, $2)`, hh, name)
		exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
		      VALUES ($1, $2, $3, 'x', 'Tester')`, u, hh, u.String()+"@example.test")
		exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, products, status)
		      VALUES ($1, $2, $3, '\x00', '{investments}', 'active')`, item, u, item.String())
	}
	seedHousehold(householdID, userID, itemID, "Investments Caller")
	seedHousehold(otherHousehold, otherUser, otherItemID, "Investments Other")

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id = ANY($1)`,
			[]uuid.UUID{householdID, otherHousehold})
		_, _ = pool.Exec(context.Background(), `DELETE FROM securities WHERE id = $1`, securityID)
	})

	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type, subtype, current_balance)
	      VALUES ($1, $2, $3, 'Brokerage', 'investment', 'brokerage', 25000.00)`,
		brokerageID, itemID, brokerageID.String())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type, subtype, current_balance)
	      VALUES ($1, $2, $3, 'Workplace plan', 'investment', '401k', 15000.00)`,
		iraID, itemID, iraID.String())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type, subtype, current_balance)
	      VALUES ($1, $2, $3, 'Someone else', 'investment', 'brokerage', 99999.00)`,
		foreignAccountID, otherItemID, foreignAccountID.String())
	exec(`INSERT INTO securities (id, plaid_security_id, name, ticker, type, close_price)
	      VALUES ($1, $2, 'Total Market ETF', 'VTI', 'etf', 275.50)`, securityID, securityID.String())
	// One position with a basis, one without: the coverage disclosure has
	// nothing to disclose unless both exist.
	exec(`INSERT INTO holdings (account_id, security_id, quantity, cost_basis, institution_price, institution_value)
	      VALUES ($1, $2, 80.5, 18000.00, 275.50, 22177.75)`, brokerageID, securityID)
	exec(`INSERT INTO holdings (account_id, security_id, quantity, institution_price, institution_value)
	      VALUES ($1, $2, 50.0, 275.50, 13775.00)`, iraID, securityID)

	caller := auth.Identity{UserID: userID, HouseholdID: householdID, DisplayName: "Tester"}
	withCaller := func(r *http.Request) *http.Request {
		return r.WithContext(auth.ContextWithIdentity(ctx, caller))
	}
	withParam := func(r *http.Request, accountID uuid.UUID) *http.Request {
		routeCtx := chi.NewRouteContext()
		routeCtx.URLParams.Add("accountID", accountID.String())
		return r.WithContext(context.WithValue(
			auth.ContextWithIdentity(ctx, caller), chi.RouteCtxKey, routeCtx))
	}

	t.Run("overview discloses partial basis and never pre-tags", func(t *testing.T) {
		rec := httptest.NewRecorder()
		srv.handleInvestmentOverview(rec, withCaller(
			httptest.NewRequest(http.MethodGet, "/api/investments/", nil)))

		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var body investmentOverviewResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}

		// Only the caller's two accounts; the other household's 99,999 must not
		// be reachable through this endpoint.
		if len(body.Accounts) != 2 {
			t.Fatalf("got %d accounts, want 2 (visibility scoping leaked?)", len(body.Accounts))
		}
		if !body.TotalValue.Equal(decFromString(t, "40000.00")) {
			t.Errorf("total_value = %s, want 40000.00", body.TotalValue)
		}

		// Every account starts untagged, and nothing may write one on the
		// user's behalf.
		if body.UntaggedAccounts != 2 {
			t.Errorf("untagged_accounts = %d, want 2", body.UntaggedAccounts)
		}
		for _, a := range body.Accounts {
			if a.TaxTreatment != nil {
				t.Errorf("account %q was auto-tagged %q", a.Name, *a.TaxTreatment)
			}
			switch a.Name {
			case "Brokerage":
				if a.SuggestedTaxTreatment != "taxable" {
					t.Errorf("brokerage suggestion = %q, want taxable", a.SuggestedTaxTreatment)
				}
			case "Workplace plan":
				// Plaid reports "401k" for both traditional and Roth plans, so
				// there must be no suggestion at all.
				if a.SuggestedTaxTreatment != "" {
					t.Errorf("401k suggestion = %q, want no suggestion — it cannot be known",
						a.SuggestedTaxTreatment)
				}
			}
		}

		// The gain covers one holding; the other is excluded and counted.
		if body.BasisExcluded != 1 {
			t.Errorf("basis_excluded_holdings = %d, want 1", body.BasisExcluded)
		}
		if body.UnrealisedGain == nil {
			t.Fatal("unrealised_gain is nil despite one holding reporting a basis")
		}
		// 22177.75 − 18000.00 = 4177.75, computed over the covered holding only.
		if !body.UnrealisedGain.Equal(decFromString(t, "4177.75")) {
			t.Errorf("unrealised_gain = %s, want 4177.75", body.UnrealisedGain)
		}
		if !body.BasisCoverage.Equal(decFromString(t, "22177.75")) {
			t.Errorf("basis_coverage_value = %s, want 22177.75", body.BasisCoverage)
		}
	})

	t.Run("performance refuses to report on no history", func(t *testing.T) {
		rec := httptest.NewRecorder()
		srv.handleInvestmentPerformance(rec, withCaller(
			httptest.NewRequest(http.MethodGet, "/api/investments/performance?period=1y", nil)))

		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var body performanceResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.Computable {
			t.Error("performance reported as computable with no snapshots recorded")
		}
		if body.Caveat == "" {
			t.Error("no caveat explaining why performance is unavailable")
		}
		if body.TWR != nil || body.MWR != nil {
			t.Errorf("returns present without history: twr=%v mwr=%v", body.TWR, body.MWR)
		}
	})

	t.Run("performance rejects an unknown period", func(t *testing.T) {
		rec := httptest.NewRecorder()
		srv.handleInvestmentPerformance(rec, withCaller(
			httptest.NewRequest(http.MethodGet, "/api/investments/performance?period=10y", nil)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status %d, want 400", rec.Code)
		}
	})

	t.Run("benchmarks serve without the fetch enabled", func(t *testing.T) {
		rec := httptest.NewRecorder()
		srv.handleInvestmentBenchmarks(rec, withCaller(
			httptest.NewRequest(http.MethodGet, "/api/investments/benchmarks?period=1y", nil)))

		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var body benchmarkResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.Enabled {
			t.Error("enabled = true with BENCHMARK_PRICES_ENABLED off")
		}
		if body.Basis == "" {
			t.Error("no basis statement; a rebased index must never read as a dollar value")
		}
	})

	t.Run("allocation keeps the untagged gap visible", func(t *testing.T) {
		rec := httptest.NewRecorder()
		srv.handleInvestmentAllocation(rec, withCaller(
			httptest.NewRequest(http.MethodGet, "/api/investments/allocation", nil)))

		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var body allocationResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}

		var sawUntagged bool
		for _, s := range body.ByTaxTreatment {
			if s.Label == "Untagged" {
				sawUntagged = true
			}
		}
		if !sawUntagged {
			t.Errorf("no Untagged slice in %v; untagged value must not be hidden", body.ByTaxTreatment)
		}
		if body.Note == "" {
			t.Error("allocation note is empty; partial coverage must be disclosed")
		}
	})

	t.Run("fees refuse to guess", func(t *testing.T) {
		rec := httptest.NewRecorder()
		srv.handleInvestmentFees(rec, withCaller(
			httptest.NewRequest(http.MethodGet, "/api/investments/fees", nil)))

		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var body feeDragResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		// No expense-ratio source exists, so the only honest cost is zero with
		// every holding declared excluded.
		if !body.AnnualCost.IsZero() {
			t.Errorf("annual_cost = %s with no ratio source, want 0", body.AnnualCost)
		}
		if body.ExcludedHoldings != 2 {
			t.Errorf("excluded_holdings = %d, want 2", body.ExcludedHoldings)
		}
		if body.Note == "" {
			t.Error("fee note is empty; coverage must always be disclosed")
		}
	})

	t.Run("tax treatment is written only from an explicit choice", func(t *testing.T) {
		rec := httptest.NewRecorder()
		srv.handleSetAccountTaxTreatment(rec, withParam(
			httptest.NewRequest(http.MethodPatch, "/api/investments/accounts/x/tax-treatment",
				strings.NewReader(`{"tax_treatment":"roth_401k","is_managed":false}`)), iraID))

		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var body taxTreatmentResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.TaxTreatment == nil || *body.TaxTreatment != "roth_401k" {
			t.Fatalf("tax_treatment = %v, want roth_401k", body.TaxTreatment)
		}

		// The overview must now count one fewer untagged account.
		overviewRec := httptest.NewRecorder()
		srv.handleInvestmentOverview(overviewRec, withCaller(
			httptest.NewRequest(http.MethodGet, "/api/investments/", nil)))
		var overview investmentOverviewResponse
		if err := json.Unmarshal(overviewRec.Body.Bytes(), &overview); err != nil {
			t.Fatalf("decode overview: %v", err)
		}
		if overview.UntaggedAccounts != 1 {
			t.Errorf("untagged_accounts = %d after tagging one, want 1", overview.UntaggedAccounts)
		}
	})

	t.Run("tax treatment rejects a value outside the constraint", func(t *testing.T) {
		rec := httptest.NewRecorder()
		srv.handleSetAccountTaxTreatment(rec, withParam(
			httptest.NewRequest(http.MethodPatch, "/api/investments/accounts/x/tax-treatment",
				strings.NewReader(`{"tax_treatment":"crypto_yolo"}`)), iraID))

		// A readable 400, not a 500 from the CHECK constraint.
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status %d, want 400: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("tax treatment cannot reach another household", func(t *testing.T) {
		rec := httptest.NewRecorder()
		srv.handleSetAccountTaxTreatment(rec, withParam(
			httptest.NewRequest(http.MethodPatch, "/api/investments/accounts/x/tax-treatment",
				strings.NewReader(`{"tax_treatment":"taxable"}`)), foreignAccountID))

		if rec.Code != http.StatusNotFound {
			t.Errorf("status %d, want 404: %s", rec.Code, rec.Body.String())
		}
	})

	// The end-to-end case: recorded values and a real contribution go in as
	// rows, and a correct time-weighted return comes out of the endpoint. The
	// unit tests pin the maths and the query tests pin the SQL; this is the only
	// thing that proves the flow classification actually connects the two — a
	// subtype the classifier does not recognise would make the deposit vanish
	// from the flow list and the return would silently include it as growth.
	t.Run("performance strips a real contribution end to end", func(t *testing.T) {
		exec(`DELETE FROM investment_snapshots WHERE account_id = $1`, brokerageID)
		exec(`DELETE FROM investment_transactions WHERE account_id = $1`, brokerageID)

		// 100 → 110 (a genuine 10% gain), then a 100 contribution lands and the
		// account closes at 210. The honest answer is 10%, not 110%.
		for _, row := range [][2]string{
			{"2026-06-01", "100.00"},
			{"2026-06-02", "110.00"},
			{"2026-06-03", "210.00"},
		} {
			exec(`INSERT INTO investment_snapshots (account_id, as_of, market_value)
			      VALUES ($1, $2, $3)`, brokerageID, row[0], row[1])
		}
		// Plaid's sign: cash credited into the account is negative.
		exec(`INSERT INTO investment_transactions
		      (account_id, plaid_investment_transaction_id, type, subtype, amount, date, name)
		      VALUES ($1, $2, 'cash', 'contribution', -100.00, '2026-06-03', 'ACH Deposit')`,
			brokerageID, uuid.New().String())

		rec := httptest.NewRecorder()
		srv.handleInvestmentPerformance(rec, withCaller(
			httptest.NewRequest(http.MethodGet, "/api/investments/performance?period=inception", nil)))

		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var body performanceResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !body.Computable {
			t.Fatalf("not computable with three snapshots: %s", body.Caveat)
		}
		if body.TWR == nil {
			t.Fatal("twr is nil")
		}
		if !body.TWR.Round(6).Equal(decFromString(t, "0.1")) {
			t.Errorf("twr = %s, want 0.1 — the contribution was counted as growth",
				body.TWR)
		}
		// Positive = money into the portfolio, i.e. Plaid's sign flipped once.
		if !body.NetFlows.Equal(decFromString(t, "100")) {
			t.Errorf("net_flows = %s, want 100", body.NetFlows)
		}
		if !body.Gain.Equal(decFromString(t, "10")) {
			t.Errorf("gain = %s, want 10", body.Gain)
		}
		// Three days of history must never be annualised.
		if body.Annualised != nil {
			t.Errorf("annualised = %s over three days; it must be null", body.Annualised)
		}
		if body.Caveat == "" {
			t.Error("a three-day return was reported with no caveat")
		}
	})

	t.Run("holdings list is scoped to the household", func(t *testing.T) {
		rec := httptest.NewRecorder()
		srv.handleInvestmentHoldings(rec, withCaller(
			httptest.NewRequest(http.MethodGet, "/api/investments/holdings", nil)))

		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var body []detailedHoldingResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(body) != 2 {
			t.Fatalf("got %d holdings, want 2", len(body))
		}

		var withGain, withoutGain int
		for _, h := range body {
			if h.Gain != nil {
				withGain++
			} else {
				withoutGain++
			}
		}
		// The basis-less holding must report no gain rather than a gain of its
		// full value.
		if withGain != 1 || withoutGain != 1 {
			t.Errorf("gain coverage = %d with / %d without, want 1 / 1", withGain, withoutGain)
		}
	})
}

// decFromString is a test helper: a parse failure here is a broken test, not a
// broken assertion.
func decFromString(t *testing.T, s string) decimal.Decimal {
	t.Helper()
	d, err := decimal.NewFromString(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return d
}

// decNullable is decFromString for the nullable columns sqlc generates.
func decNullable(t *testing.T, s string) decimal.NullDecimal {
	t.Helper()
	return decimal.NullDecimal{Decimal: decFromString(t, s), Valid: true}
}
