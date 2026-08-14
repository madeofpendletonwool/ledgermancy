package api

import (
	"context"
	"encoding/json"
	"fmt"
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

// The transaction-link HTTP surface against a real Postgres.
//
// The cases that matter are the ones where a link could quietly lie or quietly
// leak:
//
//   - a link names TWO transactions, so both ends are scoped. You cannot link to
//     a row you cannot see, and a link whose far end you cannot see is absent
//     from your reading of the near end — not redacted, absent, because "there
//     is a link to something you cannot see" is itself a fact about that row;
//   - one stored edge is read from both ends, and the two readings must be
//     inverses of each other rather than two rows that can drift;
//   - netting is OPT-IN, subtracts from the ORIGINAL CHARGE's month, ignores
//     links the viewer cannot see, ignores link types that do not net, and never
//     produces negative spending — reconciled here against a total computed in
//     Go from the fixture, not from the same SQL;
//   - the three system link types are readable by every household and writable
//     by none, because `refund` is what the netting query keys on.
//
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' go test ./internal/api/ -run TestTransactionLink
func TestTransactionLinkEndpoints(t *testing.T) {
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

	// Household A has two members. Alice owns a SHARED checking account; Bob owns
	// a PRIVATE one. Household B is the cross-household boundary.
	householdA, alice, bob := uuid.New(), uuid.New(), uuid.New()
	sharedItem, privateItem := uuid.New(), uuid.New()
	sharedAcct, privateAcct := uuid.New(), uuid.New()
	householdB, carol, itemB, acctB := uuid.New(), uuid.New(), uuid.New(), uuid.New()

	exec(`INSERT INTO households (id, name) VALUES ($1, 'Links A')`, householdA)
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

	exec(`INSERT INTO households (id, name) VALUES ($1, 'Links B')`, householdB)
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

	// A system spending category, so the per-category averages (which INNER JOIN
	// categories) have something to group by.
	var foodCategory uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT id FROM categories WHERE slug = 'food-and-drink' AND household_id IS NULL`,
	).Scan(&foodCategory); err != nil {
		t.Fatalf("system category: %v", err)
	}

	// TWO date windows, and the separation is load-bearing. Everything the CRUD
	// and visibility subtests link lands in MAY; the reporting window below is
	// JUNE. Sharing one window would make the netting arithmetic depend on which
	// subtests had run first.
	may := func(d int) time.Time { return time.Date(2026, 5, d, 0, 0, 0, 0, time.UTC) }
	june := func(d int) time.Time { return time.Date(2026, 6, d, 0, 0, 0, 0, time.UTC) }
	from, to := june(1), june(30)

	// seedTx inserts a posted row. amount is Plaid's convention: positive = money
	// out, negative = money in. category may be uuid.Nil for uncategorised.
	seedTx := func(account uuid.UUID, date time.Time, merchant, amount string, category uuid.UUID) uuid.UUID {
		t.Helper()
		id := uuid.New()
		var cat *uuid.UUID
		if category != uuid.Nil {
			cat = &category
		}
		exec(`INSERT INTO transactions
		        (id, account_id, amount, currency, date, name, merchant_name, merchant_key, category_id, source, pending)
		      VALUES ($1, $2, $3, 'USD', $4, $5, $5, $6, $7, 'manual', FALSE)`,
			id, account, amount, date, merchant, strings.ToLower(merchant), cat)
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

	listLinkTypes := func(t *testing.T, c auth.Identity) []linkTypeResponse {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.handleListLinkTypes(rec, newReq(c, http.MethodGet, "/api/link-types", "", nil))
		var out []linkTypeResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode link types: %v (%s)", err, rec.Body.String())
		}
		return out
	}
	linkTypeBySlug := func(t *testing.T, c auth.Identity, slug string) linkTypeResponse {
		t.Helper()
		for _, lt := range listLinkTypes(t, c) {
			if lt.Slug == slug {
				return lt
			}
		}
		t.Fatalf("link type %q not found", slug)
		return linkTypeResponse{}
	}

	createLink := func(c auth.Identity, anchor uuid.UUID, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		srv.handleCreateTransactionLink(rec, newReq(c, http.MethodPost,
			"/api/transactions/"+anchor.String()+"/links", body,
			map[string]string{"transactionID": anchor.String()}))
		return rec
	}
	linkBody := func(other uuid.UUID, typeID, direction string) string {
		return fmt.Sprintf(`{"transaction_id":%q,"link_type_id":%q,"direction":%q}`,
			other, typeID, direction)
	}
	listLinks := func(t *testing.T, c auth.Identity, anchor uuid.UUID) []transactionLinkResponse {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.handleListTransactionLinks(rec, newReq(c, http.MethodGet,
			"/api/transactions/"+anchor.String()+"/links", "",
			map[string]string{"transactionID": anchor.String()}))
		if rec.Code != http.StatusOK {
			t.Fatalf("list links = %d (%s)", rec.Code, rec.Body.String())
		}
		var out []transactionLinkResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode links: %v (%s)", err, rec.Body.String())
		}
		return out
	}
	deleteLink := func(c auth.Identity, anchor uuid.UUID, linkID string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		srv.handleDeleteTransactionLink(rec, newReq(c, http.MethodDelete,
			"/api/transactions/"+anchor.String()+"/links/"+linkID, "",
			map[string]string{"transactionID": anchor.String(), "linkID": linkID}))
		return rec
	}

	// mustLink links two rows and fails the test if the server refuses, for the
	// setup steps where the link itself is not what is under test.
	mustLink := func(t *testing.T, c auth.Identity, anchor, other uuid.UUID, typeID, direction string) {
		t.Helper()
		if rec := createLink(c, anchor, linkBody(other, typeID, direction)); rec.Code != http.StatusCreated {
			t.Fatalf("link = %d, want 201 (%s)", rec.Code, rec.Body.String())
		}
	}

	reportQuery := func(net bool) string {
		return fmt.Sprintf("?from=%s&to=%s&net_refunds=%t",
			from.Format(time.DateOnly), to.Format(time.DateOnly), net)
	}
	trend := func(t *testing.T, c auth.Identity, net bool) trendResponse {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.handleTrend(rec, newReq(c, http.MethodGet, "/api/reports/trend"+reportQuery(net), "", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("trend = %d (%s)", rec.Code, rec.Body.String())
		}
		var out trendResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode trend: %v (%s)", err, rec.Body.String())
		}
		return out
	}
	// juneSpending pulls the one month the fixtures live in. A missing point is a
	// failure rather than a zero: it would mean the window moved.
	juneSpending := func(t *testing.T, c auth.Identity, net bool) trendPoint {
		t.Helper()
		resp := trend(t, c, net)
		if resp.NetRefunds != net {
			t.Fatalf("response net_refunds = %v, want %v", resp.NetRefunds, net)
		}
		for _, p := range resp.Points {
			if p.Month == "2026-06" {
				return p
			}
		}
		t.Fatalf("no 2026-06 point in trend (%+v)", resp.Points)
		return trendPoint{}
	}
	foodAverage := func(t *testing.T, c auth.Identity, net bool) categoryAverageResponse {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.handleCategoryAverages(rec, newReq(c, http.MethodGet,
			"/api/reports/averages"+reportQuery(net), "", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("averages = %d (%s)", rec.Code, rec.Body.String())
		}
		var out []categoryAverageResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode averages: %v (%s)", err, rec.Body.String())
		}
		for _, c := range out {
			if c.CategoryID == foodCategory {
				return c
			}
		}
		t.Fatalf("no food category in averages (%+v)", out)
		return categoryAverageResponse{}
	}
	wantMoney := func(t *testing.T, label string, got decimal.Decimal, want string) {
		t.Helper()
		if !got.Equal(decimal.RequireFromString(want)) {
			t.Errorf("%s = %s, want %s", label, got.StringFixed(2), want)
		}
	}

	// --- Link types --------------------------------------------------------

	t.Run("the three system types ship, and only refund nets", func(t *testing.T) {
		types := listLinkTypes(t, callerA)
		bySlug := map[string]linkTypeResponse{}
		for _, lt := range types {
			bySlug[lt.Slug] = lt
		}
		for _, slug := range []string{"refund", "relates-to", "paid-for"} {
			lt, ok := bySlug[slug]
			if !ok {
				t.Fatalf("missing system link type %q (%+v)", slug, types)
			}
			if !lt.IsSystem {
				t.Errorf("%s is_system = false, want true", slug)
			}
			// nets_spend is what the reporting query keys on. If a second type
			// ever gains it, that is a decision, not an accident — this assertion
			// is where it has to be made deliberately.
			if want := slug == "refund"; lt.NetsSpend != want {
				t.Errorf("%s nets_spend = %v, want %v", slug, lt.NetsSpend, want)
			}
		}
		if refund := bySlug["refund"]; refund.Outward == refund.Inward {
			t.Errorf("refund reads the same from both ends (%q) — direction would be meaningless",
				refund.Outward)
		}
		// "relates to" is symmetric on purpose: the picker offers one option
		// rather than two identical ones.
		if rel := bySlug["relates-to"]; rel.Outward != rel.Inward {
			t.Errorf("relates-to is not symmetric: %q vs %q", rel.Outward, rel.Inward)
		}
	})

	t.Run("system types cannot be edited or deleted by anyone", func(t *testing.T) {
		refund := linkTypeBySlug(t, callerA, "refund")

		upd := httptest.NewRecorder()
		srv.handleUpdateLinkType(upd, newReq(callerA, http.MethodPut,
			"/api/link-types/"+refund.ID, `{"name":"Rebate","outward":"rebates","inward":"is rebated by"}`,
			map[string]string{"linkTypeID": refund.ID}))
		if upd.Code != http.StatusNotFound {
			t.Errorf("update system type = %d, want 404 (%s)", upd.Code, upd.Body.String())
		}

		del := httptest.NewRecorder()
		srv.handleDeleteLinkType(del, newReq(callerA, http.MethodDelete,
			"/api/link-types/"+refund.ID, "", map[string]string{"linkTypeID": refund.ID}))
		if del.Code != http.StatusNotFound {
			t.Errorf("delete system type = %d, want 404 (%s)", del.Code, del.Body.String())
		}

		// Still there, still netting.
		if again := linkTypeBySlug(t, callerA, "refund"); !again.NetsSpend || again.Name != refund.Name {
			t.Errorf("refund type changed: %+v", again)
		}
	})

	t.Run("custom types are household-scoped and never net", func(t *testing.T) {
		rec := httptest.NewRecorder()
		srv.handleCreateLinkType(rec, newReq(callerA, http.MethodPost, "/api/link-types",
			`{"name":"Duplicate of","outward":"duplicates","inward":"is duplicated by"}`, nil))
		if rec.Code != http.StatusCreated {
			t.Fatalf("create = %d (%s)", rec.Code, rec.Body.String())
		}
		var created linkTypeResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
			t.Fatalf("decode: %v", err)
		}
		// A household-defined relationship must never move a reported figure —
		// there is no wording of that checkbox that would make the consequence
		// clear at the moment it was ticked, so it is not offered at all.
		if created.NetsSpend {
			t.Error("a household-created link type nets spending")
		}
		if created.IsSystem {
			t.Error("a household-created link type is marked system")
		}
		if created.Slug == "refund" {
			t.Error("a custom type resolved onto the refund slug")
		}

		// Case-folded uniqueness, and the system types share the namespace.
		dup := httptest.NewRecorder()
		srv.handleCreateLinkType(dup, newReq(callerA, http.MethodPost, "/api/link-types",
			`{"name":"duplicate of","outward":"x","inward":"y"}`, nil))
		if dup.Code != http.StatusConflict {
			t.Errorf("duplicate name = %d, want 409 (%s)", dup.Code, dup.Body.String())
		}
		// Shadowing a SYSTEM name is refused too, and no index can enforce that:
		// a system row's household_id is NULL and a household's is not, so both
		// "Refund"s are distinct keys as far as Postgres is concerned. Two picker
		// entries called "Refund", one of which nets and one of which cannot, is
		// how someone's refunds silently stop netting.
		clash := httptest.NewRecorder()
		srv.handleCreateLinkType(clash, newReq(callerA, http.MethodPost, "/api/link-types",
			`{"name":"refund","outward":"x","inward":"y"}`, nil))
		if clash.Code != http.StatusConflict {
			t.Errorf("shadowing a system name = %d, want 409 (%s)", clash.Code, clash.Body.String())
		}
		// And a RENAME cannot get there either, which is the same hole one step
		// later.
		rename := httptest.NewRecorder()
		srv.handleUpdateLinkType(rename, newReq(callerA, http.MethodPut,
			"/api/link-types/"+created.ID, `{"name":"Refund","outward":"x","inward":"y"}`,
			map[string]string{"linkTypeID": created.ID}))
		if rename.Code != http.StatusConflict {
			t.Errorf("renaming onto a system name = %d, want 409 (%s)", rename.Code, rename.Body.String())
		}

		// Household B can neither see it nor write to it.
		for _, lt := range listLinkTypes(t, callerB) {
			if lt.ID == created.ID {
				t.Error("household B saw household A's link type")
			}
		}
		cross := httptest.NewRecorder()
		srv.handleUpdateLinkType(cross, newReq(callerB, http.MethodPut,
			"/api/link-types/"+created.ID, `{"name":"Hijacked","outward":"x","inward":"y"}`,
			map[string]string{"linkTypeID": created.ID}))
		if cross.Code != http.StatusNotFound {
			t.Errorf("cross-household update = %d, want 404", cross.Code)
		}

		// Blank fields are refused: a relationship with no verb cannot be read
		// from either end.
		blank := httptest.NewRecorder()
		srv.handleCreateLinkType(blank, newReq(callerA, http.MethodPost, "/api/link-types",
			`{"name":"Nameless","outward":"  ","inward":"y"}`, nil))
		if blank.Code != http.StatusBadRequest {
			t.Errorf("blank outward = %d, want 400 (%s)", blank.Code, blank.Body.String())
		}
	})

	// --- Links: CRUD and the two readings of one edge ----------------------

	refundType := linkTypeBySlug(t, callerA, "refund")
	relatesType := linkTypeBySlug(t, callerA, "relates-to")

	t.Run("one edge reads as inverses from its two ends", func(t *testing.T) {
		charge := seedTx(sharedAcct, may(4), "Bike Shop", "200.00", uuid.Nil)
		credit := seedTx(sharedAcct, may(18), "Bike Shop", "-200.00", uuid.Nil)

		// Stated from the credit's end: "this refunds that".
		mustLink(t, callerA, credit, charge, refundType.ID, "outward")

		fromCredit := listLinks(t, callerA, credit)
		if len(fromCredit) != 1 {
			t.Fatalf("credit has %d links, want 1", len(fromCredit))
		}
		if fromCredit[0].Direction != "outward" || fromCredit[0].Relation != refundType.Outward {
			t.Errorf("from credit: direction %q relation %q, want outward/%q",
				fromCredit[0].Direction, fromCredit[0].Relation, refundType.Outward)
		}
		if fromCredit[0].Transaction.ID != charge.String() {
			t.Errorf("from credit: far end = %s, want the charge", fromCredit[0].Transaction.ID)
		}

		fromCharge := listLinks(t, callerA, charge)
		if len(fromCharge) != 1 {
			t.Fatalf("charge has %d links, want 1", len(fromCharge))
		}
		// Same stored row, read from the other end: the inverse verb, and the
		// same id. A second row would be two facts that can disagree.
		if fromCharge[0].ID != fromCredit[0].ID {
			t.Errorf("the two ends report different link ids (%s vs %s)",
				fromCharge[0].ID, fromCredit[0].ID)
		}
		if fromCharge[0].Direction != "inward" || fromCharge[0].Relation != refundType.Inward {
			t.Errorf("from charge: direction %q relation %q, want inward/%q",
				fromCharge[0].Direction, fromCharge[0].Relation, refundType.Inward)
		}
		if fromCharge[0].Transaction.ID != credit.String() {
			t.Errorf("from charge: far end = %s, want the credit", fromCharge[0].Transaction.ID)
		}
		if !fromCharge[0].NetsSpend {
			t.Error("a refund link does not report nets_spend")
		}

		// One link per PAIR, in either direction and regardless of type: a second
		// edge over the same two rows says nothing the first does not, and a
		// second refund edge would net the pair twice.
		again := createLink(callerA, charge, linkBody(credit, refundType.ID, "outward"))
		if again.Code != http.StatusConflict {
			t.Errorf("re-linking the same pair = %d, want 409 (%s)", again.Code, again.Body.String())
		}
		other := createLink(callerA, charge, linkBody(credit, relatesType.ID, "outward"))
		if other.Code != http.StatusConflict {
			t.Errorf("second link type over the same pair = %d, want 409 (%s)",
				other.Code, other.Body.String())
		}

		// Removable from EITHER end, and removing it leaves both transactions.
		if rec := deleteLink(callerA, charge, fromCharge[0].ID); rec.Code != http.StatusNoContent {
			t.Fatalf("delete = %d (%s)", rec.Code, rec.Body.String())
		}
		if n := len(listLinks(t, callerA, credit)); n != 0 {
			t.Errorf("credit still has %d links after unlink", n)
		}
		for _, id := range []uuid.UUID{charge, credit} {
			var count int
			if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM transactions WHERE id = $1`, id).Scan(&count); err != nil {
				t.Fatalf("count: %v", err)
			}
			if count != 1 {
				t.Errorf("transaction %s did not survive the unlink", id)
			}
		}
		// A second delete is a 404, not a silent success.
		if rec := deleteLink(callerA, charge, fromCharge[0].ID); rec.Code != http.StatusNotFound {
			t.Errorf("delete twice = %d, want 404", rec.Code)
		}
	})

	t.Run("a transaction cannot be linked to itself", func(t *testing.T) {
		lonely := seedTx(sharedAcct, may(6), "Solo", "10.00", uuid.Nil)
		rec := createLink(callerA, lonely, linkBody(lonely, refundType.ID, "outward"))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("self-link = %d, want 400 (%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("an unknown link type is refused", func(t *testing.T) {
		a := seedTx(sharedAcct, may(7), "A", "10.00", uuid.Nil)
		b := seedTx(sharedAcct, may(8), "B", "10.00", uuid.Nil)
		rec := createLink(callerA, a, linkBody(b, uuid.New().String(), "outward"))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("unknown link type = %d, want 400 (%s)", rec.Code, rec.Body.String())
		}
		// Another household's custom type is equally unknown here.
		mine := httptest.NewRecorder()
		srv.handleCreateLinkType(mine, newReq(callerB, http.MethodPost, "/api/link-types",
			`{"name":"Carol only","outward":"x","inward":"y"}`, nil))
		var carolType linkTypeResponse
		if err := json.Unmarshal(mine.Body.Bytes(), &carolType); err != nil {
			t.Fatalf("decode: %v", err)
		}
		cross := createLink(callerA, a, linkBody(b, carolType.ID, "outward"))
		if cross.Code != http.StatusBadRequest {
			t.Errorf("another household's type = %d, want 400 (%s)", cross.Code, cross.Body.String())
		}
	})

	// --- Visibility --------------------------------------------------------

	t.Run("you cannot link to a transaction you cannot see", func(t *testing.T) {
		mine := seedTx(sharedAcct, may(9), "Mine", "40.00", uuid.Nil)
		bobsPrivate := seedTx(privateAcct, may(9), "Bobs", "40.00", uuid.Nil)
		carols := seedTx(acctB, may(9), "Carols", "40.00", uuid.Nil)

		// Same household, private account: Alice cannot reach it.
		if rec := createLink(callerA, mine, linkBody(bobsPrivate, refundType.ID, "outward")); rec.Code != http.StatusNotFound {
			t.Errorf("link to a private row = %d, want 404 (%s)", rec.Code, rec.Body.String())
		}
		// Across households.
		if rec := createLink(callerA, mine, linkBody(carols, refundType.ID, "outward")); rec.Code != http.StatusNotFound {
			t.Errorf("cross-household link = %d, want 404 (%s)", rec.Code, rec.Body.String())
		}
		// And Carol cannot use Alice's row as an ANCHOR either.
		if rec := createLink(callerB, mine, linkBody(carols, refundType.ID, "outward")); rec.Code != http.StatusNotFound {
			t.Errorf("cross-household anchor = %d, want 404 (%s)", rec.Code, rec.Body.String())
		}

		var links int
		if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM transaction_links
		    WHERE source_transaction_id = $1 OR target_transaction_id = $1`, mine).Scan(&links); err != nil {
			t.Fatalf("count links: %v", err)
		}
		if links != 0 {
			t.Errorf("%d links were written despite every request being refused", links)
		}
	})

	t.Run("a link to a private row is absent for the other member", func(t *testing.T) {
		shared := seedTx(sharedAcct, may(11), "Shared Charge", "60.00", uuid.Nil)
		bobsPrivate := seedTx(privateAcct, may(12), "Bobs Credit", "-60.00", uuid.Nil)

		// Bob can see both, so Bob can make the link.
		mustLink(t, callerBob, bobsPrivate, shared, refundType.ID, "outward")
		if n := len(listLinks(t, callerBob, shared)); n != 1 {
			t.Fatalf("Bob sees %d links on the shared charge, want 1", n)
		}

		// Alice can see the shared charge but not the far end. The link is ABSENT
		// rather than redacted: a placeholder would still say "there is something
		// on Bob's private account", which is the fact being protected.
		if got := listLinks(t, callerA, shared); len(got) != 0 {
			t.Errorf("Alice saw %d links into a private account: %+v", len(got), got)
		}
	})

	// --- Netting -----------------------------------------------------------

	// June is this section's own window. Every figure below is reconciled against
	// a total computed here, in Go, from the fixture — never against a second
	// reading of the same SQL.
	t.Run("netting is opt-in and subtracts from the original charge", func(t *testing.T) {
		charge := seedTx(sharedAcct, june(5), "Kitchen Store", "200.00", foodCategory)
		seedTx(sharedAcct, june(10), "Cafe", "50.00", foodCategory)
		credit := seedTx(sharedAcct, june(20), "Kitchen Store", "-80.00", foodCategory)

		// Independent computation. Un-netted, a refund is not is_spend and counts
		// in nothing, so June is 200 + 50; the credit is invisible to spending.
		gross := decimal.RequireFromString("250.00")
		netted := decimal.RequireFromString("170.00") // (200 - 80) + 50

		before := juneSpending(t, callerA, false)
		wantMoney(t, "un-netted June spending", before.Spending, gross.StringFixed(2))
		// Asking to net BEFORE any link exists must change nothing: netting
		// honours links, it does not go looking for refunds.
		noLinks := juneSpending(t, callerA, true)
		wantMoney(t, "netted June spending with no links", noLinks.Spending, gross.StringFixed(2))

		mustLink(t, callerA, credit, charge, refundType.ID, "outward")

		// The default is unchanged. This is the whole contract: a link is a
		// statement, and the figures every existing consumer reads do not move
		// because someone made one.
		still := juneSpending(t, callerA, false)
		wantMoney(t, "un-netted June spending after linking", still.Spending, gross.StringFixed(2))

		after := juneSpending(t, callerA, true)
		wantMoney(t, "netted June spending", after.Spending, netted.StringFixed(2))
		// The refund comes off the CHARGE's month. June is the only month here, so
		// the check that matters is that nothing leaked into a neighbour.
		for _, p := range trend(t, callerA, true).Points {
			if p.Month != "2026-06" && !p.Spending.IsZero() {
				t.Errorf("netting put %s of spending into %s", p.Spending.StringFixed(2), p.Month)
			}
		}
		// The stacked bar stays a decomposition of the headline.
		if sum := after.FixedSpending.Add(after.DiscretionarySpending); !sum.Equal(after.Spending) {
			t.Errorf("fixed + discretionary = %s, want %s", sum.StringFixed(2), after.Spending.StringFixed(2))
		}
		// Income is untouched: a credit is not income, and netting must not turn
		// it into one.
		if !after.Income.Equal(before.Income) {
			t.Errorf("netting moved income from %s to %s",
				before.Income.StringFixed(2), after.Income.StringFixed(2))
		}

		// The same arithmetic through the averages endpoint, which is the one the
		// planning figures come from.
		grossAvg := foodAverage(t, callerA, false)
		wantMoney(t, "un-netted food total", grossAvg.Total, gross.StringFixed(2))
		nettedAvg := foodAverage(t, callerA, true)
		wantMoney(t, "netted food total", nettedAvg.Total, netted.StringFixed(2))
		// The count is a fact about the CHARGES, not the money: a refunded charge
		// still happened, so netting must not quietly reduce it.
		if nettedAvg.TransactionCount != grossAvg.TransactionCount {
			t.Errorf("netting changed the transaction count from %d to %d",
				grossAvg.TransactionCount, nettedAvg.TransactionCount)
		}

		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), `DELETE FROM transactions WHERE id IN ($1, $2)`,
				charge, credit)
		})
	})

	t.Run("netting ignores links that do not net, and refunds nobody can see", func(t *testing.T) {
		charge := seedTx(sharedAcct, june(6), "Hardware", "300.00", foodCategory)
		related := seedTx(sharedAcct, june(7), "Hardware", "-90.00", foodCategory)
		bobsCredit := seedTx(privateAcct, june(8), "Hardware", "-120.00", foodCategory)

		base := juneSpending(t, callerA, true).Spending

		// A "relates to" link is not a refund and must move nothing, even though
		// the far end is an inflow of exactly the shape a refund has.
		mustLink(t, callerA, related, charge, relatesType.ID, "outward")
		if got := juneSpending(t, callerA, true).Spending; !got.Equal(base) {
			t.Errorf("a relates-to link moved netted spending from %s to %s",
				base.StringFixed(2), got.StringFixed(2))
		}

		// A refund on Bob's PRIVATE account, linked to the shared charge. Bob's
		// own netted view may honour it; Alice's must not — otherwise the amount
		// of a private credit is readable by subtraction.
		mustLink(t, callerBob, bobsCredit, charge, refundType.ID, "outward")
		if got := juneSpending(t, callerA, true).Spending; !got.Equal(base) {
			t.Errorf("a private refund moved Alice's netted spending from %s to %s — that leaks its amount",
				base.StringFixed(2), got.StringFixed(2))
		}

		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), `DELETE FROM transactions WHERE id IN ($1, $2, $3)`,
				charge, related, bobsCredit)
		})
	})

	t.Run("an over-refunded charge nets to zero, never below", func(t *testing.T) {
		charge := seedTx(sharedAcct, june(12), "Returns Desk", "40.00", foodCategory)
		credit := seedTx(sharedAcct, june(13), "Returns Desk", "-75.00", foodCategory)

		base := juneSpending(t, callerA, true).Spending
		mustLink(t, callerA, credit, charge, refundType.ID, "outward")

		after := juneSpending(t, callerA, true).Spending
		// The charge contributed 40 before the link and must contribute exactly 0
		// after — not -35, which is the artifact netting exists to remove.
		if want := base.Sub(decimal.RequireFromString("40.00")); !after.Equal(want) {
			t.Errorf("netted spending = %s, want %s", after.StringFixed(2), want.StringFixed(2))
		}
		if after.IsNegative() {
			t.Errorf("netting produced negative spending: %s", after.StringFixed(2))
		}

		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), `DELETE FROM transactions WHERE id IN ($1, $2)`,
				charge, credit)
		})
	})
}
