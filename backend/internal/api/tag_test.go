package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/testdb"
)

// The tag HTTP surface against a real Postgres.
//
// The cases that matter are the ones where a tag could quietly lie:
//
//   - a tag is HOUSEHOLD data, so household B can neither see nor mutate one of
//     household A's;
//   - a TAGGED TRANSACTION is still per-member data, so labelling a charge on a
//     private account must not leak that charge or its amount to the other
//     member — the tag is visible to both, the money is not;
//   - the by-tag report reconciles with an independent calculation, and does NOT
//     silently become a partition of the month just because it sits beside the
//     by-category panel;
//   - "apply to all from this merchant" ADDS rather than replaces, because a
//     replace would destroy labels somebody else put there.
//
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' go test ./internal/api/ -run TestTag
func TestTagEndpoints(t *testing.T) {
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

	// Household A has two members. Alice owns a SHARED checking account; Bob
	// owns a PRIVATE one, which Alice must never see through any tag surface.
	// Household B is the cross-household boundary.
	householdA, alice, bob := uuid.New(), uuid.New(), uuid.New()
	sharedItem, privateItem := uuid.New(), uuid.New()
	sharedAcct, privateAcct := uuid.New(), uuid.New()
	householdB, carol, itemB, acctB := uuid.New(), uuid.New(), uuid.New(), uuid.New()

	exec(`INSERT INTO households (id, name) VALUES ($1, 'Household A')`, householdA)
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

	exec(`INSERT INTO households (id, name) VALUES ($1, 'Household B')`, householdB)
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
	callerBob := auth.Identity{UserID: bob, HouseholdID: householdA, DisplayName: "Bob", Role: "adult"}
	callerB := auth.Identity{UserID: carol, HouseholdID: householdB, DisplayName: "Carol", Role: "adult"}

	// Dates land inside one window the reporting assertions below share.
	day := func(d int) time.Time {
		return time.Date(2026, 6, d, 0, 0, 0, 0, time.UTC)
	}
	from, to := day(1), day(30)

	// seedTx inserts a posted, categorised spending row. amount is Plaid's
	// convention: positive = money out.
	seedTx := func(account uuid.UUID, date time.Time, merchant, amount string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		exec(`INSERT INTO transactions
		        (id, account_id, amount, currency, date, name, merchant_name, merchant_key, source, pending)
		      VALUES ($1, $2, $3, 'USD', $4, $5, $5, $6, 'manual', FALSE)`,
			id, account, amount, date, merchant, strings.ToLower(merchant))
		return id
	}

	// --- HTTP plumbing -----------------------------------------------------

	newReq := func(c auth.Identity, method, url, body string, params map[string]string) *http.Request {
		req := httptest.NewRequest(method, url, strings.NewReader(body))
		rctx := chi.NewRouteContext()
		for k, v := range params {
			rctx.URLParams.Add(k, v)
		}
		return req.WithContext(auth.ContextWithIdentity(
			context.WithValue(ctx, chi.RouteCtxKey, rctx), c))
	}
	createTag := func(c auth.Identity, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		srv.handleCreateTag(rec, newReq(c, http.MethodPost, "/api/tags", body, nil))
		return rec
	}
	listTags := func(t *testing.T, c auth.Identity) []tagResponse {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.handleListTags(rec, newReq(c, http.MethodGet, "/api/tags", "", nil))
		var out []tagResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode tags: %v (%s)", err, rec.Body.String())
		}
		return out
	}
	decodeTag := func(t *testing.T, rec *httptest.ResponseRecorder) tagResponse {
		t.Helper()
		var tag tagResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &tag); err != nil {
			t.Fatalf("decode tag: %v (%s)", err, rec.Body.String())
		}
		return tag
	}
	setTags := func(c auth.Identity, txID string, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		srv.handleSetTransactionTags(rec, newReq(c, http.MethodPut,
			"/api/transactions/"+txID+"/tags", body, map[string]string{"transactionID": txID}))
		return rec
	}
	byTag := func(t *testing.T, c auth.Identity) []tagSpendResponse {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.handleSpendingByTag(rec, newReq(c, http.MethodGet,
			"/api/reports/by-tag?from="+from.Format(time.DateOnly)+"&to="+to.Format(time.DateOnly),
			"", nil))
		var out []tagSpendResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode by-tag: %v (%s)", err, rec.Body.String())
		}
		return out
	}
	tagIDBody := func(ids ...string) string {
		quoted := make([]string, 0, len(ids))
		for _, id := range ids {
			quoted = append(quoted, `"`+id+`"`)
		}
		return `{"tag_ids":[` + strings.Join(quoted, ",") + `]}`
	}

	// --- CRUD --------------------------------------------------------------

	t.Run("create rejects a blank name", func(t *testing.T) {
		if rec := createTag(callerA, `{"name":"   "}`); rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("create rejects a zero expected amount", func(t *testing.T) {
		// Zero is not a target, it is the absence of one — and that already has
		// its own representation (null). Storing it would render a "full"
		// progress bar for an envelope nobody set.
		rec := createTag(callerA, `{"name":"Zero Target","expected_amount":"0"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("duplicate names collide case-insensitively", func(t *testing.T) {
		rec := createTag(callerA, `{"name":"Reimbursable"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("first create: %d (%s)", rec.Code, rec.Body.String())
		}
		// "reimbursable" is not a second tag — it is the same tag with its total
		// silently split in half, which is the failure the folded unique index
		// exists to prevent.
		dup := createTag(callerA, `{"name":"reimbursable"}`)
		if dup.Code != http.StatusConflict {
			t.Fatalf("duplicate status = %d, want 409 (%s)", dup.Code, dup.Body.String())
		}
		// The SAME name is free in another household.
		other := createTag(callerB, `{"name":"Reimbursable"}`)
		if other.Code != http.StatusCreated {
			t.Fatalf("other household create: %d (%s)", other.Code, other.Body.String())
		}
	})

	t.Run("update and delete are household-scoped", func(t *testing.T) {
		mine := decodeTag(t, createTag(callerA, `{"name":"Doomed","expected_amount":"100.00"}`))

		// Carol has a valid-looking id from another household: every write must
		// read as not-found rather than succeeding.
		upd := httptest.NewRecorder()
		srv.handleUpdateTag(upd, newReq(callerB, http.MethodPut, "/api/tags/"+mine.ID,
			`{"name":"Hijacked"}`, map[string]string{"tagID": mine.ID}))
		if upd.Code != http.StatusNotFound {
			t.Errorf("cross-household update = %d, want 404 (%s)", upd.Code, upd.Body.String())
		}
		del := httptest.NewRecorder()
		srv.handleDeleteTag(del, newReq(callerB, http.MethodDelete, "/api/tags/"+mine.ID,
			"", map[string]string{"tagID": mine.ID}))
		if del.Code != http.StatusNotFound {
			t.Errorf("cross-household delete = %d, want 404", del.Code)
		}

		// Carol cannot see it either.
		for _, tag := range listTags(t, callerB) {
			if tag.ID == mine.ID {
				t.Errorf("household B saw household A's tag in its list")
			}
		}

		// The owner can update it, including clearing the expected amount.
		ok := httptest.NewRecorder()
		srv.handleUpdateTag(ok, newReq(callerA, http.MethodPut, "/api/tags/"+mine.ID,
			`{"name":"Renamed","description":"now with a note"}`,
			map[string]string{"tagID": mine.ID}))
		if ok.Code != http.StatusOK {
			t.Fatalf("own update = %d (%s)", ok.Code, ok.Body.String())
		}
		updated := decodeTag(t, ok)
		if updated.Name != "Renamed" {
			t.Errorf("name = %q, want Renamed", updated.Name)
		}
		if updated.ExpectedAmount != nil {
			t.Errorf("expected_amount = %v, want null after being cleared", *updated.ExpectedAmount)
		}

		// And delete it.
		gone := httptest.NewRecorder()
		srv.handleDeleteTag(gone, newReq(callerA, http.MethodDelete, "/api/tags/"+mine.ID,
			"", map[string]string{"tagID": mine.ID}))
		if gone.Code != http.StatusNoContent {
			t.Fatalf("own delete = %d (%s)", gone.Code, gone.Body.String())
		}
	})

	// --- Labelling ---------------------------------------------------------

	t.Run("a tag from another household cannot be applied", func(t *testing.T) {
		foreign := decodeTag(t, createTag(callerB, `{"name":"Carol's Tag"}`))
		tx := seedTx(sharedAcct, day(3), "Hotel", "100.00")

		rec := setTags(callerA, tx.String(), tagIDBody(foreign.ID))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
		}
		// And nothing was written — including the clear that runs first, so a
		// refused request cannot strip a row's existing labels.
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM transaction_tags WHERE transaction_id = $1`, tx).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 0 {
			t.Errorf("rows written on a refused request = %d, want 0", n)
		}
	})

	t.Run("deleting a tag unlabels transactions but keeps them", func(t *testing.T) {
		tag := decodeTag(t, createTag(callerA, `{"name":"Temporary"}`))
		tx := seedTx(sharedAcct, day(4), "Bookshop", "20.00")
		if rec := setTags(callerA, tx.String(), tagIDBody(tag.ID)); rec.Code != http.StatusOK {
			t.Fatalf("set tags: %d (%s)", rec.Code, rec.Body.String())
		}

		del := httptest.NewRecorder()
		srv.handleDeleteTag(del, newReq(callerA, http.MethodDelete, "/api/tags/"+tag.ID,
			"", map[string]string{"tagID": tag.ID}))
		if del.Code != http.StatusNoContent {
			t.Fatalf("delete: %d (%s)", del.Code, del.Body.String())
		}

		var links, txns int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM transaction_tags WHERE tag_id = $1`, tag.ID).Scan(&links); err != nil {
			t.Fatalf("count links: %v", err)
		}
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM transactions WHERE id = $1`, tx).Scan(&txns); err != nil {
			t.Fatalf("count transactions: %v", err)
		}
		if links != 0 {
			t.Errorf("orphaned links = %d, want 0", links)
		}
		if txns != 1 {
			t.Errorf("transactions after tag delete = %d, want 1 — deleting a label must not delete money", txns)
		}
	})

	t.Run("apply to merchant adds without stripping other tags", func(t *testing.T) {
		trip := decodeTag(t, createTag(callerA, `{"name":"Road Trip"}`))
		reimb := decodeTag(t, createTag(callerA, `{"name":"Expensed"}`))

		// Three charges from one merchant. The second already carries an
		// unrelated label somebody put there for their own reasons.
		first := seedTx(sharedAcct, day(5), "Gas Co", "40.00")
		second := seedTx(sharedAcct, day(6), "Gas Co", "35.00")
		third := seedTx(sharedAcct, day(7), "Gas Co", "25.00")
		if rec := setTags(callerA, second.String(), tagIDBody(reimb.ID)); rec.Code != http.StatusOK {
			t.Fatalf("seed second row's tag: %d (%s)", rec.Code, rec.Body.String())
		}

		rec := setTags(callerA, first.String(),
			`{"tag_ids":["`+trip.ID+`"],"apply_to_merchant":true}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("apply to merchant: %d (%s)", rec.Code, rec.Body.String())
		}

		// Every charge from the merchant now carries the trip tag...
		for _, id := range []uuid.UUID{first, second, third} {
			var n int
			if err := pool.QueryRow(ctx,
				`SELECT count(*) FROM transaction_tags WHERE transaction_id = $1 AND tag_id = $2`,
				id, trip.ID).Scan(&n); err != nil {
				t.Fatalf("count: %v", err)
			}
			if n != 1 {
				t.Errorf("transaction %s carries the applied tag %d times, want 1", id, n)
			}
		}
		// ...and the pre-existing unrelated label on the second row survived.
		// A merchant-wide REPLACE would have wiped it, which is the whole reason
		// this action adds instead.
		var kept int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM transaction_tags WHERE transaction_id = $1 AND tag_id = $2`,
			second, reimb.ID).Scan(&kept); err != nil {
			t.Fatalf("count: %v", err)
		}
		if kept != 1 {
			t.Errorf("pre-existing tag on another member's charge survived = %d, want 1", kept)
		}
	})

	// --- Visibility --------------------------------------------------------

	// THE case this feature could get wrong. A tag is household data, so Alice
	// sees Bob's tag by name. The charges behind it are NOT household data, so
	// Bob's private-account spending must not reach Alice through the tag's
	// count, its total, or the by-tag report.
	t.Run("a tag on a private account leaks neither count nor total", func(t *testing.T) {
		tag := decodeTag(t, createTag(callerBob, `{"name":"Bob Private Project"}`))

		shared := seedTx(sharedAcct, day(10), "Timber Yard", "60.00")
		private := seedTx(privateAcct, day(11), "Private Supplier", "500.00")
		if rec := setTags(callerBob, shared.String(), tagIDBody(tag.ID)); rec.Code != http.StatusOK {
			t.Fatalf("tag shared row: %d (%s)", rec.Code, rec.Body.String())
		}
		if rec := setTags(callerBob, private.String(), tagIDBody(tag.ID)); rec.Code != http.StatusOK {
			t.Fatalf("tag private row: %d (%s)", rec.Code, rec.Body.String())
		}

		findTag := func(list []tagResponse) *tagResponse {
			for i := range list {
				if list[i].ID == tag.ID {
					return &list[i]
				}
			}
			return nil
		}

		// Bob sees both charges behind it: 60 + 500.
		bobView := findTag(listTags(t, callerBob))
		if bobView == nil {
			t.Fatalf("Bob cannot see his own tag")
		}
		if bobView.Total != "560.00" || bobView.TransactionCount != 2 {
			t.Errorf("Bob's view = %s over %d txns, want 560.00 over 2",
				bobView.Total, bobView.TransactionCount)
		}

		// Alice sees the TAG (it is household data) but only the shared charge.
		aliceView := findTag(listTags(t, callerA))
		if aliceView == nil {
			t.Fatalf("Alice cannot see a household tag — tags are household-scoped")
		}
		if aliceView.Total != "60.00" {
			t.Errorf("Alice's total = %s, want 60.00 — the private charge must not leak", aliceView.Total)
		}
		if aliceView.TransactionCount != 1 {
			t.Errorf("Alice's count = %d, want 1", aliceView.TransactionCount)
		}

		// Same split through the report, which is the surface a chart reads.
		findSpend := func(list []tagSpendResponse) *tagSpendResponse {
			for i := range list {
				if list[i].TagID == tag.ID {
					return &list[i]
				}
			}
			return nil
		}
		if row := findSpend(byTag(t, callerA)); row == nil {
			t.Errorf("Alice's by-tag report dropped a tag she can see spending under")
		} else if row.Total.StringFixed(2) != "60.00" {
			t.Errorf("Alice's by-tag total = %s, want 60.00", row.Total.StringFixed(2))
		}
		if row := findSpend(byTag(t, callerBob)); row == nil {
			t.Errorf("Bob's by-tag report dropped his own tag")
		} else if row.Total.StringFixed(2) != "560.00" {
			t.Errorf("Bob's by-tag total = %s, want 560.00", row.Total.StringFixed(2))
		}

		// And the ledger itself does not hand Alice the private row's chip.
		rec := httptest.NewRecorder()
		srv.handleListTransactions(rec, newReq(callerA, http.MethodGet,
			"/api/transactions?from=2026-06-01&to=2026-06-30&tags="+tag.ID, "", nil))
		var listed []transactionResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
			t.Fatalf("decode transactions: %v (%s)", err, rec.Body.String())
		}
		for _, row := range listed {
			if row.ID == private {
				t.Errorf("filtering by a tag surfaced another member's private transaction")
			}
		}
		if len(listed) != 1 || listed[0].ID != shared {
			t.Errorf("tag filter returned %d rows, want just the shared one", len(listed))
		}
		if len(listed) == 1 && (len(listed[0].Tags) != 1 || listed[0].Tags[0].ID != tag.ID) {
			t.Errorf("row's tags = %+v, want the one tag it carries", listed[0].Tags)
		}
	})

	// --- Reporting ---------------------------------------------------------

	// The by-tag report reconciled against an independent calculation: the same
	// window, summed straight from the rows in Go rather than through the
	// reporting query's own predicate.
	//
	// It also pins the property that makes a tag breakdown different from a
	// category one — a charge under two tags is counted under BOTH, so the
	// panel's rows do not sum to the period's spending. That is correct, and a
	// future "fix" that made them sum would be the actual regression.
	t.Run("by-tag reconciles and overlaps deliberately", func(t *testing.T) {
		trip := decodeTag(t, createTag(callerA, `{"name":"Lisbon","expected_amount":"1000.00"}`))
		work := decodeTag(t, createTag(callerA, `{"name":"Work Travel"}`))

		flight := seedTx(sharedAcct, day(12), "Airline", "400.00")
		hotel := seedTx(sharedAcct, day(13), "Hotel Lisboa", "250.00")
		dinner := seedTx(sharedAcct, day(14), "Restaurante", "80.00")
		// Untagged, and inside the window: it must appear under no tag at all.
		seedTx(sharedAcct, day(15), "Corner Shop", "12.00")
		// Tagged but OUTSIDE the window: the report must not reach it.
		outside := seedTx(sharedAcct, day(1).AddDate(0, -1, 0), "Old Charge", "999.00")

		// The flight is BOTH the trip and work travel — the overlap case.
		if rec := setTags(callerA, flight.String(), tagIDBody(trip.ID, work.ID)); rec.Code != http.StatusOK {
			t.Fatalf("tag flight: %d (%s)", rec.Code, rec.Body.String())
		}
		for _, id := range []uuid.UUID{hotel, dinner, outside} {
			if rec := setTags(callerA, id.String(), tagIDBody(trip.ID)); rec.Code != http.StatusOK {
				t.Fatalf("tag %s: %d (%s)", id, rec.Code, rec.Body.String())
			}
		}

		// Independent calculation: sum the seeded amounts directly.
		wantTrip := decimal.RequireFromString("400.00").
			Add(decimal.RequireFromString("250.00")).
			Add(decimal.RequireFromString("80.00")) // 730.00
		wantWork := decimal.RequireFromString("400.00")

		rows := byTag(t, callerA)
		got := map[string]tagSpendResponse{}
		for _, r := range rows {
			got[r.TagID] = r
		}

		tripRow, ok := got[trip.ID]
		if !ok {
			t.Fatalf("by-tag dropped the trip tag")
		}
		if !tripRow.Total.Equal(wantTrip) {
			t.Errorf("trip total = %s, want %s", tripRow.Total, wantTrip)
		}
		if tripRow.TransactionCount != 3 {
			t.Errorf("trip count = %d, want 3 (the out-of-window charge must not count)",
				tripRow.TransactionCount)
		}
		if tripRow.ExpectedAmount == nil || *tripRow.ExpectedAmount != "1000.00" {
			t.Errorf("trip expected = %v, want 1000.00 so the panel can render the envelope",
				tripRow.ExpectedAmount)
		}

		workRow, ok := got[work.ID]
		if !ok {
			t.Fatalf("by-tag dropped the work tag")
		}
		if !workRow.Total.Equal(wantWork) {
			t.Errorf("work total = %s, want %s", workRow.Total, wantWork)
		}

		// The flight is inside both totals. Summing the panel therefore
		// double-counts it, by design — the tag axis is a set of overlapping
		// answers, not a partition of the month.
		var panel decimal.Decimal
		for _, r := range rows {
			panel = panel.Add(r.Total)
		}
		if !panel.GreaterThan(wantTrip) {
			t.Errorf("panel total %s does not exceed the trip's %s — the overlap was lost",
				panel, wantTrip)
		}
	})

	// The untagged filter is the backlog drain, and it must agree with the tag
	// chips the same list returns: a row the ledger says has no tags is exactly
	// a row this filter returns.
	t.Run("untagged filter matches the rows carrying no chips", func(t *testing.T) {
		rec := httptest.NewRecorder()
		srv.handleListTransactions(rec, newReq(callerA, http.MethodGet,
			"/api/transactions?from=2026-06-01&to=2026-06-30&untagged=true&limit=500", "", nil))
		var listed []transactionResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
			t.Fatalf("decode: %v (%s)", err, rec.Body.String())
		}
		if len(listed) == 0 {
			t.Fatalf("untagged filter returned nothing; the fixture seeds untagged rows")
		}
		for _, row := range listed {
			if len(row.Tags) != 0 {
				t.Errorf("row %s came back under ?untagged=true carrying %d tags",
					row.ID, len(row.Tags))
			}
		}
	})
}
