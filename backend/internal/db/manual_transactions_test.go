package db

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
)

// TestManualTransactionQueries exercises the manual-transaction CRUD against a
// real Postgres: that a hand-entered row lifts spending by exactly its amount
// while leaving balances untouched, that the household + source='manual' guards
// hold, and that the read-time duplicate flag fires only when a matching Plaid
// row arrives.
//
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' go test ./internal/db/
func TestManualTransactionQueries(t *testing.T) {
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

	// Caller's household.
	householdID := uuid.New()
	userID := uuid.New()
	itemID := uuid.New()
	acctID := uuid.New()
	foodID := uuid.New()

	// A separate household, used to prove scoping: its account must be unwritable
	// and its rows untouchable from the caller's identity.
	otherHouseholdID := uuid.New()
	otherUserID := uuid.New()
	otherItemID := uuid.New()
	otherAcctID := uuid.New()

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed exec: %v\n%s", err, sql)
		}
	}

	seedHousehold := func(hh, u, item, acct uuid.UUID, name string) {
		exec(`INSERT INTO households (id, name) VALUES ($1, $2)`, hh, name)
		exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
		      VALUES ($1, $2, $3, 'x', 'Tester')`, u, hh, u.String()+"@example.test")
		exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, products, status)
		      VALUES ($1, $2, $3, '\x00', '{transactions}', 'active')`, item, u, item.String())
		exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type, current_balance)
		      VALUES ($1, $2, $3, 'Checking', 'depository', '100.00')`, acct, item, acct.String())
	}

	seedHousehold(householdID, userID, itemID, acctID, "Manual Test")
	seedHousehold(otherHouseholdID, otherUserID, otherItemID, otherAcctID, "Other Household")
	exec(`INSERT INTO categories (id, household_id, name, slug) VALUES ($1, $2, 'Food & Drink', 'food-drink')`, foodID, householdID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id = ANY($1)`,
			[]uuid.UUID{householdID, otherHouseholdID})
	})

	yearStart := mustDate(t, "2026-01-01")
	yearEnd := mustDate(t, "2026-12-31")
	txDate := mustDate(t, "2026-06-15")
	amount := decimal.RequireFromString("11.86")

	foodSpend := func() decimal.Decimal {
		t.Helper()
		rows, err := q.GetSpendingByCategory(ctx, dbgen.GetSpendingByCategoryParams{
			HouseholdID: householdID, UserID: userID, Date: yearStart, Date_2: yearEnd,
		})
		if err != nil {
			t.Fatalf("GetSpendingByCategory: %v", err)
		}
		for _, c := range rows {
			if c.CategoryName == "Food & Drink" {
				return c.Total
			}
		}
		return decimal.Zero
	}

	cash := func() decimal.Decimal {
		t.Helper()
		nw, err := q.ComputeNetWorth(ctx, householdID)
		if err != nil {
			t.Fatalf("ComputeNetWorth: %v", err)
		}
		return nw.Cash
	}

	baseSpend := foodSpend()
	baseCash := cash()

	// --- Create ------------------------------------------------------------
	created, err := q.CreateManualTransaction(ctx, dbgen.CreateManualTransactionParams{
		AccountID:   acctID,
		HouseholdID: householdID,
		UserID:      userID,
		Amount:      amount,
		Date:        txDate,
		Name:        "Amazon charge Plaid dropped",
		CategoryID:  &foodID,
	})
	if err != nil {
		t.Fatalf("CreateManualTransaction: %v", err)
	}
	if created.Source != "manual" {
		t.Errorf("source = %q, want manual", created.Source)
	}
	if created.PlaidTransactionID != nil {
		t.Errorf("plaid_transaction_id = %v, want nil", *created.PlaidTransactionID)
	}
	if created.CategorySource == nil || *created.CategorySource != "manual" {
		t.Errorf("category_source = %v, want manual", created.CategorySource)
	}
	if created.Pending {
		t.Error("manual transaction should not be pending")
	}

	if got, want := foodSpend(), baseSpend.Add(amount); !got.Equal(want) {
		t.Errorf("food spend after create = %s, want %s (base %s + %s)", got, want, baseSpend, amount)
	}
	if got := cash(); !got.Equal(baseCash) {
		t.Errorf("cash after create = %s, want unchanged %s", got, baseCash)
	}

	// --- Scoping: cannot insert against another household's account --------
	if _, err := q.CreateManualTransaction(ctx, dbgen.CreateManualTransactionParams{
		AccountID:   otherAcctID,
		HouseholdID: householdID, // caller's household, foreign account
		UserID:      userID,
		Amount:      amount,
		Date:        txDate,
		Name:        "should not insert",
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("create against foreign account err = %v, want pgx.ErrNoRows", err)
	}

	// --- Update ------------------------------------------------------------
	newAmount := decimal.RequireFromString("20.00")
	updated, err := q.UpdateManualTransaction(ctx, dbgen.UpdateManualTransactionParams{
		ID:          created.ID,
		HouseholdID: householdID,
		Amount:      newAmount,
		Date:        txDate,
		Name:        "Amazon charge (corrected)",
		CategoryID:  &foodID,
	})
	if err != nil {
		t.Fatalf("UpdateManualTransaction: %v", err)
	}
	if !updated.Amount.Equal(newAmount) {
		t.Errorf("updated amount = %s, want %s", updated.Amount, newAmount)
	}
	if got, want := foodSpend(), baseSpend.Add(newAmount); !got.Equal(want) {
		t.Errorf("food spend after update = %s, want %s", got, want)
	}

	// Update from another household must not match the row.
	if _, err := q.UpdateManualTransaction(ctx, dbgen.UpdateManualTransactionParams{
		ID:          created.ID,
		HouseholdID: otherHouseholdID,
		Amount:      newAmount,
		Date:        txDate,
		Name:        "should not update",
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("cross-household update err = %v, want pgx.ErrNoRows", err)
	}

	// --- The endpoints must refuse a source='plaid' row -------------------
	plaidTxID := uuid.New()
	exec(`INSERT INTO transactions
	        (id, account_id, plaid_transaction_id, amount, currency, date, name, category_id, source)
	      VALUES ($1, $2, $3, '9.99', 'USD', $4, 'STARBUCKS', $5, 'plaid')`,
		plaidTxID, acctID, "plaid-"+plaidTxID.String(), txDate, foodID)

	if _, err := q.UpdateManualTransaction(ctx, dbgen.UpdateManualTransactionParams{
		ID:          plaidTxID,
		HouseholdID: householdID,
		Amount:      newAmount,
		Date:        txDate,
		Name:        "should not touch a plaid row",
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("update of plaid row err = %v, want pgx.ErrNoRows", err)
	}
	if rows, err := q.DeleteManualTransaction(ctx, dbgen.DeleteManualTransactionParams{
		ID: plaidTxID, HouseholdID: householdID,
	}); err != nil {
		t.Fatalf("DeleteManualTransaction (plaid): %v", err)
	} else if rows != 0 {
		t.Errorf("delete of plaid row affected %d rows, want 0", rows)
	}

	// --- Duplicate flag ----------------------------------------------------
	// The manual row is not a duplicate until a matching Plaid charge arrives.
	if flagged := isFlaggedDuplicate(t, ctx, q, householdID, userID, created.ID); flagged {
		t.Error("manual row flagged as duplicate before any matching Plaid charge")
	}
	// A Plaid charge on the same account, same (updated) amount, one day later.
	dupID := uuid.New()
	exec(`INSERT INTO transactions
	        (id, account_id, plaid_transaction_id, amount, currency, date, name, source)
	      VALUES ($1, $2, $3, $4, 'USD', $5, 'AMAZON', 'plaid')`,
		dupID, acctID, "plaid-"+dupID.String(), newAmount, mustDate(t, "2026-06-16"))
	if flagged := isFlaggedDuplicate(t, ctx, q, householdID, userID, created.ID); !flagged {
		t.Error("manual row not flagged after a matching Plaid charge arrived")
	}

	// --- Delete ------------------------------------------------------------
	if rows, err := q.DeleteManualTransaction(ctx, dbgen.DeleteManualTransactionParams{
		ID: created.ID, HouseholdID: householdID,
	}); err != nil {
		t.Fatalf("DeleteManualTransaction: %v", err)
	} else if rows != 1 {
		t.Errorf("delete affected %d rows, want 1", rows)
	}
	if got := foodSpend(); !got.Equal(baseSpend.Add(decimal.RequireFromString("9.99"))) {
		// After deleting the manual row, only the seeded Plaid Starbucks (9.99)
		// remains above baseline in Food & Drink.
		t.Errorf("food spend after delete = %s, want base + 9.99 (%s)", got,
			baseSpend.Add(decimal.RequireFromString("9.99")))
	}
}

// isFlaggedDuplicate finds one transaction in the visible list and returns its
// possible-duplicate flag.
func isFlaggedDuplicate(
	t *testing.T,
	ctx context.Context,
	q *dbgen.Queries,
	householdID, userID, txID uuid.UUID,
) bool {
	t.Helper()
	rows, err := q.ListVisibleTransactions(ctx, dbgen.ListVisibleTransactionsParams{
		HouseholdID: householdID,
		UserID:      userID,
		Date:        mustDate(t, "2026-01-01"),
		Date_2:      mustDate(t, "2026-12-31"),
		Limit:       500,
		Offset:      0,
	})
	if err != nil {
		t.Fatalf("ListVisibleTransactions: %v", err)
	}
	for _, r := range rows {
		if r.ID == txID {
			return r.IsPossibleDuplicate != nil && *r.IsPossibleDuplicate
		}
	}
	t.Fatalf("transaction %s not found in visible list", txID)
	return false
}
