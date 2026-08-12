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

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/audit"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/testdb"
)

// TestObjectHistory exercises the per-object change log end to end against a
// real Postgres: every wired mutation writes the right field-level rows, those
// rows live in the same transaction as the mutation (a rolled-back edit writes
// nothing), history respects household/private-object visibility, and the
// append-only guard refuses UPDATE and DELETE at the database.
//
//	TEST_DATABASE_URL='postgres://ledgermancy:...@localhost:5433/ledgermancy?sslmode=disable' \
//	  go test ./internal/api/ -run TestObjectHistory
func TestObjectHistory(t *testing.T) {
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

	// One household with two adult members (owner of a private account + a
	// spouse who must not see it), plus a separate household for cross-scope.
	household := uuid.New()
	owner := uuid.New()
	spouse := uuid.New()
	foreignHH := uuid.New()
	foreignUser := uuid.New()

	for _, hh := range []uuid.UUID{household, foreignHH} {
		exec(`INSERT INTO households (id, name) VALUES ($1, $2)`, hh, hh.String())
	}
	for _, u := range []struct{ id, hh uuid.UUID }{
		{owner, household}, {spouse, household}, {foreignUser, foreignHH},
	} {
		exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
		      VALUES ($1, $2, $3, 'x', 'Tester')`, u.id, u.hh, u.id.String()+"@example.test")
	}

	// A shared Plaid account the spouse can see, a private one only the owner
	// can, and a foreign account in the other household.
	sharedItem, sharedAcct := uuid.New(), uuid.New()
	privateItem, privateAcct := uuid.New(), uuid.New()
	foreignItem, foreignAcct := uuid.New(), uuid.New()
	seedPlaidAccount := func(item, acct, u uuid.UUID, isShared bool) {
		exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, products, status, is_shared)
		      VALUES ($1, $2, $3, '\x00', '{transactions}', 'active', $4)`,
			item, u, item.String(), isShared)
		exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type, current_balance)
		      VALUES ($1, $2, $3, 'Checking', 'depository', '100.00')`, acct, item, acct.String())
	}
	seedPlaidAccount(sharedItem, sharedAcct, owner, true)
	seedPlaidAccount(privateItem, privateAcct, owner, false)
	seedPlaidAccount(foreignItem, foreignAcct, foreignUser, true)

	catA, catB := uuid.New(), uuid.New()
	exec(`INSERT INTO categories (id, household_id, name, slug) VALUES ($1, $2, 'Food', 'food')`, catA, household)
	exec(`INSERT INTO categories (id, household_id, name, slug) VALUES ($1, $2, 'Travel', 'travel')`, catB, household)

	// A manual account so the manual-transaction handler paths can round-trip.
	manualAcct := uuid.New()
	exec(`INSERT INTO accounts (id, household_id, user_id, is_shared, name, type, source)
	      VALUES ($1, $2, $3, TRUE, 'Wallet', 'depository', 'manual')`, manualAcct, household, owner)

	// Transactions on each account, categorised as Food to start.
	sharedTx, privateTx, foreignTx := uuid.New(), uuid.New(), uuid.New()
	seedTx := func(id, acct uuid.UUID, amount string) {
		exec(`INSERT INTO transactions (id, account_id, amount, currency, date, name, source, category_id, category_source)
		      VALUES ($1, $2, $3, 'USD', '2026-06-15', 'Coffee', 'plaid', $4, 'manual')`, id, acct, amount, catA)
	}
	seedTx(sharedTx, sharedAcct, "-4.50")
	seedTx(privateTx, privateAcct, "-4.50")
	seedTx(foreignTx, foreignAcct, "-4.50")

	// --- request helpers ---------------------------------------------------
	ownerIdent := auth.Identity{UserID: owner, HouseholdID: household, DisplayName: "Owner", Role: auth.RoleMember}
	spouseIdent := auth.Identity{UserID: spouse, HouseholdID: household, DisplayName: "Spouse", Role: auth.RoleMember}

	reqWith := func(ident auth.Identity, method, target, body string, params ...[2]string) *http.Request {
		r := httptest.NewRequest(method, target, strings.NewReader(body))
		rctx := chi.NewRouteContext()
		for _, p := range params {
			rctx.URLParams.Add(p[0], p[1])
		}
		return r.WithContext(context.WithValue(auth.ContextWithIdentity(ctx, ident), chi.RouteCtxKey, rctx))
	}

	history := func(ident auth.Identity, kind string, objectID uuid.UUID) []objectChangeResponse {
		t.Helper()
		r := reqWith(ident, http.MethodGet, "/api/audit?object_kind="+kind+"&object_id="+objectID.String(), "")
		rec := httptest.NewRecorder()
		srv.handleListObjectChanges(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("list history: status %d, body %s", rec.Code, rec.Body.String())
		}
		var rows []objectChangeResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
			t.Fatalf("decode history: %v", err)
		}
		return rows
	}

	countHistory := func(kind string, objectID uuid.UUID) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM object_changes WHERE object_kind = $1 AND object_id = $2`,
			kind, objectID).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}

	// --- transaction: recategorise writes exactly the changed field --------
	t.Run("recategorise writes one row for category_id", func(t *testing.T) {
		body := `{"category_id":"` + catB.String() + `"}`
		rec := httptest.NewRecorder()
		srv.handleRecategoriseTransaction(rec, reqWith(ownerIdent, http.MethodPatch,
			"/api/transactions/x", body, [2]string{"transactionID", sharedTx.String()}))
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
		}
		rows := history(ownerIdent, "transaction", sharedTx)
		if len(rows) != 1 {
			t.Fatalf("got %d rows, want 1 (only category_id changed): %+v", len(rows), rows)
		}
		if rows[0].Field != "category_id" {
			t.Errorf("field = %q, want category_id", rows[0].Field)
		}
		if string(rows[0].OldValue) != `"`+catA.String()+`"` {
			t.Errorf("old = %s, want %s", rows[0].OldValue, catA)
		}
		if string(rows[0].NewValue) != `"`+catB.String()+`"` {
			t.Errorf("new = %s, want %s", rows[0].NewValue, catB)
		}
		if rows[0].ActorUserID == nil || *rows[0].ActorUserID != owner {
			t.Errorf("actor = %v, want owner", rows[0].ActorUserID)
		}
	})

	// --- transaction: same-transaction (a rolled-back mutation writes nothing)
	t.Run("a rolled-back mutation writes no history", func(t *testing.T) {
		rolledBackObject := uuid.New()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		qtx := srv.Queries.WithTx(tx)
		// Record inside the tx, then throw the tx away — the row must not survive.
		if err := audit.Record(ctx, qtx, audit.RecordParams{
			HouseholdID: household,
			ObjectKind:  audit.KindTransaction,
			ObjectID:    rolledBackObject,
			ActorUserID: owner,
		}, []audit.Change{{Field: "notes", Old: "a", New: "b"}}); err != nil {
			t.Fatalf("record: %v", err)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("rollback: %v", err)
		}
		if n := countHistory("transaction", rolledBackObject); n != 0 {
			t.Errorf("rolled-back edit wrote %d rows; same-tx invariant broken", n)
		}
		// A committed write for a different object must still land — proving the
		// zero above is the rollback, not a broken recorder.
		committedObject := uuid.New()
		if err := srv.Queries.InsertObjectChange(ctx, dbgen.InsertObjectChangeParams{
			HouseholdID: household, ObjectKind: audit.KindTransaction, ObjectID: committedObject,
			ActorUserID: &owner, Field: "notes", OldValue: []byte(`"x"`), NewValue: []byte(`"y"`),
		}); err != nil {
			t.Fatalf("committed seed: %v", err)
		}
		if n := countHistory("transaction", committedObject); n != 1 {
			t.Errorf("committed write should produce 1 row, got %d", n)
		}
	})

	// --- transaction: visibility scoping ----------------------------------
	t.Run("private transaction history is invisible to the spouse", func(t *testing.T) {
		// Owner recategorises the private transaction.
		body := `{"category_id":"` + catB.String() + `"}`
		rec := httptest.NewRecorder()
		srv.handleRecategoriseTransaction(rec, reqWith(ownerIdent, http.MethodPatch,
			"/api/transactions/x", body, [2]string{"transactionID", privateTx.String()}))
		if rec.Code != http.StatusOK {
			t.Fatalf("owner recategorise: status %d, body %s", rec.Code, rec.Body.String())
		}
		// Owner sees the history; the spouse does not (private account).
		if rows := history(ownerIdent, "transaction", privateTx); len(rows) == 0 {
			t.Fatal("owner sees no history on own private transaction")
		}
		if rows := history(spouseIdent, "transaction", privateTx); len(rows) != 0 {
			t.Fatalf("spouse saw %d rows on the owner's private transaction; visibility scoping broken", len(rows))
		}
	})

	t.Run("a foreign transaction's history is invisible across households", func(t *testing.T) {
		if rows := history(spouseIdent, "transaction", foreignTx); len(rows) != 0 {
			t.Fatalf("spouse saw %d rows on a foreign household transaction", len(rows))
		}
	})

	// --- transaction: manual create + update ------------------------------
	t.Run("manual transaction create and edit are both recorded", func(t *testing.T) {
		createBody := `{"account_id":"` + manualAcct.String() + `","date":"2026-06-16","amount":"-12.00","name":"Lunch","category_id":"` + catA.String() + `"}`
		rec := httptest.NewRecorder()
		srv.handleCreateManualTransaction(rec, reqWith(ownerIdent, http.MethodPost, "/api/transactions", createBody))
		if rec.Code != http.StatusCreated {
			t.Fatalf("create: status %d, body %s", rec.Code, rec.Body.String())
		}
		var created struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
			t.Fatalf("decode created: %v", err)
		}
		createdID := uuid.MustParse(created.ID)
		if rows := history(ownerIdent, "transaction", createdID); len(rows) != 1 || rows[0].Field != audit.FieldCreated {
			t.Fatalf("create should record one 'created' row, got %+v", rows)
		}

		// Edit the amount; that should produce an amount diff row (and nothing
		// else, since only amount changed).
		updateBody := `{"account_id":"` + manualAcct.String() + `","date":"2026-06-16","amount":"-15.00","name":"Lunch","category_id":"` + catA.String() + `"}`
		rec = httptest.NewRecorder()
		srv.handleUpdateManualTransaction(rec, reqWith(ownerIdent, http.MethodPut,
			"/api/transactions/x", updateBody, [2]string{"transactionID", createdID.String()}))
		if rec.Code != http.StatusOK {
			t.Fatalf("update: status %d, body %s", rec.Code, rec.Body.String())
		}
		rows := history(ownerIdent, "transaction", createdID)
		var amountRow *objectChangeResponse
		for i := range rows {
			if rows[i].Field == "amount" {
				amountRow = &rows[i]
			}
		}
		if amountRow == nil {
			t.Fatalf("no amount row after edit; got %+v", rows)
		}
		if string(amountRow.OldValue) != `"-12"` || string(amountRow.NewValue) != `"-15"` {
			t.Errorf("amount old→new = %s→%s, want -12→-15", amountRow.OldValue, amountRow.NewValue)
		}
	})

	// --- budget: create then edit -----------------------------------------
	t.Run("budget upsert records create then amount diff", func(t *testing.T) {
		budgetCat := uuid.New()
		exec(`INSERT INTO categories (id, household_id, name, slug) VALUES ($1, $2, 'Groceries', 'groceries')`, budgetCat, household)

		first := httptest.NewRecorder()
		srv.handleCreateBudget(first, reqWith(ownerIdent, http.MethodPost, "/api/budgets",
			`{"category_id":"`+budgetCat.String()+`","amount":"300.00","period":"monthly"}`))
		if first.Code != http.StatusCreated {
			t.Fatalf("create budget: %d %s", first.Code, first.Body.String())
		}
		var budget struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(first.Body.Bytes(), &budget); err != nil {
			t.Fatalf("decode budget: %v", err)
		}
		budgetID := uuid.MustParse(budget.ID)
		if rows := history(ownerIdent, "budget", budgetID); len(rows) != 1 || rows[0].Field != audit.FieldCreated {
			t.Fatalf("first upsert should record 'created', got %+v", rows)
		}

		second := httptest.NewRecorder()
		srv.handleCreateBudget(second, reqWith(ownerIdent, http.MethodPost, "/api/budgets",
			`{"category_id":"`+budgetCat.String()+`","amount":"350.00","period":"monthly"}`))
		if second.Code != http.StatusCreated {
			t.Fatalf("update budget: %d %s", second.Code, second.Body.String())
		}
		rows := history(ownerIdent, "budget", budgetID)
		var amt *objectChangeResponse
		for i := range rows {
			if rows[i].Field == "amount" {
				amt = &rows[i]
			}
		}
		if amt == nil {
			t.Fatalf("no amount diff after second upsert; got %+v", rows)
		}
		if string(amt.OldValue) != `"300"` || string(amt.NewValue) != `"350"` {
			t.Errorf("budget amount old→new = %s→%s, want 300→350", amt.OldValue, amt.NewValue)
		}
	})

	// --- goal: create then edit -------------------------------------------
	t.Run("goal create and edit are both recorded", func(t *testing.T) {
		first := httptest.NewRecorder()
		srv.handleCreateGoal(first, reqWith(ownerIdent, http.MethodPost, "/api/goals",
			`{"scope":"household","kind":"savings","name":"Trip","target_amount":"2000.00","target_date":"2026-12-31"}`))
		if first.Code != http.StatusCreated {
			t.Fatalf("create goal: %d %s", first.Code, first.Body.String())
		}
		// The goal response is a derived shape; pull the id via a generic decode.
		var raw map[string]any
		if err := json.Unmarshal(first.Body.Bytes(), &raw); err != nil {
			t.Fatalf("decode goal: %v", err)
		}
		goalID := uuid.MustParse(raw["id"].(string))
		if rows := history(ownerIdent, "goal", goalID); len(rows) != 1 || rows[0].Field != audit.FieldCreated {
			t.Fatalf("create should record 'created', got %+v", rows)
		}

		second := httptest.NewRecorder()
		srv.handleUpdateGoal(second, reqWith(ownerIdent, http.MethodPut, "/api/goals/x",
			`{"scope":"household","kind":"savings","name":"Trip to Japan","target_amount":"2500.00","target_date":"2026-12-31"}`,
			[2]string{"goalID", goalID.String()}))
		if second.Code != http.StatusOK {
			t.Fatalf("update goal: %d %s", second.Code, second.Body.String())
		}
		rows := history(ownerIdent, "goal", goalID)
		fields := map[string]bool{}
		for _, r := range rows {
			fields[r.Field] = true
		}
		if !fields["name"] || !fields["target_amount"] {
			t.Errorf("edit should record name + target_amount, got %v", fields)
		}
	})

	// --- append-only: UPDATE and DELETE are refused -----------------------
	t.Run("append-only guard refuses UPDATE and DELETE", func(t *testing.T) {
		// sharedTx has a committed row from the recategorise subtest.
		if countHistory("transaction", sharedTx) == 0 {
			t.Fatal("need at least one row to exercise the guard")
		}
		// Postgres fires the BEFORE trigger per matched row, so an unbounded
		// WHERE on existing rows is enough to trip it.
		if _, err := pool.Exec(ctx,
			`UPDATE object_changes SET field = field WHERE object_kind='transaction' AND object_id=$1`, sharedTx); err == nil {
			t.Fatal("UPDATE on object_changes succeeded; append-only guard is missing")
		}
		if _, err := pool.Exec(ctx,
			`DELETE FROM object_changes WHERE object_kind='transaction' AND object_id=$1`, sharedTx); err == nil {
			t.Fatal("DELETE on object_changes succeeded; append-only guard is missing")
		}
	})

	// --- bad request shape ------------------------------------------------
	t.Run("unknown object_kind is rejected", func(t *testing.T) {
		r := reqWith(ownerIdent, http.MethodGet, "/api/audit?object_kind=account&object_id="+sharedTx.String(), "")
		rec := httptest.NewRecorder()
		srv.handleListObjectChanges(rec, r)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
	})
}
