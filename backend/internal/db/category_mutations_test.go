package db

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
)

// TestCategoryMutations covers the pieces that back custom categories and the
// per-merchant "apply to all" recategorisation: creating/updating/deleting a
// household category (with the system-default guard), and the retroactive
// merchant apply that fixes every existing charge from a merchant at once.
//
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' go test ./internal/db/
func TestCategoryMutations(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	pool, err := Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	q := dbgen.New(pool)

	householdID := uuid.New()
	userID := uuid.New()
	itemID := uuid.New()
	acctID := uuid.New()

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, sql)
		}
	}
	exec(`INSERT INTO households (id, name) VALUES ($1, 'Cat Mut Test')`, householdID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id = $1`, householdID)
	})
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
	      VALUES ($1, $2, $3, 'x', 'Tester')`, userID, householdID, userID.String()+"@example.test")
	exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, products, status)
	      VALUES ($1, $2, $3, '\x00', '{transactions}', 'active')`, itemID, userID, itemID.String())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type)
	      VALUES ($1, $2, $3, 'Checking', 'depository')`, acctID, itemID, acctID.String())

	// --- Create / update / delete a custom category ------------------------
	custom, err := q.CreateCategory(ctx, dbgen.CreateCategoryParams{
		HouseholdID: &householdID, Name: "Childcare", Slug: "childcare", IsFixed: false,
	})
	if err != nil {
		t.Fatalf("create category: %v", err)
	}
	if custom.HouseholdID == nil || *custom.HouseholdID != householdID {
		t.Fatalf("created category not owned by household")
	}

	updated, err := q.UpdateCategory(ctx, dbgen.UpdateCategoryParams{
		ID: custom.ID, HouseholdID: &householdID, Name: "Child Care", IsFixed: true,
	})
	if err != nil {
		t.Fatalf("update category: %v", err)
	}
	if updated.Name != "Child Care" || !updated.IsFixed {
		t.Errorf("update didn't apply: %+v", updated)
	}

	// A system default (household_id NULL) must be un-editable via the guard:
	// UpdateCategory returns no row for a category the household doesn't own.
	var sysID uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT id FROM categories WHERE slug = 'groceries' AND household_id IS NULL`).Scan(&sysID); err != nil {
		t.Fatalf("find system category: %v", err)
	}
	_, err = q.UpdateCategory(ctx, dbgen.UpdateCategoryParams{
		ID: sysID, HouseholdID: &householdID, Name: "Hacked", IsFixed: false,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("expected system category to be un-editable (ErrNoRows), got %v", err)
	}

	// --- Retroactive per-merchant apply ------------------------------------
	// Three charges from "Free Range Exchange", all currently non-manual.
	for _, amt := range []string{"11.86", "9.40", "12.10"} {
		exec(`INSERT INTO transactions (account_id, amount, currency, date, name, merchant_name, merchant_key, category_source, source)
		      VALUES ($1, $2, 'USD', '2026-07-21', 'Free Range Exchange', 'Free Range Exchange', 'free-range-exchange', 'plaid', 'plaid')`,
			acctID, amt)
	}
	// One the user already pinned manually to something else — must be preserved.
	exec(`INSERT INTO transactions (account_id, amount, currency, date, name, merchant_name, merchant_key, category_id, category_source, source)
	      VALUES ($1, '5.00', 'USD', '2026-07-01', 'Free Range Exchange', 'Free Range Exchange', 'free-range-exchange', $2, 'manual', 'plaid')`,
		acctID, custom.ID)

	// Apply the household "Food & Drink"-ish custom category to the whole merchant.
	target, err := q.CreateCategory(ctx, dbgen.CreateCategoryParams{
		HouseholdID: &householdID, Name: "Coffee", Slug: "coffee",
	})
	if err != nil {
		t.Fatalf("create target category: %v", err)
	}
	mk := "free-range-exchange"
	if err := q.ApplyMerchantCategoryRewritable(ctx, dbgen.ApplyMerchantCategoryRewritableParams{
		HouseholdID: householdID, MerchantKey: &mk, CategoryID: &target.ID,
	}); err != nil {
		t.Fatalf("apply merchant category: %v", err)
	}

	// All three non-manual rows now carry the target (source 'cache'); the
	// manual row is untouched.
	var cacheCount, manualPreserved int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM transactions WHERE account_id = $1 AND merchant_key = $2 AND category_id = $3 AND category_source = 'cache'`,
		acctID, mk, target.ID).Scan(&cacheCount); err != nil {
		t.Fatal(err)
	}
	if cacheCount != 3 {
		t.Errorf("cache-applied rows = %d, want 3", cacheCount)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM transactions WHERE account_id = $1 AND merchant_key = $2 AND category_source = 'manual' AND category_id = $3`,
		acctID, mk, custom.ID).Scan(&manualPreserved); err != nil {
		t.Fatal(err)
	}
	if manualPreserved != 1 {
		t.Errorf("manual-pinned rows preserved = %d, want 1", manualPreserved)
	}

	// --- Delete reverts charges to uncategorised (ON DELETE SET NULL) ------
	if err := q.DeleteCategory(ctx, dbgen.DeleteCategoryParams{ID: target.ID, HouseholdID: &householdID}); err != nil {
		t.Fatalf("delete category: %v", err)
	}
	var nulled int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM transactions WHERE account_id = $1 AND merchant_key = $2 AND category_id IS NULL`, acctID, mk).Scan(&nulled); err != nil {
		t.Fatal(err)
	}
	if nulled != 3 {
		t.Errorf("after delete, uncategorised rows = %d, want 3", nulled)
	}
}
