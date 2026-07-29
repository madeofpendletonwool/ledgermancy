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

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/config"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// The retirement surface, end to end against a real Postgres.
//
// The cases worth an HTTP test rather than a unit test are the ones where the
// boundary itself is the risk: an untagged account has to reach the response as
// a named exclusion, a contribution plan must not be writable across household
// lines, and the assumptions have to travel with the projection so a client
// cannot render a curve without them.
//
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' go test ./internal/api/
func TestProjectionEndpoints(t *testing.T) {
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
		Pool: pool, Queries: dbgen.New(pool),
		Config: config.Config{Retirement: config.RetirementConfig{MonteCarloEnabled: true}},
	}

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, sql)
		}
	}

	callerHousehold, callerUser := uuid.New(), uuid.New()
	otherHousehold, otherUser := uuid.New(), uuid.New()
	seedHousehold := func(hh, u uuid.UUID, name string) {
		exec(`INSERT INTO households (id, name) VALUES ($1, $2)`, hh, name)
		exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
		      VALUES ($1, $2, $3, 'x', 'Tester')`, u, hh, u.String()+"@example.test")
	}
	seedHousehold(callerHousehold, callerUser, "Projection Caller")
	seedHousehold(otherHousehold, otherUser, "Projection Other")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id = ANY($1)`,
			[]uuid.UUID{callerHousehold, otherHousehold})
	})

	// Two accounts in the caller's household: one tagged, one deliberately not.
	// The untagged one is the whole point — it must surface as an exclusion
	// rather than quietly vanishing from the totals.
	callerItem := uuid.New()
	exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, institution_name, is_shared)
	      VALUES ($1, $2, $3, '\x00', 'Broker', true)`, callerItem, callerUser, uuid.NewString())

	taggedID, untaggedID := uuid.New(), uuid.New()
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type, subtype, current_balance, tax_treatment)
	      VALUES ($1, $2, $3, 'Roth IRA', 'investment', 'roth', '50000', 'roth_ira')`,
		taggedID, callerItem, uuid.NewString())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type, subtype, current_balance)
	      VALUES ($1, $2, $3, 'Mystery 401k', 'investment', '401k', '75000')`,
		untaggedID, callerItem, uuid.NewString())

	// An account in the other household, to prove the write guard.
	otherItem, otherAccount := uuid.New(), uuid.New()
	exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, institution_name, is_shared)
	      VALUES ($1, $2, $3, '\x00', 'Broker', true)`, otherItem, otherUser, uuid.NewString())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type, subtype, current_balance, tax_treatment)
	      VALUES ($1, $2, $3, 'Not yours', 'investment', 'brokerage', '1000', 'taxable')`,
		otherAccount, otherItem, uuid.NewString())

	caller := auth.Identity{UserID: callerUser, HouseholdID: callerHousehold, DisplayName: "Tester"}
	withCaller := func(r *http.Request) *http.Request {
		return r.WithContext(auth.ContextWithIdentity(ctx, caller))
	}
	withAccount := func(r *http.Request, id uuid.UUID) *http.Request {
		routeCtx := chi.NewRouteContext()
		routeCtx.URLParams.Add("accountID", id.String())
		return r.WithContext(context.WithValue(
			auth.ContextWithIdentity(ctx, caller), chi.RouteCtxKey, routeCtx))
	}

	t.Run("assumptions default before anything is saved", func(t *testing.T) {
		rec := httptest.NewRecorder()
		srv.handleGetAssumptions(rec, withCaller(
			httptest.NewRequest(http.MethodGet, "/api/projections/assumptions", nil)))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
		}
		var got assumptionsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.RealReturnRate.String() != "0.05" {
			t.Errorf("real_return_rate = %s, want 0.05", got.RealReturnRate)
		}
		if got.CurrentAge != nil {
			t.Errorf("current_age = %d, want null — undecided is not zero", *got.CurrentAge)
		}
		if !got.SpendingIsDefaulted {
			t.Error("spending_is_defaulted should be true with nothing saved")
		}
		if got.Basis == "" {
			t.Error("assumptions must carry their basis")
		}
	})

	t.Run("assumptions round-trip", func(t *testing.T) {
		body := `{"real_return_rate":"0.06","inflation_rate":"0.025","withdrawal_rate":"0.035",
		          "target_retirement_age":60,"current_age":40,"annual_ss_income":"24000",
		          "ss_start_age":67,"target_annual_spending":"55000"}`
		rec := httptest.NewRecorder()
		srv.handleSaveAssumptions(rec, withCaller(httptest.NewRequest(
			http.MethodPut, "/api/projections/assumptions", strings.NewReader(body))))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
		}
		var got assumptionsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.CurrentAge == nil || *got.CurrentAge != 40 {
			t.Errorf("current_age = %v, want 40", got.CurrentAge)
		}
		if got.SpendingIsDefaulted {
			t.Error("spending_is_defaulted should be false once a target is set")
		}
		if got.TargetAnnualSpending == nil || got.TargetAnnualSpending.String() != "55000" {
			t.Errorf("target_annual_spending = %v, want 55000", got.TargetAnnualSpending)
		}
	})

	t.Run("a rate typed as a percentage is refused", func(t *testing.T) {
		// "5" meaning 5% must not become a 500%-a-year projection.
		body := `{"real_return_rate":"5","inflation_rate":"0.03","withdrawal_rate":"0.04",
		          "target_retirement_age":null,"current_age":null,"annual_ss_income":null,
		          "ss_start_age":null,"target_annual_spending":null}`
		rec := httptest.NewRecorder()
		srv.handleSaveAssumptions(rec, withCaller(httptest.NewRequest(
			http.MethodPut, "/api/projections/assumptions", strings.NewReader(body))))

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("contributions list both accounts and count the untagged one", func(t *testing.T) {
		rec := httptest.NewRecorder()
		srv.handleListContributions(rec, withCaller(
			httptest.NewRequest(http.MethodGet, "/api/projections/contributions", nil)))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
		}
		var got contributionsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(got.Accounts) != 2 {
			t.Fatalf("accounts = %d, want 2", len(got.Accounts))
		}
		if got.UntaggedAccounts != 1 {
			t.Errorf("untagged_accounts = %d, want 1", got.UntaggedAccounts)
		}
		if got.LimitsNote == "" {
			t.Error("the limits note must always be present, configured or not")
		}
		for _, a := range got.Accounts {
			if a.ID == taggedID && a.AnnualLimit == nil && got.LimitsConfigured {
				t.Error("a tagged IRA should carry its annual limit")
			}
			if a.ID == untaggedID && a.AnnualLimit != nil {
				t.Error("an untagged account has no known limit and must not claim one")
			}
		}
	})

	t.Run("a contribution round-trips and returns refreshed headroom", func(t *testing.T) {
		body := `{"monthly_contribution":"500.00","employer_match_pct":null,"annual_salary":null,
		          "employer_match_limit":null,"beneficiary_current_age":null,"beneficiary_target_age":null}`
		rec := httptest.NewRecorder()
		srv.handleSaveContribution(rec, withAccount(httptest.NewRequest(
			http.MethodPut, "/api/projections/contributions/"+taggedID.String(),
			strings.NewReader(body)), taggedID))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
		}
		var got contributionsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		for _, a := range got.Accounts {
			if a.ID != taggedID {
				continue
			}
			if a.MonthlyContribution.String() != "500" {
				t.Errorf("monthly_contribution = %s, want 500", a.MonthlyContribution)
			}
			if a.PlannedAnnual.String() != "6000" {
				t.Errorf("planned_annual = %s, want 6000", a.PlannedAnnual)
			}
		}
	})

	t.Run("a match percentage with no salary is refused", func(t *testing.T) {
		// A percentage of nothing is not an amount, and silently dropping it
		// would leave the user wondering why the match never appears.
		body := `{"monthly_contribution":"500.00","employer_match_pct":"0.05","annual_salary":null,
		          "employer_match_limit":null,"beneficiary_current_age":null,"beneficiary_target_age":null}`
		rec := httptest.NewRecorder()
		srv.handleSaveContribution(rec, withAccount(httptest.NewRequest(
			http.MethodPut, "/api/projections/contributions/"+taggedID.String(),
			strings.NewReader(body)), taggedID))

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("another household's account is not writable", func(t *testing.T) {
		body := `{"monthly_contribution":"999.00","employer_match_pct":null,"annual_salary":null,
		          "employer_match_limit":null,"beneficiary_current_age":null,"beneficiary_target_age":null}`
		rec := httptest.NewRecorder()
		srv.handleSaveContribution(rec, withAccount(httptest.NewRequest(
			http.MethodPut, "/api/projections/contributions/"+otherAccount.String(),
			strings.NewReader(body)), otherAccount))

		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body: %s", rec.Code, rec.Body.String())
		}

		var count int
		if err := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM account_contributions WHERE account_id = $1`,
			otherAccount).Scan(&count); err != nil {
			t.Fatalf("check: %v", err)
		}
		if count != 0 {
			t.Errorf("wrote %d rows into another household's account", count)
		}
	})

	t.Run("the projection carries its assumptions and names its exclusions", func(t *testing.T) {
		rec := httptest.NewRecorder()
		srv.handleRetirementProjection(rec, withCaller(
			httptest.NewRequest(http.MethodGet, "/api/projections/retirement", nil)))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
		}
		var got retirementResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}

		// The contract this whole surface rests on.
		if got.Assumptions.WithdrawalRate.String() != "0.035" {
			t.Errorf("assumptions did not travel with the projection: %+v", got.Assumptions)
		}
		if !got.Estimate || got.Basis == "" {
			t.Error("the estimate flag and basis must travel with every curve")
		}
		if len(got.Omissions) == 0 {
			t.Error("the model's known omissions must be stated, not left implicit")
		}

		if len(got.Projection.ExcludedAccounts) != 1 ||
			got.Projection.ExcludedAccounts[0] != "Mystery 401k" {
			t.Fatalf("excluded = %v, want [Mystery 401k]", got.Projection.ExcludedAccounts)
		}
		if got.Projection.ExcludedValue.String() != "75000" {
			t.Errorf("excluded_value = %s, want 75000 — the size of the gap, not just its count",
				got.Projection.ExcludedValue)
		}
		if len(got.Projection.Points) == 0 {
			t.Fatal("no points projected")
		}
		// The untagged $75,000 must be absent from the nest egg, not folded in.
		if got.Projection.Points[0].Retirement.GreaterThan(
			got.Projection.ExcludedValue) {
			t.Errorf("first point %s appears to include the excluded account",
				got.Projection.Points[0].Retirement)
		}

		// A target age is set, so the savings solve should be present.
		if got.Savings == nil {
			t.Error("required_savings should be present when a target age is set")
		}
	})

	t.Run("clearing a contribution removes the plan", func(t *testing.T) {
		rec := httptest.NewRecorder()
		srv.handleDeleteContribution(rec, withAccount(httptest.NewRequest(
			http.MethodDelete, "/api/projections/contributions/"+taggedID.String(), nil), taggedID))

		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204; body: %s", rec.Code, rec.Body.String())
		}
		var count int
		if err := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM account_contributions WHERE account_id = $1`,
			taggedID).Scan(&count); err != nil {
			t.Fatalf("check: %v", err)
		}
		if count != 0 {
			t.Errorf("plan rows remaining = %d, want 0", count)
		}
	})
}
