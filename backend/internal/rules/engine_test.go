package rules

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/testdb"
)

// The engine against a real Postgres.
//
// rules_test.go covers matching and planning, which are pure. What needs a
// database is everything the SQL is responsible for, and each of these is a way
// the engine could quietly do damage:
//
//   - the STICKY-MANUAL predicate, asserted against the statement itself rather
//     than only against the Go check in front of it;
//   - VISIBILITY, in both directions: a member running a rule must not touch a
//     charge on the other member's private account, and a household's rules must
//     never reach another household's rows;
//   - IDEMPOTENCE end to end — a second run changes nothing, appends no note
//     twice, adds no duplicate tag;
//   - RECONCILIATION: the engine's match count against the same set worked out
//     by hand from the seeded rows.
//
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' go test ./internal/rules/
func TestEngineAgainstDatabase(t *testing.T) {
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
	q := dbgen.New(pool)

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, sql)
		}
	}

	// Household A has two members: Alice owns a SHARED account, Bob a PRIVATE
	// one. Household B is the cross-household boundary.
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

	coffee, groceries := uuid.New(), uuid.New()
	exec(`INSERT INTO categories (id, household_id, name, slug) VALUES ($1, $2, 'Coffee', 'coffee')`,
		coffee, householdA)
	exec(`INSERT INTO categories (id, household_id, name, slug) VALUES ($1, $2, 'Groceries', 'groceries')`,
		groceries, householdA)
	caffeine := uuid.New()
	exec(`INSERT INTO tags (id, household_id, name) VALUES ($1, $2, 'Caffeine')`, caffeine, householdA)

	// seedTx inserts a posted transaction. amount follows the schema's
	// convention: positive = money out.
	seedTx := func(account uuid.UUID, name, amount string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		exec(`INSERT INTO transactions
		        (id, account_id, amount, currency, date, name, merchant_name, merchant_key, source, pending)
		      VALUES ($1, $2, $3, 'USD', '2026-06-01', $4, $4, $5, 'manual', FALSE)`,
			id, account, amount, name, strings.ToLower(name))
		return id
	}

	// The seeded world, and the hand-worked answer the engine must reconcile
	// against. The rule under test is "description contains 'blue bottle' AND
	// amount more than 5".
	sharedBig := seedTx(sharedAcct, "SQ *BLUE BOTTLE 0421", "12.50")  // matches
	sharedSmall := seedTx(sharedAcct, "SQ *BLUE BOTTLE 0099", "3.00") // too small
	seedTx(sharedAcct, "SAFEWAY #2811", "84.20")                      // wrong description
	privateBig := seedTx(privateAcct, "SQ *BLUE BOTTLE 7788", "9.00") // matches, but Bob's
	seedTx(acctB, "SQ *BLUE BOTTLE 0001", "40.00")                    // matches, but household B

	// By hand: of household A's five-ish rows, exactly two satisfy both
	// conditions — one on the shared account and one on Bob's private one.
	wantMatchesHousehold := []uuid.UUID{sharedBig, privateBig}
	wantMatchesAlice := []uuid.UUID{sharedBig}

	// --- The rule, written through the same tables the API writes ----------

	ruleID := uuid.New()
	exec(`INSERT INTO rules (id, household_id, name, active, priority)
	      VALUES ($1, $2, 'Blue Bottle is coffee', TRUE, 10)`, ruleID, householdA)
	exec(`INSERT INTO rule_triggers (rule_id, trigger_type, value, position)
	      VALUES ($1, 'description_contains', 'blue bottle', 0)`, ruleID)
	exec(`INSERT INTO rule_triggers (rule_id, trigger_type, value, position)
	      VALUES ($1, 'amount_more', '5', 1)`, ruleID)
	exec(`INSERT INTO rule_actions (rule_id, action_type, value, position)
	      VALUES ($1, 'set_category', $2, 0)`, ruleID, coffee.String())
	exec(`INSERT INTO rule_actions (rule_id, action_type, value, position)
	      VALUES ($1, 'add_tag', $2, 1)`, ruleID, caffeine.String())
	exec(`INSERT INTO rule_actions (rule_id, action_type, value, position)
	      VALUES ($1, 'append_notes', 'filed by rule', 2)`, ruleID)

	// An inactive rule that would tag everything, to prove `active` is honoured
	// by the load rather than by luck.
	inactiveID := uuid.New()
	exec(`INSERT INTO rules (id, household_id, name, active, priority)
	      VALUES ($1, $2, 'Off', FALSE, 99)`, inactiveID, householdA)
	exec(`INSERT INTO rule_triggers (rule_id, trigger_type, value, position)
	      VALUES ($1, 'description_contains', '', 0)`, inactiveID)
	exec(`INSERT INTO rule_actions (rule_id, action_type, value, position)
	      VALUES ($1, 'set_category', $2, 0)`, inactiveID, groceries.String())

	// --- Loading ----------------------------------------------------------

	t.Run("load reads active rules with their conditions", func(t *testing.T) {
		engine, err := Load(ctx, q, householdA)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if len(engine.Rules()) != 1 {
			t.Fatalf("rules = %d, want 1 — the inactive rule must not load", len(engine.Rules()))
		}
		rule := engine.Rules()[0]
		if len(rule.Triggers) != 2 || len(rule.Actions) != 3 {
			t.Fatalf("rule has %d triggers and %d actions, want 2 and 3",
				len(rule.Triggers), len(rule.Actions))
		}
		// Actions are ordered, and the order is what makes them meaningful.
		if rule.Actions[0].Type != ActionSetCategory || rule.Actions[2].Type != ActionAppendNotes {
			t.Fatalf("actions came back out of order: %v", rule.Actions)
		}
	})

	t.Run("load rule reads an inactive one by id", func(t *testing.T) {
		// Testing a rule before switching it on is exactly when this is wanted.
		engine, err := LoadRule(ctx, q, householdA, inactiveID)
		if err != nil {
			t.Fatalf("load rule: %v", err)
		}
		if engine == nil || len(engine.Rules()) != 1 {
			t.Fatal("an inactive rule must still load by id")
		}
	})

	t.Run("a rule id from another household is not found", func(t *testing.T) {
		engine, err := LoadRule(ctx, q, householdB, ruleID)
		if err != nil {
			t.Fatalf("load rule: %v", err)
		}
		if engine != nil {
			t.Fatal("household B loaded household A's rule")
		}
	})

	// --- The two candidate reads must agree -------------------------------

	t.Run("the batch and single reads produce the same snapshot", func(t *testing.T) {
		// ListRuleCandidates and GetRuleCandidate are two statements with the
		// same columns. The day they drift, the sync path and the create hook
		// silently disagree about what a rule sees, so this asserts them equal
		// rather than trusting the duplication.
		one, err := q.GetRuleCandidate(ctx, dbgen.GetRuleCandidateParams{
			ID: sharedBig, HouseholdID: householdA,
		})
		if err != nil {
			t.Fatalf("get candidate: %v", err)
		}
		page, err := q.ListRuleCandidates(ctx, dbgen.ListRuleCandidatesParams{
			HouseholdID: householdA, Lim: 500,
		})
		if err != nil {
			t.Fatalf("list candidates: %v", err)
		}
		var found bool
		for _, row := range page {
			if row.ID != sharedBig {
				continue
			}
			found = true
			a, b := FromCandidateRow(row), FromSingleRow(one)
			if a.Name != b.Name || a.AccountID != b.AccountID ||
				!a.Amount.Equal(b.Amount) || a.Notes != b.Notes ||
				a.HasAttachments != b.HasAttachments {
				t.Fatalf("snapshots differ:\n batch  = %+v\n single = %+v", a, b)
			}
		}
		if !found {
			t.Fatal("the batch read did not return the transaction the single read did")
		}
	})

	// --- Scoping ----------------------------------------------------------

	t.Run("a member's run never touches the other member's private rows", func(t *testing.T) {
		engine, err := Load(ctx, q, householdA)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		summary, matches, err := engine.PreviewHousehold(ctx, q, householdA, &alice, 100)
		if err != nil {
			t.Fatalf("preview: %v", err)
		}
		// RECONCILIATION against the hand-worked answer: Alice sees the shared
		// match and nothing of Bob's, whatever the rule says.
		assertMatchIDs(t, matches, wantMatchesAlice)
		if summary.Matched != len(wantMatchesAlice) {
			t.Fatalf("matched = %d, want %d", summary.Matched, len(wantMatchesAlice))
		}
	})

	t.Run("the system run sees the whole household", func(t *testing.T) {
		engine, err := Load(ctx, q, householdA)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		_, matches, err := engine.PreviewHousehold(ctx, q, householdA, nil, 100)
		if err != nil {
			t.Fatalf("preview: %v", err)
		}
		// A sync has nobody to scope to, so Bob's private charge is filed too —
		// and household B's identical charge still is not.
		assertMatchIDs(t, matches, wantMatchesHousehold)
	})

	t.Run("a preview writes nothing", func(t *testing.T) {
		// Everything above ran a preview. If any of it wrote, the run below
		// would report zero changes for the wrong reason and every idempotence
		// assertion after it would be vacuous.
		var count int64
		if err := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM transactions WHERE category_id IS NOT NULL
			   AND id = ANY($1::uuid[])`, wantMatchesHousehold).Scan(&count); err != nil {
			t.Fatalf("count: %v", err)
		}
		if count != 0 {
			t.Fatalf("%d transactions were categorised by a dry run", count)
		}
	})

	// --- The run, and running it again ------------------------------------

	t.Run("the run applies every action", func(t *testing.T) {
		engine, err := Load(ctx, q, householdA)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		summary, err := engine.RunHousehold(ctx, q, householdA, nil)
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if summary.Changed != len(wantMatchesHousehold) {
			t.Fatalf("changed = %d, want %d", summary.Changed, len(wantMatchesHousehold))
		}

		row := readTx(t, ctx, pool, sharedBig)
		if row.categoryID == nil || *row.categoryID != coffee {
			t.Fatalf("category = %v, want Coffee", row.categoryID)
		}
		if row.categorySource != "rule" {
			t.Fatalf("category source = %q, want \"rule\"", row.categorySource)
		}
		if row.notes != "filed by rule" {
			t.Fatalf("notes = %q, want %q", row.notes, "filed by rule")
		}
		if row.tagCount != 1 {
			t.Fatalf("tags = %d, want 1", row.tagCount)
		}

		// The rows that did not match are untouched. A rule that also filed the
		// small charge would be a rule nobody could reason about.
		if untouched := readTx(t, ctx, pool, sharedSmall); untouched.categoryID != nil {
			t.Fatal("a transaction that did not match was categorised anyway")
		}
	})

	t.Run("running it again changes nothing", func(t *testing.T) {
		engine, err := Load(ctx, q, householdA)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		summary, err := engine.RunHousehold(ctx, q, householdA, nil)
		if err != nil {
			t.Fatalf("re-run: %v", err)
		}
		// Matched stays high — the rule still fires — while Changed falls to
		// zero. That pair IS idempotence: the rule did not stop matching, it
		// found nothing left to do.
		if summary.Matched != len(wantMatchesHousehold) {
			t.Fatalf("matched = %d, want the rule to still fire on %d",
				summary.Matched, len(wantMatchesHousehold))
		}
		if summary.Changed != 0 {
			t.Fatalf("changed = %d on the second run, want 0", summary.Changed)
		}

		row := readTx(t, ctx, pool, sharedBig)
		// The two ways a rule engine ruins data, asserted directly.
		if row.notes != "filed by rule" {
			t.Fatalf("notes grew on the second run: %q", row.notes)
		}
		if row.tagCount != 1 {
			t.Fatalf("tags = %d after two runs, want 1", row.tagCount)
		}
	})

	// --- The sticky-manual invariant, at the SQL layer --------------------

	t.Run("the statement itself refuses to overwrite a manual category", func(t *testing.T) {
		// The engine checks this in Go and reports a reason, which is what the
		// user sees. This asserts the PREDICATE, by calling the statement
		// directly — so the guarantee survives a future caller that forgets the
		// Go check, which is the whole reason it lives in the WHERE clause.
		exec(`UPDATE transactions SET category_id = $2, category_source = 'manual' WHERE id = $1`,
			sharedSmall, groceries)

		rows, err := q.ApplyRuleCategory(ctx, dbgen.ApplyRuleCategoryParams{
			ID: sharedSmall, CategoryID: &coffee, HouseholdID: householdA,
		})
		if err != nil {
			t.Fatalf("apply category: %v", err)
		}
		if rows != 0 {
			t.Fatal("the statement overwrote a manual category")
		}
		if got := readTx(t, ctx, pool, sharedSmall); *got.categoryID != groceries {
			t.Fatal("a manual category was changed")
		}
	})

	t.Run("a category from another household is refused", func(t *testing.T) {
		// The rule names a real category id; it is just not one this household
		// can use. Silently succeeding here would file a charge into a category
		// the household cannot even see.
		foreign := uuid.New()
		exec(`INSERT INTO categories (id, household_id, name, slug) VALUES ($1, $2, 'Foreign', 'foreign')`,
			foreign, householdB)

		rows, err := q.ApplyRuleCategory(ctx, dbgen.ApplyRuleCategoryParams{
			ID: sharedBig, CategoryID: &foreign, HouseholdID: householdA,
		})
		if err != nil {
			t.Fatalf("apply category: %v", err)
		}
		if rows != 0 {
			t.Fatal("a category from another household was applied")
		}
	})

	t.Run("a tag from another household is refused", func(t *testing.T) {
		foreignTag := uuid.New()
		exec(`INSERT INTO tags (id, household_id, name) VALUES ($1, $2, 'Foreign')`,
			foreignTag, householdB)

		rows, err := q.AddRuleTag(ctx, dbgen.AddRuleTagParams{
			TransactionID: sharedBig, TagID: foreignTag, HouseholdID: householdA,
		})
		if err != nil {
			t.Fatalf("add tag: %v", err)
		}
		if rows != 0 {
			t.Fatal("a tag from another household was applied")
		}
	})

	// --- The single-row hook ----------------------------------------------

	t.Run("the create hook files one new row", func(t *testing.T) {
		fresh := seedTx(sharedAcct, "SQ *BLUE BOTTLE 5150", "20.00")

		final, err := ApplyToTransaction(ctx, q, householdA, fresh)
		if err != nil {
			t.Fatalf("apply to transaction: %v", err)
		}
		if final == nil {
			t.Fatal("the household has rules, so the hook must report a snapshot")
		}
		if final.CategoryID == nil || *final.CategoryID != coffee {
			t.Fatalf("returned snapshot category = %v, want Coffee", final.CategoryID)
		}
		// The snapshot must describe what is actually stored, not what the
		// planner hoped: the create handler echoes it straight to the client.
		if row := readTx(t, ctx, pool, fresh); row.categoryID == nil || *row.categoryID != coffee {
			t.Fatal("the hook reported a category it did not write")
		}
	})

	t.Run("household B is untouched throughout", func(t *testing.T) {
		// The last line of defence against every scoping mistake above: after
		// all of it, the other household's identical charge is exactly as it
		// was seeded.
		var count int64
		if err := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM transactions t
			 JOIN accounts a ON a.id = t.account_id
			 WHERE a.id = $1 AND (t.category_id IS NOT NULL OR t.notes IS NOT NULL)`,
			acctB).Scan(&count); err != nil {
			t.Fatalf("count: %v", err)
		}
		if count != 0 {
			t.Fatalf("%d of household B's transactions were touched", count)
		}
	})
}

func assertMatchIDs(t *testing.T, matches []Match, want []uuid.UUID) {
	t.Helper()
	got := make(map[uuid.UUID]bool, len(matches))
	for _, m := range matches {
		got[m.Transaction.ID] = true
	}
	if len(got) != len(want) {
		t.Fatalf("matched %d transactions, want %d", len(got), len(want))
	}
	for _, id := range want {
		if !got[id] {
			t.Fatalf("transaction %s did not match but should have", id)
		}
	}
}

type txRow struct {
	categoryID     *uuid.UUID
	categorySource string
	notes          string
	tagCount       int
}

func readTx(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id uuid.UUID) txRow {
	t.Helper()
	var row txRow
	var source, notes *string
	err := pool.QueryRow(ctx, `
		SELECT t.category_id, t.category_source, t.notes,
		       (SELECT COUNT(*) FROM transaction_tags tt WHERE tt.transaction_id = t.id)
		FROM transactions t WHERE t.id = $1`, id).
		Scan(&row.categoryID, &source, &notes, &row.tagCount)
	if err != nil {
		t.Fatalf("read transaction: %v", err)
	}
	if source != nil {
		row.categorySource = *source
	}
	if notes != nil {
		row.notes = *notes
	}
	return row
}
