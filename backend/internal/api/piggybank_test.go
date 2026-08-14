package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/testdb"
)

// The piggy bank HTTP surface against a real Postgres.
//
// The cases that matter are the honesty ones: the running balance is the SQL
// sum of the event log (never a stored figure that can drift), a withdrawal
// that would take a jar negative is refused, over-allocating an account across
// jars is a loud warning rather than a silently clipped number, and a piggy
// bank in one household is invisible — and unmovable — from another.
//
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' go test ./internal/api/ -run TestPiggyBank
func TestPiggyBankEndpoints(t *testing.T) {
	url := testdb.URL(t)

	ctx := context.Background()
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	srv := &Server{Pool: pool, Queries: dbgen.New(pool)}

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, sql)
		}
	}

	// Household A: a checking account (the piggy bank funding account), a
	// savings account, and a credit card (a debt — a piggy bank must refuse it).
	householdA, userA, itemA := uuid.New(), uuid.New(), uuid.New()
	checkingA, savingsA, cardA := uuid.New(), uuid.New(), uuid.New()
	// Household B: its own checking account, for the visibility cases.
	householdB, userB, itemB, checkingB := uuid.New(), uuid.New(), uuid.New(), uuid.New()

	exec(`INSERT INTO households (id, name) VALUES ($1, 'Household A')`, householdA)
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
	      VALUES ($1, $2, $3, 'x', 'A')`, userA, householdA, userA.String()+"@example.test")
	exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, products, status)
	      VALUES ($1, $2, $3, '\x00', '{transactions}', 'active')`, itemA, userA, itemA.String())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type, subtype, current_balance)
	      VALUES ($1, $2, $3, 'Checking A', 'depository', 'checking', 1000.00)`,
		checkingA, itemA, checkingA.String())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type, subtype, current_balance)
	      VALUES ($1, $2, $3, 'Savings A', 'depository', 'savings', 5000.00)`,
		savingsA, itemA, savingsA.String())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type, subtype, current_balance)
	      VALUES ($1, $2, $3, 'Card A', 'credit', 'credit card', 500.00)`,
		cardA, itemA, cardA.String())

	exec(`INSERT INTO households (id, name) VALUES ($1, 'Household B')`, householdB)
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
	      VALUES ($1, $2, $3, 'x', 'B')`, userB, householdB, userB.String()+"@example.test")
	exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, products, status)
	      VALUES ($1, $2, $3, '\x00', '{transactions}', 'active')`, itemB, userB, itemB.String())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type, subtype, current_balance)
	      VALUES ($1, $2, $3, 'Checking B', 'depository', 'checking', 200.00)`,
		checkingB, itemB, checkingB.String())

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id IN ($1, $2)`, householdA, householdB)
	})

	callerA := auth.Identity{UserID: userA, HouseholdID: householdA, DisplayName: "A", Role: "adult"}
	callerB := auth.Identity{UserID: userB, HouseholdID: householdB, DisplayName: "B", Role: "adult"}

	// newReq builds a request the way the chi router would: identity in the
	// context AND the URL params populated, so handlers reading chi.URLParam
	// see them. httptest.NewRequest alone leaves the param unset.
	newReq := func(c auth.Identity, method, url string, body string, params map[string]string) *http.Request {
		req := httptest.NewRequest(method, url, strings.NewReader(body))
		rctx := chi.NewRouteContext()
		for k, v := range params {
			rctx.URLParams.Add(k, v)
		}
		return req.WithContext(auth.ContextWithIdentity(
			context.WithValue(ctx, chi.RouteCtxKey, rctx), c))
	}

	createPiggy := func(t *testing.T, c auth.Identity, body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.handleCreatePiggyBank(rec, newReq(c, http.MethodPost, "/api/piggy-banks", body, nil))
		return rec
	}
	deposit := func(t *testing.T, c auth.Identity, id string, body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.handleDepositPiggyBank(rec,
			newReq(c, http.MethodPost, "/api/piggy-banks/"+id+"/deposit", body, map[string]string{"piggyBankID": id}))
		return rec
	}
	withdraw := func(t *testing.T, c auth.Identity, id string, body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.handleWithdrawPiggyBank(rec,
			newReq(c, http.MethodPost, "/api/piggy-banks/"+id+"/withdraw", body, map[string]string{"piggyBankID": id}))
		return rec
	}
	decodePiggy := func(t *testing.T, rec *httptest.ResponseRecorder) piggyBankResponse {
		t.Helper()
		var p piggyBankResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatalf("decode piggy: %v (%s)", err, rec.Body.String())
		}
		return p
	}
	decodeMovement := func(t *testing.T, rec *httptest.ResponseRecorder) piggyTransactionResponse {
		t.Helper()
		var m piggyTransactionResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatalf("decode movement: %v (%s)", err, rec.Body.String())
		}
		return m
	}

	t.Run("create rejects a debt account", func(t *testing.T) {
		rec := createPiggy(t, callerA, `{"name":"Should Fail","account_id":"`+cardA.String()+`"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("create rejects another household's account", func(t *testing.T) {
		rec := createPiggy(t, callerA, `{"name":"Should Fail","account_id":"`+checkingB.String()+`"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
		}
	})

	// A single piggy bank that exercises the full deposit/withdraw cycle. The
	// balance assertions are the reconciliation: every figure the API returns
	// is recomputed independently as the event log's SQL sum.
	t.Run("deposit and withdraw reconcile to the event log", func(t *testing.T) {
		rec := createPiggy(t, callerA, `{"name":"Car Fund","account_id":"`+checkingA.String()+`","target_amount":"2000.00"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create: status %d (%s)", rec.Code, rec.Body.String())
		}
		p := decodePiggy(t, rec)
		if p.CurrentAmount != "0.00" {
			t.Errorf("new piggy current = %s, want 0.00", p.CurrentAmount)
		}
		if p.TargetAmount == nil || *p.TargetAmount != "2000.00" {
			t.Errorf("target = %v, want 2000.00", p.TargetAmount)
		}

		if rec := deposit(t, callerA, p.ID, `{"amount":"100.00"}`); rec.Code != http.StatusCreated {
			t.Fatalf("deposit 100: %d (%s)", rec.Code, rec.Body.String())
		}
		if rec := deposit(t, callerA, p.ID, `{"amount":"50.25"}`); rec.Code != http.StatusCreated {
			t.Fatalf("deposit 50.25: %d (%s)", rec.Code, rec.Body.String())
		}
		// 100 + 50.25 - 30 = 120.25
		wRec := withdraw(t, callerA, p.ID, `{"amount":"30.00"}`)
		if wRec.Code != http.StatusCreated {
			t.Fatalf("withdraw 30: %d (%s)", wRec.Code, wRec.Body.String())
		}
		w := decodeMovement(t, wRec)
		if w.PiggyBank.CurrentAmount != "120.25" {
			t.Errorf("after withdraw current = %s, want 120.25", w.PiggyBank.CurrentAmount)
		}
		if w.Event.Type != "withdraw" {
			t.Errorf("event type = %s, want withdraw", w.Event.Type)
		}

		// The list endpoint reads the same derived figure, so it must agree to
		// the cent with the movement response — no stored value sits between.
		listRec := httptest.NewRecorder()
		srv.handleListPiggyBanks(listRec, newReq(callerA, http.MethodGet, "/api/piggy-banks", "", nil))
		var list []piggyBankResponse
		if err := json.Unmarshal(listRec.Body.Bytes(), &list); err != nil {
			t.Fatalf("list: %v", err)
		}
		var got *piggyBankResponse
		for i := range list {
			if list[i].ID == p.ID {
				got = &list[i]
			}
		}
		if got == nil {
			t.Fatalf("piggy bank missing from list")
		}
		if got.CurrentAmount != "120.25" {
			t.Errorf("list current = %s, want 120.25 (must match movement)", got.CurrentAmount)
		}
	})

	t.Run("withdraw more than the jar holds is refused", func(t *testing.T) {
		rec := createPiggy(t, callerA, `{"name":"Small Jar","account_id":"`+checkingA.String()+`"}`)
		p := decodePiggy(t, rec)
		if rec := deposit(t, callerA, p.ID, `{"amount":"50.00"}`); rec.Code != http.StatusCreated {
			t.Fatalf("deposit: %d (%s)", rec.Code, rec.Body.String())
		}
		// 75 against a 50 balance — hard refused, never a negative balance.
		bad := withdraw(t, callerA, p.ID, `{"amount":"75.00"}`)
		if bad.Code != http.StatusBadRequest {
			t.Fatalf("over-withdraw status = %d, want 400 (%s)", bad.Code, bad.Body.String())
		}
		// The jar is untouched.
		bal := decodeMovement(t, deposit(t, callerA, p.ID, `{"amount":"0.01"}`))
		if bal.PiggyBank.CurrentAmount != "50.01" {
			t.Errorf("after refused withdraw + 0.01, current = %s, want 50.01", bal.PiggyBank.CurrentAmount)
		}
	})

	// THE case this feature exists to catch. Assigning more to piggy banks than
	// the account holds is allowed (the household may intend it, or may be
	// moving money in later) but must be a loud warning, never a silent wrong
	// number. The deposit succeeds; the allocation surface reports negative
	// available and over_allocated = true.
	t.Run("over-allocation is a warning, not a silent wrong number", func(t *testing.T) {
		jar1 := decodePiggy(t, createPiggy(t, callerA, `{"name":"Jar 1","account_id":"`+savingsA.String()+`"}`))
		jar2 := decodePiggy(t, createPiggy(t, callerA, `{"name":"Jar 2","account_id":"`+savingsA.String()+`"}`))

		// Savings A holds 5000. Assign 3000 + 2500 = 5500 across the two jars.
		m1 := decodeMovement(t, deposit(t, callerA, jar1.ID, `{"amount":"3000.00"}`))
		if m1.Allocation.OverAllocated {
			t.Errorf("after first deposit, over_allocated = true, want false (assigned 3000 of 5000)")
		}
		if m1.Allocation.Available != "2000.00" {
			t.Errorf("after first deposit, available = %s, want 2000.00", m1.Allocation.Available)
		}

		m2 := decodeMovement(t, deposit(t, callerA, jar2.ID, `{"amount":"2500.00"}`))
		if !m2.Allocation.OverAllocated {
			t.Errorf("after over-allocation, over_allocated = false, want true")
		}
		if m2.Allocation.Available != "-500.00" {
			t.Errorf("after over-allocation, available = %s, want -500.00", m2.Allocation.Available)
		}
		if m2.Allocation.Assigned != "5500.00" {
			t.Errorf("assigned = %s, want 5500.00", m2.Allocation.Assigned)
		}
		if m2.Allocation.AccountBalance != "5000.00" {
			t.Errorf("account_balance = %s, want 5000.00", m2.Allocation.AccountBalance)
		}

		// The standalone allocation endpoint agrees with the deposit response.
		allocRec := httptest.NewRecorder()
		srv.handleAccountAvailableForPiggy(allocRec, newReq(callerA, http.MethodGet,
			"/api/accounts/"+savingsA.String()+"/available-for-piggy", "",
			map[string]string{"accountID": savingsA.String()}))
		var alloc accountAllocation
		if err := json.Unmarshal(allocRec.Body.Bytes(), &alloc); err != nil {
			t.Fatalf("alloc: %v", err)
		}
		if !alloc.OverAllocated || alloc.Available != "-500.00" {
			t.Errorf("alloc endpoint: over=%v available=%s, want true / -500.00", alloc.OverAllocated, alloc.Available)
		}
	})

	// Visibility: a piggy bank in household A is invisible — and unmovable —
	// from household B. Every write is household-scoped in SQL, so a foreign id
	// reads as 404 rather than as a successful cross-household mutation.
	t.Run("piggy banks are household-scoped", func(t *testing.T) {
		// B's piggy bank.
		pB := decodePiggy(t, createPiggy(t, callerB, `{"name":"B's jar","account_id":"`+checkingB.String()+`"}`))

		// A cannot see it in its list.
		listRec := httptest.NewRecorder()
		srv.handleListPiggyBanks(listRec, newReq(callerA, http.MethodGet, "/api/piggy-banks", "", nil))
		var listA []piggyBankResponse
		_ = json.Unmarshal(listRec.Body.Bytes(), &listA)
		for _, p := range listA {
			if p.ID == pB.ID {
				t.Errorf("household A saw household B's piggy bank in its list")
			}
		}

		// A cannot deposit into it.
		if rec := deposit(t, callerA, pB.ID, `{"amount":"10.00"}`); rec.Code != http.StatusNotFound {
			t.Errorf("cross-household deposit status = %d, want 404", rec.Code)
		}
		// A cannot withdraw from it.
		if rec := withdraw(t, callerA, pB.ID, `{"amount":"10.00"}`); rec.Code != http.StatusNotFound {
			t.Errorf("cross-household withdraw status = %d, want 404", rec.Code)
		}
		// A cannot delete it.
		delRec := httptest.NewRecorder()
		srv.handleDeletePiggyBank(delRec, newReq(callerA, http.MethodDelete,
			"/api/piggy-banks/"+pB.ID, "", map[string]string{"piggyBankID": pB.ID}))
		if delRec.Code != http.StatusNotFound {
			t.Errorf("cross-household delete status = %d, want 404", delRec.Code)
		}

		// B still owns it intact.
		eventsRec := httptest.NewRecorder()
		srv.handleListPiggyBankEvents(eventsRec, newReq(callerB, http.MethodGet,
			"/api/piggy-banks/"+pB.ID+"/events", "", map[string]string{"piggyBankID": pB.ID}))
		if eventsRec.Code != http.StatusOK {
			t.Errorf("B listing own events: %d", eventsRec.Code)
		}
	})

	t.Run("delete removes the piggy bank and its events", func(t *testing.T) {
		p := decodePiggy(t, createPiggy(t, callerA, `{"name":"Doomed","account_id":"`+checkingA.String()+`"}`))
		_ = decodeMovement(t, deposit(t, callerA, p.ID, `{"amount":"25.00"}`))

		delRec := httptest.NewRecorder()
		srv.handleDeletePiggyBank(delRec, newReq(callerA, http.MethodDelete,
			"/api/piggy-banks/"+p.ID, "", map[string]string{"piggyBankID": p.ID}))
		if delRec.Code != http.StatusNoContent {
			t.Fatalf("delete: %d (%s)", delRec.Code, delRec.Body.String())
		}
		// Gone from the list.
		listRec := httptest.NewRecorder()
		srv.handleListPiggyBanks(listRec, newReq(callerA, http.MethodGet, "/api/piggy-banks", "", nil))
		var list []piggyBankResponse
		_ = json.Unmarshal(listRec.Body.Bytes(), &list)
		for _, q := range list {
			if q.ID == p.ID {
				t.Errorf("deleted piggy bank still in list")
			}
		}
		// The events cascaded away — the count of event rows for the bank is 0.
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM piggy_bank_events WHERE piggy_bank_id = $1`, p.ID).Scan(&n); err != nil {
			t.Fatalf("count events: %v", err)
		}
		if n != 0 {
			t.Errorf("orphaned events after delete = %d, want 0", n)
		}
	})
}
