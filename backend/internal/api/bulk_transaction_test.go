package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/testdb"
)

// The multi-select write surface, against a real Postgres.
//
// A bulk endpoint is a lever long enough to do real damage with one click, so
// the cases that matter are the ones where it could reach further or destroy
// more than the row-at-a-time endpoints it stands in for:
//
//   - an id from another household, or from a private account the caller cannot
//     read, must drop OUT of the set rather than be written to. The selection
//     narrows; it never widens;
//   - "add" must add. A row already carrying other labels keeps them, which is
//     the whole reason bulk tagging is add/remove instead of the single-row
//     endpoint's replace;
//   - "remove" must take off the named tag and nothing else;
//   - the same action applied twice must be safe;
//   - a bulk recategorise must leave the same change-history trail the per-row
//     endpoint does, because doing it in bulk is not a reason to lose the record.
//
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' go test ./internal/api/ -run TestBulk
func TestBulkTransactionEndpoints(t *testing.T) {
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

	// Same shape as the tag tests: household A has a shared account Alice can
	// see and a private one only Bob can; household B is the outer boundary.
	householdA, alice, bob := uuid.New(), uuid.New(), uuid.New()
	sharedItem, privateItem := uuid.New(), uuid.New()
	sharedAcct, privateAcct := uuid.New(), uuid.New()
	householdB, carol, itemB, acctB := uuid.New(), uuid.New(), uuid.New(), uuid.New()

	exec(`INSERT INTO households (id, name) VALUES ($1, 'Bulk A')`, householdA)
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
	      VALUES ($1, $2, $3, 'x', 'Alice')`, alice, householdA, alice.String()+"@example.test")
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
	      VALUES ($1, $2, $3, 'x', 'Bob')`, bob, householdA, bob.String()+"@example.test")

	exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, products, status, is_shared)
	      VALUES ($1, $2, $3, '\x00', '{transactions}', 'active', TRUE)`, sharedItem, alice, sharedItem.String())
	exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, products, status, is_shared)
	      VALUES ($1, $2, $3, '\x00', '{transactions}', 'active', FALSE)`, privateItem, bob, privateItem.String())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type, subtype, current_balance)
	      VALUES ($1, $2, $3, 'Shared Checking', 'depository', 'checking', 1000.00)`,
		sharedAcct, sharedItem, sharedAcct.String())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type, subtype, current_balance)
	      VALUES ($1, $2, $3, 'Bob Private', 'depository', 'checking', 500.00)`,
		privateAcct, privateItem, privateAcct.String())

	exec(`INSERT INTO households (id, name) VALUES ($1, 'Bulk B')`, householdB)
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
	      VALUES ($1, $2, $3, 'x', 'Carol')`, carol, householdB, carol.String()+"@example.test")
	exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, products, status)
	      VALUES ($1, $2, $3, '\x00', '{transactions}', 'active')`, itemB, carol, itemB.String())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type, subtype, current_balance)
	      VALUES ($1, $2, $3, 'Checking B', 'depository', 'checking', 200.00)`,
		acctB, itemB, acctB.String())

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id IN ($1, $2)`,
			householdA, householdB)
	})

	callerA := auth.Identity{UserID: alice, HouseholdID: householdA, DisplayName: "Alice", Role: "adult"}

	day := func(d int) time.Time { return time.Date(2026, 6, d, 0, 0, 0, 0, time.UTC) }

	seedTx := func(account uuid.UUID, date time.Time, merchant, amount string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		exec(`INSERT INTO transactions
		        (id, account_id, amount, currency, date, name, merchant_name, merchant_key, source, pending)
		      VALUES ($1, $2, $3, 'USD', $4, $5, $5, $6, 'manual', FALSE)`,
			id, account, amount, date, merchant, strings.ToLower(merchant))
		return id
	}
	seedTag := func(household uuid.UUID, name string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		exec(`INSERT INTO tags (id, household_id, name) VALUES ($1, $2, $3)`, id, household, name)
		return id
	}

	// tagNamesOn reads a row's labels straight from the join table, so the
	// assertions are about what was stored rather than what a handler echoed.
	tagNamesOn := func(t *testing.T, txID uuid.UUID) []string {
		t.Helper()
		rows, err := pool.Query(ctx,
			`SELECT tg.name FROM transaction_tags tt
			   JOIN tags tg ON tg.id = tt.tag_id
			  WHERE tt.transaction_id = $1`, txID)
		if err != nil {
			t.Fatalf("read tags: %v", err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatalf("scan tag: %v", err)
			}
			out = append(out, name)
		}
		sort.Strings(out)
		return out
	}

	post := func(c auth.Identity, path, body string,
		h func(http.ResponseWriter, *http.Request)) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		h(rec, req.WithContext(auth.ContextWithIdentity(ctx, c)))
		return rec
	}
	changedCount := func(t *testing.T, rec *httptest.ResponseRecorder) int {
		t.Helper()
		var out bulkResultResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode result: %v (%s)", err, rec.Body.String())
		}
		return out.Changed
	}
	idList := func(ids ...uuid.UUID) string {
		quoted := make([]string, 0, len(ids))
		for _, id := range ids {
			quoted = append(quoted, `"`+id.String()+`"`)
		}
		return strings.Join(quoted, ",")
	}

	// --- Tags --------------------------------------------------------------

	t.Run("add labels the whole selection and is idempotent", func(t *testing.T) {
		one := seedTx(sharedAcct, day(2), "Hotel Alpha", "120.00")
		two := seedTx(sharedAcct, day(3), "Hotel Beta", "95.00")
		trip := seedTag(householdA, "Bulk Trip")

		body := `{"transaction_ids":[` + idList(one, two) + `],"tag_ids":["` +
			trip.String() + `"],"action":"add"}`

		rec := post(callerA, "/api/transactions/bulk/tags", body, srv.handleBulkTransactionTags)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
		}
		if got := changedCount(t, rec); got != 2 {
			t.Fatalf("changed = %d, want 2", got)
		}
		for _, id := range []uuid.UUID{one, two} {
			if got := tagNamesOn(t, id); len(got) != 1 || got[0] != "Bulk Trip" {
				t.Fatalf("tags on %s = %v, want [Bulk Trip]", id, got)
			}
		}

		// Pressing the same button twice must not be an error, and must not
		// double-label anything: the composite primary key plus ON CONFLICT DO
		// NOTHING make the second pass a no-op, which is what `changed` reports.
		again := post(callerA, "/api/transactions/bulk/tags", body, srv.handleBulkTransactionTags)
		if again.Code != http.StatusOK {
			t.Fatalf("second status = %d, want 200 (%s)", again.Code, again.Body.String())
		}
		if got := changedCount(t, again); got != 0 {
			t.Fatalf("second changed = %d, want 0", got)
		}
		if got := tagNamesOn(t, one); len(got) != 1 {
			t.Fatalf("tags after replay = %v, want one", got)
		}
	})

	t.Run("add never strips labels already there", func(t *testing.T) {
		// The reason bulk tagging is add/remove rather than the single-row
		// endpoint's replace. "Reimbursable" was put on this row for a reason
		// that has nothing to do with the trip being tagged now.
		row := seedTx(sharedAcct, day(4), "Hotel Gamma", "80.00")
		reimbursable := seedTag(householdA, "Bulk Reimbursable")
		trip := seedTag(householdA, "Bulk Trip Two")
		exec(`INSERT INTO transaction_tags (transaction_id, tag_id) VALUES ($1, $2)`, row, reimbursable)

		body := `{"transaction_ids":["` + row.String() + `"],"tag_ids":["` +
			trip.String() + `"],"action":"add"}`
		if rec := post(callerA, "/api/transactions/bulk/tags", body, srv.handleBulkTransactionTags); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
		}

		got := tagNamesOn(t, row)
		if len(got) != 2 || got[0] != "Bulk Reimbursable" || got[1] != "Bulk Trip Two" {
			t.Fatalf("tags = %v, want both labels", got)
		}
	})

	t.Run("remove takes off only the named tag", func(t *testing.T) {
		row := seedTx(sharedAcct, day(5), "Hotel Delta", "70.00")
		keep := seedTag(householdA, "Bulk Keep")
		drop := seedTag(householdA, "Bulk Drop")
		exec(`INSERT INTO transaction_tags (transaction_id, tag_id) VALUES ($1, $2), ($1, $3)`,
			row, keep, drop)

		body := `{"transaction_ids":["` + row.String() + `"],"tag_ids":["` +
			drop.String() + `"],"action":"remove"}`
		rec := post(callerA, "/api/transactions/bulk/tags", body, srv.handleBulkTransactionTags)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
		}
		if got := changedCount(t, rec); got != 1 {
			t.Fatalf("changed = %d, want 1", got)
		}
		if got := tagNamesOn(t, row); len(got) != 1 || got[0] != "Bulk Keep" {
			t.Fatalf("tags = %v, want [Bulk Keep]", got)
		}
	})

	t.Run("rows the caller cannot see drop out of the selection", func(t *testing.T) {
		// Three ids go in: one Alice owns, one on Bob's PRIVATE account, one in
		// another household entirely. Only the first may be written, and the
		// response must say one rather than three.
		mine := seedTx(sharedAcct, day(6), "Visible", "10.00")
		private := seedTx(privateAcct, day(6), "Bob Private Charge", "20.00")
		foreign := seedTx(acctB, day(6), "Other Household", "30.00")
		tag := seedTag(householdA, "Bulk Scoped")

		body := `{"transaction_ids":[` + idList(mine, private, foreign) + `],"tag_ids":["` +
			tag.String() + `"],"action":"add"}`
		rec := post(callerA, "/api/transactions/bulk/tags", body, srv.handleBulkTransactionTags)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
		}
		if got := changedCount(t, rec); got != 1 {
			t.Fatalf("changed = %d, want 1", got)
		}
		if got := tagNamesOn(t, private); len(got) != 0 {
			t.Fatalf("private row was labelled: %v", got)
		}
		if got := tagNamesOn(t, foreign); len(got) != 0 {
			t.Fatalf("foreign row was labelled: %v", got)
		}
	})

	t.Run("a tag from another household is refused", func(t *testing.T) {
		row := seedTx(sharedAcct, day(7), "Hotel Epsilon", "60.00")
		foreignTag := seedTag(householdB, "Bulk Carol's Tag")

		body := `{"transaction_ids":["` + row.String() + `"],"tag_ids":["` +
			foreignTag.String() + `"],"action":"add"}`
		rec := post(callerA, "/api/transactions/bulk/tags", body, srv.handleBulkTransactionTags)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
		}
		if got := tagNamesOn(t, row); len(got) != 0 {
			t.Fatalf("row was labelled: %v", got)
		}
	})

	t.Run("empty and oversized selections are refused", func(t *testing.T) {
		tag := seedTag(householdA, "Bulk Bounds")

		empty := `{"transaction_ids":[],"tag_ids":["` + tag.String() + `"],"action":"add"}`
		if rec := post(callerA, "/api/transactions/bulk/tags", empty, srv.handleBulkTransactionTags); rec.Code != http.StatusBadRequest {
			t.Fatalf("empty status = %d, want 400 (%s)", rec.Code, rec.Body.String())
		}

		tooMany := make([]uuid.UUID, maxBulkTransactions+1)
		for i := range tooMany {
			tooMany[i] = uuid.New()
		}
		over := `{"transaction_ids":[` + idList(tooMany...) + `],"tag_ids":["` +
			tag.String() + `"],"action":"add"}`
		if rec := post(callerA, "/api/transactions/bulk/tags", over, srv.handleBulkTransactionTags); rec.Code != http.StatusBadRequest {
			t.Fatalf("oversized status = %d, want 400 (%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("an unknown action is refused", func(t *testing.T) {
		row := seedTx(sharedAcct, day(8), "Hotel Zeta", "50.00")
		tag := seedTag(householdA, "Bulk Action")
		body := `{"transaction_ids":["` + row.String() + `"],"tag_ids":["` +
			tag.String() + `"],"action":"set"}`
		if rec := post(callerA, "/api/transactions/bulk/tags", body, srv.handleBulkTransactionTags); rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
		}
	})

	// --- Category ----------------------------------------------------------

	t.Run("recategorise writes the rows and their history", func(t *testing.T) {
		var categoryID uuid.UUID
		if err := pool.QueryRow(ctx,
			`SELECT id FROM categories WHERE household_id IS NULL ORDER BY name LIMIT 1`).
			Scan(&categoryID); err != nil {
			t.Fatalf("read a system category: %v", err)
		}

		one := seedTx(sharedAcct, day(9), "Grocer One", "40.00")
		two := seedTx(sharedAcct, day(10), "Grocer Two", "45.00")
		foreign := seedTx(acctB, day(10), "Grocer B", "55.00")

		body := `{"transaction_ids":[` + idList(one, two, foreign) + `],"category_id":"` +
			categoryID.String() + `"}`
		rec := post(callerA, "/api/transactions/bulk/category", body, srv.handleBulkTransactionCategory)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
		}
		if got := changedCount(t, rec); got != 2 {
			t.Fatalf("changed = %d, want 2 (the foreign row must drop out)", got)
		}

		var source string
		var got uuid.UUID
		if err := pool.QueryRow(ctx,
			`SELECT category_id, category_source FROM transactions WHERE id = $1`, one).
			Scan(&got, &source); err != nil {
			t.Fatalf("read row: %v", err)
		}
		if got != categoryID || source != "manual" {
			t.Fatalf("category = %s/%s, want %s/manual", got, source, categoryID)
		}

		// Doing it in bulk must not lose the audit trail the per-row endpoint
		// writes — a household that finds a category changed can still see who.
		var entries int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM object_changes
			  WHERE object_id = $1 AND field = 'category_id'`, one).Scan(&entries); err != nil {
			t.Fatalf("read history: %v", err)
		}
		if entries != 1 {
			t.Fatalf("history entries = %d, want 1", entries)
		}
	})

	// --- Flags -------------------------------------------------------------

	t.Run("flags apply across the selection and leave the other alone", func(t *testing.T) {
		one := seedTx(sharedAcct, day(11), "Payoff", "900.00")
		two := seedTx(sharedAcct, day(12), "Tax Bill", "800.00")
		exec(`UPDATE transactions SET excluded_from_reports = TRUE WHERE id = $1`, one)

		body := `{"transaction_ids":[` + idList(one, two) + `],"is_one_time":true}`
		rec := post(callerA, "/api/transactions/bulk/flags", body, srv.handleBulkTransactionFlags)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
		}
		if got := changedCount(t, rec); got != 2 {
			t.Fatalf("changed = %d, want 2", got)
		}

		// An omitted flag means "leave alone", so the exclusion set up above has
		// to survive a request that only spoke about is_one_time.
		var oneTime, excluded bool
		if err := pool.QueryRow(ctx,
			`SELECT is_one_time, excluded_from_reports FROM transactions WHERE id = $1`, one).
			Scan(&oneTime, &excluded); err != nil {
			t.Fatalf("read flags: %v", err)
		}
		if !oneTime || !excluded {
			t.Fatalf("flags = one_time %v, excluded %v; want both true", oneTime, excluded)
		}
	})

	t.Run("a request naming no flags is refused", func(t *testing.T) {
		row := seedTx(sharedAcct, day(13), "Nothing To Say", "5.00")
		body := `{"transaction_ids":["` + row.String() + `"]}`
		if rec := post(callerA, "/api/transactions/bulk/flags", body, srv.handleBulkTransactionFlags); rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("a selection of rows nobody can see is a 404", func(t *testing.T) {
		foreign := seedTx(acctB, day(14), "Only Carol's", "15.00")
		body := `{"transaction_ids":["` + foreign.String() + `"],"is_one_time":true}`
		if rec := post(callerA, "/api/transactions/bulk/flags", body, srv.handleBulkTransactionFlags); rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (%s)", rec.Code, rec.Body.String())
		}
	})
}
