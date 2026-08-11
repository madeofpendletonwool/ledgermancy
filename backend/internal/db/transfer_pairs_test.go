package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/categorize"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// The structural transfer pairer against a real Postgres. The case is the one
// that motivated the feature: a debit on one account met by an equal credit on
// another a day later, both uncategorised, with no shared payee name to match.
//
//	TEST_DATABASE_URL='postgres://...' go test ./internal/db/ -run TestPairTransfers

type transferFixture struct {
	ctx         context.Context
	pool        *pgxpool.Pool
	q           *dbgen.Queries
	householdID uuid.UUID
	userID      uuid.UUID
	checking    uuid.UUID
	savings     uuid.UUID
	outTxn      uuid.UUID
	inTxn       uuid.UUID
}

func newTransferFixture(t *testing.T) *transferFixture {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	f := &transferFixture{
		ctx:         ctx,
		pool:        pool,
		q:           dbgen.New(pool),
		householdID: uuid.New(),
		userID:      uuid.New(),
		checking:    uuid.New(),
		savings:     uuid.New(),
		outTxn:      uuid.New(),
		inTxn:       uuid.New(),
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed exec: %v\n%s", err, sql)
		}
	}
	exec(`INSERT INTO households (id, name) VALUES ($1, 'Transfer Test')`, f.householdID)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id=$1`, f.householdID) })
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name, role) VALUES ($1,$2,$3,'h','Owner','owner')`,
		f.userID, f.householdID, f.userID.String()+"@x")

	item := uuid.New()
	exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, institution_name, is_shared) VALUES ($1,$2,$3,'\x00','Test Bank',TRUE)`,
		item, f.userID, item.String())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type, subtype, current_balance) VALUES ($1,$2,$3,'A+ Checking','depository','checking',5000)`,
		f.checking, item, f.checking.String())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type, subtype, current_balance) VALUES ($1,$2,$3,'360 Savings','depository','savings',20000)`,
		f.savings, item, f.savings.String())

	// Two legs of one transfer: +700 out of checking, -700 into savings, a day
	// apart. Uncategorized, so they are candidates.
	exec(`INSERT INTO transactions (id, account_id, plaid_transaction_id, name, amount, date, source) VALUES ($1,$2,$3,'ACH CAPITAL ONE - TRANSFER',700.00,'2026-08-06','plaid')`,
		f.outTxn, f.checking, f.outTxn.String())
	exec(`INSERT INTO transactions (id, account_id, plaid_transaction_id, name, amount, date, source) VALUES ($1,$2,$3,'ALTRA FEDERAL CREDIT UNION',-700.00,'2026-08-05','plaid')`,
		f.inTxn, f.savings, f.inTxn.String())
	return f
}

// txnCategory reads a transaction's category slug and source back, for the
// assertions. There is no generated GetTransaction (the app reads through
// filtered lists), so this goes straight through the pool.
func (f *transferFixture) txnCategory(t *testing.T, txnID uuid.UUID) (slug, source string) {
	t.Helper()
	row := f.pool.QueryRow(f.ctx, `
		SELECT COALESCE(c.slug, ''), COALESCE(t.category_source, '')
		FROM transactions t LEFT JOIN categories c ON c.id = t.category_id
		WHERE t.id = $1`, txnID)
	if err := row.Scan(&slug, &source); err != nil {
		t.Fatalf("read txn %s: %v", txnID, err)
	}
	return slug, source
}

// THE HEADLINE: the two legs pair, both get the canonical transfer categories,
// the pair is recorded, and the pass is idempotent.
func TestPairTransfersMatchesAndIsIdempotent(t *testing.T) {
	f := newTransferFixture(t)
	now := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)

	n, err := categorize.PairTransfers(f.ctx, f.q, f.householdID, now, categorize.BackfillLookback)
	if err != nil {
		t.Fatalf("pair transfers: %v", err)
	}
	if n != 1 {
		t.Fatalf("paired %d, want 1", n)
	}

	outSlug, outSrc := f.txnCategory(t, f.outTxn)
	inSlug, _ := f.txnCategory(t, f.inTxn)
	if outSlug != "transfer-out" {
		t.Errorf("out-leg category = %q, want transfer-out", outSlug)
	}
	if inSlug != "transfer-in" {
		t.Errorf("in-leg category = %q, want transfer-in", inSlug)
	}
	if outSrc != "pairing" {
		t.Errorf("out-leg category_source = %q, want pairing", outSrc)
	}

	count, _ := f.q.CountTransferPairs(f.ctx, f.householdID)
	if count != 1 {
		t.Errorf("recorded pairs = %d, want 1", count)
	}
	// A re-run pairs nothing new — the legs are now excluded as already-paired.
	n2, err := categorize.PairTransfers(f.ctx, f.q, f.householdID, now, categorize.BackfillLookback)
	if err != nil {
		t.Fatalf("second pair run: %v", err)
	}
	if n2 != 0 {
		t.Errorf("second run paired %d, want 0 (idempotent)", n2)
	}
}

// A manually categorised leg is never re-categorised by the pairer — the user's
// choice outranks structural matching. The candidate query excludes it, so the
// pair does not form at all.
func TestPairTransfersRespectsManualCategories(t *testing.T) {
	f := newTransferFixture(t)
	now := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)

	groceries, err := f.q.GetCategoryBySlug(f.ctx, dbgen.GetCategoryBySlugParams{Slug: "groceries", HouseholdID: &f.householdID})
	if err != nil {
		t.Fatalf("find groceries: %v", err)
	}
	if _, err := f.q.SetTransactionCategory(f.ctx, dbgen.SetTransactionCategoryParams{
		ID: f.outTxn, HouseholdID: f.householdID, CategoryID: &groceries.ID,
	}); err != nil {
		t.Fatalf("set manual category: %v", err)
	}

	n, err := categorize.PairTransfers(f.ctx, f.q, f.householdID, now, categorize.BackfillLookback)
	if err != nil {
		t.Fatalf("pair transfers: %v", err)
	}
	if n != 0 {
		t.Errorf("paired %d with a manual leg, want 0 (manual is not a candidate)", n)
	}
	if slug, _ := f.txnCategory(t, f.outTxn); slug != "groceries" {
		t.Errorf("manual out-leg re-categorised to %q, want groceries", slug)
	}
}
