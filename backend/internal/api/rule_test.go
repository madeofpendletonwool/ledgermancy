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

// The rules HTTP surface against a real Postgres.
//
// internal/rules already covers matching, planning and the scoping predicates.
// What this file is for is the boundary — the places a rule could be STORED in a
// state the engine cannot honour, or a verb could describe rows the caller was
// never allowed to read:
//
//   - a rule with no conditions is refused, because storing one would mean
//     actions applied to every transaction in the household;
//   - a rule naming another household's category or tag is refused ON WRITE,
//     rather than saved and discovered as a rule that silently never works;
//   - test and trigger are scoped to the CALLER, so neither the preview's count
//     nor the run's writes can reach the other member's private account;
//   - trigger is idempotent through the API, not just in the engine.
//
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' go test ./internal/api/ -run TestRule
func TestRuleEndpoints(t *testing.T) {
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

	// Household A has two members. Alice owns a SHARED account; Bob owns a
	// PRIVATE one, which no rule Alice runs may reach. Household B is the
	// cross-household boundary.
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

	coffeeA, coffeeB := uuid.New(), uuid.New()
	exec(`INSERT INTO categories (id, household_id, name, slug) VALUES ($1, $2, 'Coffee', 'coffee')`,
		coffeeA, householdA)
	exec(`INSERT INTO categories (id, household_id, name, slug) VALUES ($1, $2, 'Coffee B', 'coffee-b')`,
		coffeeB, householdB)
	tagA := uuid.New()
	exec(`INSERT INTO tags (id, household_id, name) VALUES ($1, $2, 'Caffeine')`, tagA, householdA)

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id IN ($1, $2)`,
			householdA, householdB)
	})

	callerA := auth.Identity{UserID: alice, HouseholdID: householdA, DisplayName: "Alice", Role: "adult"}
	callerB := auth.Identity{UserID: carol, HouseholdID: householdB, DisplayName: "Carol", Role: "adult"}

	seedTx := func(account uuid.UUID, name, amount string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		exec(`INSERT INTO transactions
		        (id, account_id, amount, currency, date, name, merchant_name, merchant_key, source, pending)
		      VALUES ($1, $2, $3, 'USD', '2026-06-01', $4, $4, $5, 'manual', FALSE)`,
			id, account, amount, name, strings.ToLower(name))
		return id
	}

	// One matching charge on the shared account and one on Bob's private one.
	// Worked out by hand: Alice's view of "description contains blue bottle" is
	// exactly the first.
	sharedHit := seedTx(sharedAcct, "SQ *BLUE BOTTLE 0421", "12.50")
	privateHit := seedTx(privateAcct, "SQ *BLUE BOTTLE 7788", "9.00")
	seedTx(sharedAcct, "SAFEWAY #2811", "84.20")

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
	createRule := func(c auth.Identity, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		srv.handleCreateRule(rec, newReq(c, http.MethodPost, "/api/rules", body, nil))
		return rec
	}
	listRules := func(t *testing.T, c auth.Identity) []ruleResponse {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.handleListRules(rec, newReq(c, http.MethodGet, "/api/rules", "", nil))
		var out []ruleResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode rules: %v (%s)", err, rec.Body.String())
		}
		return out
	}
	decodeRule := func(t *testing.T, rec *httptest.ResponseRecorder) ruleResponse {
		t.Helper()
		var rule ruleResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &rule); err != nil {
			t.Fatalf("decode rule: %v (%s)", err, rec.Body.String())
		}
		return rule
	}
	updateRule := func(c auth.Identity, id, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		srv.handleUpdateRule(rec, newReq(c, http.MethodPut, "/api/rules/"+id, body,
			map[string]string{"ruleID": id}))
		return rec
	}
	deleteRule := func(c auth.Identity, id string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		srv.handleDeleteRule(rec, newReq(c, http.MethodDelete, "/api/rules/"+id, "",
			map[string]string{"ruleID": id}))
		return rec
	}
	testRule := func(t *testing.T, c auth.Identity, id string) ruleTestResponse {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.handleTestRule(rec, newReq(c, http.MethodPost, "/api/rules/"+id+"/test", "",
			map[string]string{"ruleID": id}))
		if rec.Code != http.StatusOK {
			t.Fatalf("test status = %d (%s)", rec.Code, rec.Body.String())
		}
		var out ruleTestResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode test: %v (%s)", err, rec.Body.String())
		}
		return out
	}
	triggerRule := func(t *testing.T, c auth.Identity, id string) ruleTriggerResultResponse {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.handleTriggerRule(rec, newReq(c, http.MethodPost, "/api/rules/"+id+"/trigger", "",
			map[string]string{"ruleID": id}))
		if rec.Code != http.StatusOK {
			t.Fatalf("trigger status = %d (%s)", rec.Code, rec.Body.String())
		}
		var out ruleTriggerResultResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode trigger: %v (%s)", err, rec.Body.String())
		}
		return out
	}

	// A well-formed rule body for a given household's category, so a case that
	// is meant to fail on authorisation is not accidentally failing validation
	// first.
	goodRule := func(name string, category uuid.UUID) string {
		return `{"name":"` + name + `",
		         "triggers":[{"type":"description_contains","value":"blue bottle"}],
		         "actions":[{"type":"set_category","value":"` + category.String() + `"}]}`
	}

	// --- Validation on write ----------------------------------------------

	t.Run("a rule needs a name", func(t *testing.T) {
		if rec := createRule(callerA, `{"name":"  ",
			"triggers":[{"type":"description_contains","value":"x"}],
			"actions":[{"type":"set_notes","value":"y"}]}`); rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
		}
	})

	// THE DANGEROUS SHAPE. A rule with no conditions would apply its actions to
	// every transaction in the household. The engine refuses to fire one anyway,
	// but a rule that saves cleanly and then does nothing is its own bug report.
	t.Run("a rule with no conditions is refused", func(t *testing.T) {
		rec := createRule(callerA, `{"name":"Everything",
			"triggers":[],
			"actions":[{"type":"set_category","value":"`+coffeeA.String()+`"}]}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("a rule with no actions is refused", func(t *testing.T) {
		rec := createRule(callerA, `{"name":"Nothing",
			"triggers":[{"type":"description_contains","value":"x"}],
			"actions":[]}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("an unknown condition type is refused", func(t *testing.T) {
		rec := createRule(callerA, `{"name":"Teleport",
			"triggers":[{"type":"teleports","value":"x"}],
			"actions":[{"type":"set_notes","value":"y"}]}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("an unparseable amount is refused", func(t *testing.T) {
		rec := createRule(callerA, `{"name":"Ten dollars",
			"triggers":[{"type":"amount_more","value":"ten"}],
			"actions":[{"type":"set_notes","value":"y"}]}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
		}
	})

	// The check that keeps a rule honest. Without it this saves cleanly, matches
	// transactions, and then refuses every action forever — which reads as "the
	// rule engine is broken" rather than "that category is not mine".
	t.Run("a category from another household is refused on write", func(t *testing.T) {
		rec := createRule(callerA, `{"name":"Foreign category",
			"triggers":[{"type":"description_contains","value":"x"}],
			"actions":[{"type":"set_category","value":"`+coffeeB.String()+`"}]}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "category") {
			t.Fatalf("the message should say which kind of thing was wrong: %s", rec.Body.String())
		}
	})

	t.Run("a tag from another household is refused on write", func(t *testing.T) {
		foreignTag := uuid.New()
		exec(`INSERT INTO tags (id, household_id, name) VALUES ($1, $2, 'Theirs')`,
			foreignTag, householdB)

		rec := createRule(callerA, `{"name":"Foreign tag",
			"triggers":[{"type":"description_contains","value":"x"}],
			"actions":[{"type":"add_tag","value":"`+foreignTag.String()+`"}]}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("an account from another household is refused on write", func(t *testing.T) {
		rec := createRule(callerA, `{"name":"Foreign account",
			"triggers":[{"type":"account_is","value":"`+acctB.String()+`"}],
			"actions":[{"type":"set_notes","value":"y"}]}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
		}
	})

	// --- CRUD --------------------------------------------------------------

	var ruleID string

	t.Run("create round-trips the whole rule", func(t *testing.T) {
		rec := createRule(callerA, `{"name":"Blue Bottle is coffee","priority":10,
			"triggers":[
			  {"type":"description_contains","value":"  Blue Bottle "},
			  {"type":"amount_more","value":"5.000"}
			],
			"actions":[
			  {"type":"set_category","value":"`+coffeeA.String()+`","stop_on_fail":true},
			  {"type":"add_tag","value":"`+tagA.String()+`"},
			  {"type":"append_notes","value":"filed by rule"}
			]}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (%s)", rec.Code, rec.Body.String())
		}
		rule := decodeRule(t, rec)
		ruleID = rule.ID

		if len(rule.Triggers) != 2 || len(rule.Actions) != 3 {
			t.Fatalf("round-tripped %d triggers and %d actions, want 2 and 3",
				len(rule.Triggers), len(rule.Actions))
		}
		// Operands come back NORMALISED, not as typed. The stored value and the
		// value the engine compares are produced by one function, so what the
		// editor reloads is exactly what will match.
		if rule.Triggers[0].Value != "Blue Bottle" {
			t.Fatalf("trigger value = %q, want it trimmed", rule.Triggers[0].Value)
		}
		if rule.Triggers[1].Value != "5" {
			t.Fatalf("amount value = %q, want %q", rule.Triggers[1].Value, "5")
		}
		// Order is load-bearing for actions and must survive the round trip.
		if rule.Actions[0].Type != "set_category" || rule.Actions[2].Type != "append_notes" {
			t.Fatalf("actions came back out of order: %+v", rule.Actions)
		}
		if !rule.Actions[0].StopOnFail {
			t.Fatal("stop_on_fail was dropped")
		}
		if !rule.Active {
			t.Fatal("a new rule should be active unless the client said otherwise")
		}
	})

	t.Run("a new rule is not visible to another household", func(t *testing.T) {
		if rules := listRules(t, callerB); len(rules) != 0 {
			t.Fatalf("household B sees %d of household A's rules", len(rules))
		}
	})

	t.Run("update replaces the condition and action lists", func(t *testing.T) {
		rec := updateRule(callerA, ruleID, `{"name":"Blue Bottle is coffee","active":false,"priority":3,
			"triggers":[{"type":"merchant_is","value":"Blue Bottle Coffee"}],
			"actions":[{"type":"set_notes","value":"rewritten"}]}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
		}
		rule := decodeRule(t, rec)
		// Replace, not merge: the editor is a set of rows the user confirms,
		// and leaving the old conditions behind would mean a rule the user
		// cannot see the whole of.
		if len(rule.Triggers) != 1 || len(rule.Actions) != 1 {
			t.Fatalf("update left %d triggers and %d actions behind",
				len(rule.Triggers), len(rule.Actions))
		}
		if rule.Active {
			t.Fatal("active:false was ignored")
		}

		// Put it back for the verbs below.
		if rec := updateRule(callerA, ruleID, `{"name":"Blue Bottle is coffee","active":true,"priority":10,
			"triggers":[{"type":"description_contains","value":"blue bottle"}],
			"actions":[
			  {"type":"set_category","value":"`+coffeeA.String()+`"},
			  {"type":"add_tag","value":"`+tagA.String()+`"},
			  {"type":"append_notes","value":"filed by rule"}
			]}`); rec.Code != http.StatusOK {
			t.Fatalf("restore: %d (%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("another household cannot edit or delete the rule", func(t *testing.T) {
		// The body is valid FOR HOUSEHOLD B, so the 404 below is the household
		// guard on the rule id and not validation tripping first on a category
		// Carol cannot use.
		if rec := updateRule(callerB, ruleID, goodRule("Hijack", coffeeB)); rec.Code != http.StatusNotFound {
			t.Fatalf("update status = %d, want 404 (%s)", rec.Code, rec.Body.String())
		}
		if rec := deleteRule(callerB, ruleID); rec.Code != http.StatusNotFound {
			t.Fatalf("delete status = %d, want 404 (%s)", rec.Code, rec.Body.String())
		}
		// And it is still there and still intact.
		if rules := listRules(t, callerA); len(rules) != 1 || len(rules[0].Triggers) != 1 {
			t.Fatalf("household A's rule was damaged: %+v", rules)
		}
	})

	// --- Test (dry run) ----------------------------------------------------

	t.Run("test previews without writing", func(t *testing.T) {
		out := testRule(t, callerA, ruleID)

		// RECONCILIATION against the hand-worked answer: Alice's view contains
		// exactly one matching charge, and Bob's private one is not counted —
		// not even as a number.
		if out.Matched != 1 || out.WouldChange != 1 {
			t.Fatalf("matched = %d, would change = %d, want 1 and 1", out.Matched, out.WouldChange)
		}
		if len(out.Matches) != 1 || out.Matches[0].TransactionID != sharedHit.String() {
			t.Fatalf("previewed the wrong rows: %+v", out.Matches)
		}
		if len(out.Matches[0].Changes) != 3 {
			t.Fatalf("changes = %d, want one per action", len(out.Matches[0].Changes))
		}

		// Nothing was written. A tester that quietly applies is worse than no
		// tester at all.
		var count int64
		if err := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM transactions WHERE category_id IS NOT NULL
			   AND id IN ($1, $2)`, sharedHit, privateHit).Scan(&count); err != nil {
			t.Fatalf("count: %v", err)
		}
		if count != 0 {
			t.Fatalf("the dry run categorised %d transactions", count)
		}
	})

	t.Run("test on another household's rule is a 404", func(t *testing.T) {
		rec := httptest.NewRecorder()
		srv.handleTestRule(rec, newReq(callerB, http.MethodPost, "/api/rules/"+ruleID+"/test", "",
			map[string]string{"ruleID": ruleID}))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (%s)", rec.Code, rec.Body.String())
		}
	})

	// --- Trigger (run over existing rows) ----------------------------------

	t.Run("trigger applies to what the caller can see", func(t *testing.T) {
		out := triggerRule(t, callerA, ruleID)
		if out.Changed != 1 {
			t.Fatalf("changed = %d, want 1", out.Changed)
		}

		// The shared charge was filed...
		var categoryID *uuid.UUID
		if err := pool.QueryRow(ctx, `SELECT category_id FROM transactions WHERE id = $1`,
			sharedHit).Scan(&categoryID); err != nil {
			t.Fatalf("read: %v", err)
		}
		if categoryID == nil || *categoryID != coffeeA {
			t.Fatalf("shared charge category = %v, want Coffee", categoryID)
		}

		// ...and Bob's private one was NOT. A member running a rule must not be
		// able to re-file charges they were never allowed to read, even though
		// the rule itself belongs to the household they share.
		if err := pool.QueryRow(ctx, `SELECT category_id FROM transactions WHERE id = $1`,
			privateHit).Scan(&categoryID); err != nil {
			t.Fatalf("read: %v", err)
		}
		if categoryID != nil {
			t.Fatal("a rule run by Alice reached a charge on Bob's private account")
		}
	})

	t.Run("triggering again changes nothing", func(t *testing.T) {
		out := triggerRule(t, callerA, ruleID)
		// Matched stays 1 — the rule still fires — while Changed falls to 0.
		// That pair is idempotence: the rule did not stop matching, it found
		// nothing left to do.
		if out.Matched != 1 {
			t.Fatalf("matched = %d, want the rule to still fire", out.Matched)
		}
		if out.Changed != 0 {
			t.Fatalf("changed = %d on the second run, want 0", out.Changed)
		}

		var notes *string
		var tagCount int
		if err := pool.QueryRow(ctx, `
			SELECT t.notes, (SELECT COUNT(*) FROM transaction_tags tt WHERE tt.transaction_id = t.id)
			FROM transactions t WHERE t.id = $1`, sharedHit).Scan(&notes, &tagCount); err != nil {
			t.Fatalf("read: %v", err)
		}
		// The two ways a rule engine ruins data, asserted through the API.
		if notes == nil || *notes != "filed by rule" {
			t.Fatalf("notes = %v after two runs, want them unchanged", notes)
		}
		if tagCount != 1 {
			t.Fatalf("tags = %d after two runs, want 1", tagCount)
		}
	})

	// --- Delete ------------------------------------------------------------

	t.Run("delete removes the rule and leaves its work behind", func(t *testing.T) {
		if rec := deleteRule(callerA, ruleID); rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204 (%s)", rec.Code, rec.Body.String())
		}
		if rules := listRules(t, callerA); len(rules) != 0 {
			t.Fatalf("rules = %d after delete, want 0", len(rules))
		}
		// What a rule already DID is the household's data now, exactly as if a
		// member had set it by hand. Deleting the rule must not un-file
		// anything.
		var categoryID *uuid.UUID
		if err := pool.QueryRow(ctx, `SELECT category_id FROM transactions WHERE id = $1`,
			sharedHit).Scan(&categoryID); err != nil {
			t.Fatalf("read: %v", err)
		}
		if categoryID == nil {
			t.Fatal("deleting the rule undid its work")
		}
	})

	t.Run("deleting it twice is a 404", func(t *testing.T) {
		if rec := deleteRule(callerA, ruleID); rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (%s)", rec.Code, rec.Body.String())
		}
	})
}
