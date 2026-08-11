package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/allocation"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/testdb"
)

// The allocation planner against a real Postgres.
//
// The arithmetic has its own tests in internal/allocation, where it belongs and
// where it runs without a database. What has to be proven HERE is everything the
// boundary owns and the engine cannot:
//
//   - RUNNING A PLAN CHANGES NOTHING. The highest-priority item in the doc's
//     verification list, and the one an in-memory test cannot make: the engine
//     being pure is necessary but not sufficient, because a handler could always
//     write on the way past.
//   - Visibility scope. A spouse's private account never appears in a bucket.
//   - The saved blob really does hold decimal STRINGS in Postgres, not just in
//     Go — the continuity rule is about what is in the column.
//   - input_version round-trips through the database.
//
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' go test ./internal/api/ -run TestAllocation

type allocationFixture struct {
	t   *testing.T
	ctx context.Context
	srv *Server

	household, user uuid.UUID
	// spouse holds a PRIVATE item in the same household — the visibility case.
	spouse uuid.UUID

	item, privateItem uuid.UUID
	roth, brokerage   uuid.UUID
	checking, savings uuid.UUID
	card              uuid.UUID
	spouseRoth        uuid.UUID
}

func newAllocationFixture(t *testing.T) *allocationFixture {
	t.Helper()

	url := testdb.URL(t)

	ctx := context.Background()
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	f := &allocationFixture{
		t: t, ctx: ctx,
		srv:       &Server{Pool: pool, Queries: dbgen.New(pool)},
		household: uuid.New(), user: uuid.New(), spouse: uuid.New(),
		item: uuid.New(), privateItem: uuid.New(),
		roth: uuid.New(), brokerage: uuid.New(),
		checking: uuid.New(), savings: uuid.New(),
		card: uuid.New(), spouseRoth: uuid.New(),
	}

	f.exec(`INSERT INTO households (id, name) VALUES ($1, 'Allocation')`, f.household)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id = $1`, f.household)
	})
	f.exec(`INSERT INTO users (id, household_id, email, password_hash, display_name, role)
	        VALUES ($1, $2, $3, 'x', 'Tester', 'owner')`, f.user, f.household, f.user.String()+"@t.test")
	f.exec(`INSERT INTO users (id, household_id, email, password_hash, display_name, role)
	        VALUES ($1, $2, $3, 'x', 'Spouse', 'member')`, f.spouse, f.household, f.spouse.String()+"@t.test")

	f.exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, status)
	        VALUES ($1, $2, $3, '\x00', 'active')`, f.item, f.user, f.item.String())
	// is_shared FALSE: the spouse's own institution, invisible to the caller.
	f.exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, status, is_shared)
	        VALUES ($1, $2, $3, '\x00', 'active', false)`,
		f.privateItem, f.spouse, f.privateItem.String())

	acct := func(id, item uuid.UUID, name, typ, subtype string, balance string, treatment any) {
		f.exec(`INSERT INTO accounts
		          (id, plaid_item_id, plaid_account_id, name, type, subtype, current_balance, tax_treatment)
		        VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			id, item, id.String(), name, typ, subtype, balance, treatment)
	}
	acct(f.roth, f.item, "Roth IRA", "investment", "ira", "20000.00", "roth_ira")
	acct(f.brokerage, f.item, "Brokerage", "investment", "brokerage", "50000.00", "taxable")
	acct(f.checking, f.item, "Checking", "depository", "checking", "10000.00", nil)
	acct(f.savings, f.item, "Savings", "depository", "savings", "15000.00", nil)
	acct(f.card, f.item, "Visa", "credit", "credit card", "6000.00", nil)
	acct(f.spouseRoth, f.privateItem, "Spouse Private Roth", "investment", "ira", "99999.00", "roth_ira")

	f.exec(`UPDATE accounts SET deposit_apy = 4.50 WHERE id = $1`, f.savings)
	f.exec(`UPDATE accounts SET deposit_apy = 0.40 WHERE id = $1`, f.checking)

	return f
}

func (f *allocationFixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.srv.Pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("seed: %v\n%s", err, sql)
	}
}

func (f *allocationFixture) identity() auth.Identity {
	return auth.Identity{UserID: f.user, HouseholdID: f.household, Role: auth.RoleOwner}
}

// call routes one request through the handler with the fixture's identity and
// the chi route context the real router would have built, so the URL params and
// the scope arguments are production's rather than ones the test made up.
func (f *allocationFixture) call(
	handler http.HandlerFunc, method, target string, body any, params map[string]string,
) *httptest.ResponseRecorder {
	f.t.Helper()

	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			f.t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}

	req := httptest.NewRequest(method, target, reader)
	req.Header.Set("Content-Type", "application/json")

	rctx := chi.NewRouteContext()
	for k, v := range params {
		rctx.URLParams.Add(k, v)
	}
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	ctx = auth.ContextWithIdentity(ctx, f.identity())

	rec := httptest.NewRecorder()
	handler(rec, req.WithContext(ctx))
	return rec
}

// --------------------------------------------------------------------------
// The rule everything else rests on
// --------------------------------------------------------------------------

// mutationWatchedTables are the tables a plan must not touch. Named explicitly
// rather than "every table", because a diff over everything would also catch
// session rows and audit events and be useless as a signal.
var mutationWatchedTables = []string{
	"accounts", "holdings", "goals", "account_contributions", "projection_assumptions",
}

// snapshot serialises each watched table to a stable string. Ordered by primary
// key so a row's position cannot make an unchanged table look changed.
func (f *allocationFixture) snapshot() map[string]string {
	f.t.Helper()
	out := map[string]string{}
	for _, table := range mutationWatchedTables {
		rows, err := f.srv.Pool.Query(f.ctx,
			`SELECT to_jsonb(t)::text FROM `+table+` t ORDER BY t.id`)
		if err != nil {
			f.t.Fatalf("snapshot %s: %v", table, err)
		}
		var b strings.Builder
		for rows.Next() {
			var raw string
			if err := rows.Scan(&raw); err != nil {
				rows.Close()
				f.t.Fatalf("scan %s: %v", table, err)
			}
			b.WriteString(raw)
			b.WriteByte('\n')
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			f.t.Fatalf("snapshot %s: %v", table, err)
		}
		out[table] = b.String()
	}
	return out
}

// RUNNING A PLAN MUST NOT MUTATE REAL DATA. The doc's highest-priority
// verification item.
//
// A POST that computes is exactly the shape a well-meaning change turns into a
// POST that saves — "while we're here, let's persist the contribution the user
// clearly wants". This test is what makes that a build failure rather than a
// household discovering its 401(k) plan silently rewritten by a projection it
// was only looking at.
func TestAllocationRunDoesNotMutateRealData(t *testing.T) {
	f := newAllocationFixture(t)

	before := f.snapshot()

	rec := f.call(f.srv.handleRunAllocation, "POST", "/api/allocation/plan", map[string]any{
		"lump": "30000.00", "monthly": "1800.00", "horizon_years": 17,
		"target_nest_egg": "1500000.00",
		"splits": []map[string]any{
			{"account_id": f.roth.String(), "lump_pct": "25", "monthly_pct": "30"},
			{"account_id": f.brokerage.String(), "lump_pct": "50", "monthly_pct": "20"},
			{"account_id": f.card.String(), "lump_pct": "15", "monthly_pct": "50"},
			{"account_id": f.savings.String(), "lump_pct": "10", "monthly_pct": "0"},
		},
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("run: %d %s", rec.Code, rec.Body.String())
	}

	after := f.snapshot()
	for _, table := range mutationWatchedTables {
		if before[table] != after[table] {
			t.Errorf("%s changed during a plan run — a projection wrote to real data", table)
		}
	}

	// And it actually produced a plan, so the assertion above is not passing
	// because nothing happened.
	var result allocation.Result
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(result.Buckets) != 4 {
		t.Fatalf("buckets = %d, want 4", len(result.Buckets))
	}
	if !result.ProjectedAssets.IsPositive() {
		t.Errorf("projected assets = %s, want positive", result.ProjectedAssets)
	}
}

// --------------------------------------------------------------------------
// Visibility
// --------------------------------------------------------------------------

// A SPOUSE'S PRIVATE ACCOUNT NEVER APPEARS IN A BUCKET, and naming it in a split
// is refused rather than silently ignored. The second half matters as much as
// the first: a silently dropped line would leave the percentages summing to 100
// while that share of the money went nowhere.
func TestAllocationBucketsRespectVisibility(t *testing.T) {
	f := newAllocationFixture(t)

	rec := f.call(f.srv.handleAllocationBuckets, "GET", "/api/allocation/buckets", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("buckets: %d %s", rec.Code, rec.Body.String())
	}
	var resp allocationBucketsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	for _, b := range resp.Buckets {
		if b.AccountID == f.spouseRoth {
			t.Fatal("a spouse's private account appeared as an allocatable bucket")
		}
	}
	// The caller's own accounts are all there: five seeded, none private to them.
	if len(resp.Buckets) != 5 {
		t.Errorf("buckets = %d, want the caller's 5", len(resp.Buckets))
	}

	run := f.call(f.srv.handleRunAllocation, "POST", "/api/allocation/plan", map[string]any{
		"lump": "1000.00", "monthly": "0", "horizon_years": 10,
		"splits": []map[string]any{
			{"account_id": f.spouseRoth.String(), "lump_pct": "100", "monthly_pct": "0"},
		},
	}, nil)
	if run.Code != http.StatusBadRequest {
		t.Errorf("allocating into an invisible account = %d, want 400 (%s)", run.Code, run.Body.String())
	}
}

// --------------------------------------------------------------------------
// Saved plans
// --------------------------------------------------------------------------

// MONEY IN THE JSONB COLUMN IS A STRING IN POSTGRES, not merely in Go. The
// continuity rule is a claim about what is in the column — export.go passes
// jsonb through as json.RawMessage, so nothing downstream will fix it.
func TestAllocationSavedPlanStoresMoneyAsStrings(t *testing.T) {
	f := newAllocationFixture(t)

	rec := f.call(f.srv.handleSavePlan, "POST", "/api/allocation/plans", map[string]any{
		"name": "Thirty grand", "lump": "30000.50", "monthly": "1800.10", "horizon_years": 17,
		"target_nest_egg": "1500000.20",
		"splits": []map[string]any{
			{"account_id": f.roth.String(), "lump_pct": "0.1", "monthly_pct": "0.2",
				"real_return_rate": "0.0575"},
			{"account_id": f.brokerage.String(), "lump_pct": "99.9", "monthly_pct": "99.8"},
		},
	}, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	var saved planResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Read the raw jsonb back out of the database, as text. This is the check
	// that cannot be faked by the Go types on either side.
	var inputs string
	if err := f.srv.Pool.QueryRow(f.ctx,
		`SELECT inputs::text FROM allocation_plans WHERE id = $1`, saved.ID,
	).Scan(&inputs); err != nil {
		t.Fatalf("read inputs: %v", err)
	}

	for _, want := range []string{
		`"lump": "30000.50"`, `"monthly": "1800.10"`, `"target": "1500000.20"`,
		`"lump_pct": "0.10"`, `"real_return_rate": "0.0575"`,
	} {
		// Postgres renders jsonb with a space after the colon; normalise both
		// sides so the assertion is about the STRING-ness, not the whitespace.
		if !strings.Contains(squash(inputs), squash(want)) {
			t.Errorf("saved inputs do not contain %s — money must be a decimal string inside jsonb.\nGot: %s",
				want, inputs)
		}
	}
	// And no bare number where money belongs.
	for _, field := range []string{"lump", "monthly", "target", "lump_pct", "monthly_pct"} {
		if strings.Contains(squash(inputs), squash(`"`+field+`":`)+`0`) ||
			strings.Contains(squash(inputs), squash(`"`+field+`":`)+`1`) {
			t.Errorf("%q looks like a bare JSON number in %s", field, inputs)
		}
	}

	// horizon_years is a COUNT and stays a number: it is not money and it is
	// exact in float64.
	if !strings.Contains(squash(inputs), squash(`"horizon_years": 17`)) {
		t.Errorf("horizon_years should be a JSON number: %s", inputs)
	}
}

// squash removes whitespace so an assertion is about structure rather than about
// how Postgres chose to render the blob.
func squash(s string) string {
	return strings.NewReplacer(" ", "", "\n", "", "\t", "").Replace(s)
}

// OPENING A SAVED PLAN RECOMPUTES IT against the live baseline. This is the
// whole reason results are not stored: a plan is a decision, and the projection
// it rested on is a statement about balances that have since moved.
func TestAllocationSavedPlanRecomputesOnOpen(t *testing.T) {
	f := newAllocationFixture(t)

	rec := f.call(f.srv.handleSavePlan, "POST", "/api/allocation/plans", map[string]any{
		"name": "Steady", "lump": "10000.00", "monthly": "500.00", "horizon_years": 10,
		"splits": []map[string]any{
			{"account_id": f.brokerage.String(), "lump_pct": "100", "monthly_pct": "100"},
		},
	}, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	var saved planResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if saved.InputVersion != allocation.CurrentInputVersion {
		t.Errorf("input_version = %d, want %d", saved.InputVersion, allocation.CurrentInputVersion)
	}

	open := func() allocation.Result {
		t.Helper()
		rec := f.call(f.srv.handleGetPlan, "GET", "/api/allocation/plans/"+saved.ID.String(),
			nil, map[string]string{"planID": saved.ID.String()})
		if rec.Code != http.StatusOK {
			t.Fatalf("open: %d %s", rec.Code, rec.Body.String())
		}
		var got planResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.ResultError != "" {
			t.Fatalf("result error: %s", got.ResultError)
		}
		if got.Result == nil {
			t.Fatal("a saved plan opened without a recomputed result")
		}
		// The INPUTS came back byte-identical — that is what is stored.
		if got.Inputs.Lump != "10000.00" || got.Inputs.Monthly != "500.00" {
			t.Errorf("inputs = %+v, want the decimal strings that were saved", got.Inputs)
		}
		return *got.Result
	}

	first := open()

	// Move the underlying balance. A stored projection would not notice.
	f.exec(`UPDATE accounts SET current_balance = 150000.00 WHERE id = $1`, f.brokerage)

	second := open()
	if second.ProjectedAssets.Equal(first.ProjectedAssets) {
		t.Errorf("projection did not change after the balance did (%s both times) — "+
			"the result is being stored rather than recomputed", first.ProjectedAssets)
	}
	if !second.ProjectedAssets.GreaterThan(first.ProjectedAssets) {
		t.Errorf("balance tripled but the projection fell: %s → %s",
			first.ProjectedAssets, second.ProjectedAssets)
	}
}

// A plan id belonging to another household is a 404, not somebody else's plan.
func TestAllocationPlanIsHouseholdScoped(t *testing.T) {
	f := newAllocationFixture(t)

	other := uuid.New()
	f.exec(`INSERT INTO households (id, name) VALUES ($1, 'Elsewhere')`, other)
	t.Cleanup(func() {
		_, _ = f.srv.Pool.Exec(context.Background(), `DELETE FROM households WHERE id = $1`, other)
	})
	foreign := uuid.New()
	f.exec(`INSERT INTO allocation_plans (id, household_id, name, inputs, assumptions)
	        VALUES ($1, $2, 'Theirs', '{}'::jsonb, '{}'::jsonb)`, foreign, other)

	rec := f.call(f.srv.handleGetPlan, "GET", "/api/allocation/plans/"+foreign.String(),
		nil, map[string]string{"planID": foreign.String()})
	if rec.Code != http.StatusNotFound {
		t.Errorf("reading another household's plan = %d, want 404", rec.Code)
	}

	del := f.call(f.srv.handleDeletePlan, "DELETE", "/api/allocation/plans/"+foreign.String(),
		nil, map[string]string{"planID": foreign.String()})
	if del.Code != http.StatusNotFound {
		t.Errorf("deleting another household's plan = %d, want 404", del.Code)
	}

	// And it is still there.
	var count int
	if err := f.srv.Pool.QueryRow(f.ctx,
		`SELECT count(*) FROM allocation_plans WHERE id = $1`, foreign).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Error("another household's plan was deleted")
	}
}

// A plan saved under a version this build cannot read STILL OPENS — showing its
// inputs and an explanation — rather than 500ing. A saved decision the user
// cannot look at is worse than one that cannot be projected.
func TestAllocationUnreadableVersionStillOpens(t *testing.T) {
	f := newAllocationFixture(t)

	id := uuid.New()
	f.exec(`INSERT INTO allocation_plans (id, household_id, name, inputs, input_version, assumptions)
	        VALUES ($1, $2, 'From the future', $3::jsonb, 99, '{}'::jsonb)`,
		id, f.household, `{"lump":"1.00","monthly":"0.00","horizon_years":5,"buckets":[]}`)

	rec := f.call(f.srv.handleGetPlan, "GET", "/api/allocation/plans/"+id.String(),
		nil, map[string]string{"planID": id.String()})
	if rec.Code != http.StatusOK {
		t.Fatalf("open: %d %s", rec.Code, rec.Body.String())
	}
	var got planResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Result != nil {
		t.Error("a plan this build cannot read must not be projected anyway")
	}
	if !strings.Contains(got.ResultError, "version") {
		t.Errorf("result_error = %q, want it to name the version problem", got.ResultError)
	}
	if got.Name != "From the future" {
		t.Errorf("name = %q — the row itself should still be readable", got.Name)
	}
}

// --------------------------------------------------------------------------
// Idle cash and deposit yields, end to end
// --------------------------------------------------------------------------

// The cash-drag detector over real rows, and the write that turns it on.
func TestAllocationIdleCashEndToEnd(t *testing.T) {
	f := newAllocationFixture(t)

	rec := f.call(f.srv.handleIdleCash, "GET", "/api/accounts/idle-cash", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("idle cash: %d %s", rec.Code, rec.Body.String())
	}
	var report allocation.IdleCashReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !report.HasBenchmark {
		t.Fatal("want a benchmark: the savings account is at 4.50%")
	}
	if report.BenchmarkAccount != "Savings" {
		t.Errorf("benchmark from %q, want the household's own best account", report.BenchmarkAccount)
	}
	if len(report.Items) != 1 || report.Items[0].Name != "Checking" {
		t.Fatalf("items = %+v, want just the 0.40%% checking account", report.Items)
	}

	// CLEARING the yield turns the check back off for that account. "I no longer
	// know what this earns" has to be expressible, or the detector keeps
	// reporting against a rate the user has abandoned.
	clear := f.call(f.srv.handleSetDepositAPY, "PUT", "/api/accounts/"+f.checking.String()+"/deposit-apy",
		map[string]any{"deposit_apy": nil}, map[string]string{"accountID": f.checking.String()})
	if clear.Code != http.StatusOK {
		t.Fatalf("clear apy: %d %s", clear.Code, clear.Body.String())
	}

	rec = f.call(f.srv.handleIdleCash, "GET", "/api/accounts/idle-cash", nil, nil)
	var after allocation.IdleCashReport
	if err := json.Unmarshal(rec.Body.Bytes(), &after); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(after.Items) != 0 {
		t.Errorf("items = %+v, want none once the yield is unknown", after.Items)
	}
	if len(after.UnknownYieldAccounts) == 0 {
		t.Error("the account should be named as unmeasured rather than dropped silently")
	}
}

// The yield write refuses a fraction typed where a percent belongs, and refuses
// an account the caller cannot see.
func TestAllocationDepositAPYValidation(t *testing.T) {
	f := newAllocationFixture(t)

	// 450 meaning 4.50% would make every other account look catastrophic.
	bad := f.call(f.srv.handleSetDepositAPY, "PUT", "/api/accounts/"+f.savings.String()+"/deposit-apy",
		map[string]any{"deposit_apy": "450"}, map[string]string{"accountID": f.savings.String()})
	if bad.Code != http.StatusBadRequest {
		t.Errorf("450%% accepted: %d %s", bad.Code, bad.Body.String())
	}

	hidden := f.call(f.srv.handleSetDepositAPY, "PUT", "/api/accounts/"+f.spouseRoth.String()+"/deposit-apy",
		map[string]any{"deposit_apy": "1.00"}, map[string]string{"accountID": f.spouseRoth.String()})
	if hidden.Code != http.StatusNotFound {
		t.Errorf("writing to an invisible account = %d, want 404", hidden.Code)
	}
}
