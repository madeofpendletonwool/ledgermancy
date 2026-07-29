package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/networth"
)

// The double-counting assertions from doc 21's verification list.
//
// These are the tests that make the rest of the feature trustworthy. A split is
// an ATTRIBUTION OVERLAY and an allowance entry is not a transaction; if either
// ever reaches a household aggregate, every spending figure in the app inflates
// silently and in one direction. Exact decimals throughout — a tolerance here
// would defeat the point.

type splitFixture struct {
	*peopleFixture
	accountID uuid.UUID
	txnID     uuid.UUID
	txnAmount decimal.Decimal
}

func newSplitFixture(t *testing.T) *splitFixture {
	t.Helper()

	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	pf := newPeopleFixture(t)

	ctx := context.Background()
	pool, err := Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed exec: %v\n%s", err, sql)
		}
	}

	itemID := uuid.New()
	f := &splitFixture{
		peopleFixture: pf,
		accountID:     uuid.New(),
		txnID:         uuid.New(),
		txnAmount:     decimal.RequireFromString("100.00"),
	}

	exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, institution_name, is_shared)
	      VALUES ($1,$2,$3,'\x00','Test Bank',TRUE)`, itemID, pf.ownerID, itemID.String())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type, current_balance)
	      VALUES ($1,$2,$3,'Joint Checking','depository',5000.00)`,
		f.accountID, itemID, f.accountID.String())
	// Positive = money out, the Plaid convention this codebase follows.
	exec(`INSERT INTO transactions (id, account_id, plaid_transaction_id, name, amount, date, source)
	      VALUES ($1,$2,$3,'Groceries',100.00,CURRENT_DATE,'plaid')`,
		f.txnID, f.accountID, f.txnID.String())

	return f
}

// householdSpend is the figure that must not move. It reads the same column the
// reporting queries do, over the same visibility join.
func (f *splitFixture) householdSpend(t *testing.T) decimal.Decimal {
	t.Helper()
	sum, err := f.q.GetSpendingSummary(f.ctx, dbgen.GetSpendingSummaryParams{
		HouseholdID: f.householdID,
		UserID:      uuid.Nil,
		Date:        time.Now().AddDate(0, 0, -7),
		Date_2:      time.Now().AddDate(0, 0, 1),
	})
	if err != nil {
		t.Fatalf("spending summary: %v", err)
	}
	return sum.Spending
}

// TestSplittingDoesNotChangeHouseholdSpend is THE trap this feature had to
// avoid. Splitting a transaction across members must leave household spending
// byte-identical: the money left the household once.
func TestSplittingDoesNotChangeHouseholdSpend(t *testing.T) {
	f := newSplitFixture(t)

	before := f.householdSpend(t)

	for _, share := range []struct {
		person uuid.UUID
		amount string
	}{
		{f.ownerPerson, "50.00"},
		{f.partnerPers, "50.00"},
	} {
		if _, err := f.q.CreateTransactionSplit(f.ctx, dbgen.CreateTransactionSplitParams{
			TransactionID: f.txnID,
			PersonID:      share.person,
			HouseholdID:   f.householdID,
			Amount:        decimal.RequireFromString(share.amount),
		}); err != nil {
			t.Fatalf("create split: %v", err)
		}
	}

	after := f.householdSpend(t)
	if !before.Equal(after) {
		t.Errorf("splitting changed household spend: %s -> %s",
			before.StringFixed(2), after.StringFixed(2))
	}
}

// TestAllowanceEntriesDoNotChangeHouseholdSpend: pocket money is not household
// spending. There is no account behind an allowance entry and it must never
// reach a spending aggregate.
func TestAllowanceEntriesDoNotChangeHouseholdSpend(t *testing.T) {
	f := newSplitFixture(t)

	before := f.householdSpend(t)

	for _, e := range []struct {
		kind   string
		amount string
	}{
		{"allowance", "25.00"},
		{"spend", "-18.75"},
		{"gift", "50.00"},
	} {
		if _, err := f.q.CreateAllowanceEntry(f.ctx, dbgen.CreateAllowanceEntryParams{
			PersonID: f.kidPerson, HouseholdID: f.householdID,
			Kind: e.kind, Amount: decimal.RequireFromString(e.amount),
			OccurredOn: time.Now(),
		}); err != nil {
			t.Fatalf("create entry: %v", err)
		}
	}

	after := f.householdSpend(t)
	if !before.Equal(after) {
		t.Errorf("allowance entries changed household spend: %s -> %s",
			before.StringFixed(2), after.StringFixed(2))
	}
}

// TestBeneficiaryTaggingDoesNotChangeNetWorth: tagging an account with the
// person it is held for is a BREAKDOWN, not a transfer. The household's net
// worth must not move by a cent.
func TestBeneficiaryTaggingDoesNotChangeNetWorth(t *testing.T) {
	f := newSplitFixture(t)

	before, err := networth.Compute(f.ctx, f.q, f.householdID)
	if err != nil {
		t.Fatalf("compute net worth: %v", err)
	}

	if _, err := f.q.SetAccountBeneficiary(f.ctx, dbgen.SetAccountBeneficiaryParams{
		ID: f.accountID, HouseholdID: f.householdID, PersonID: &f.kidPerson,
	}); err != nil {
		t.Fatalf("set beneficiary: %v", err)
	}

	after, err := networth.Compute(f.ctx, f.q, f.householdID)
	if err != nil {
		t.Fatalf("compute net worth: %v", err)
	}

	if !before.NetWorth.Equal(after.NetWorth) {
		t.Errorf("beneficiary tagging changed net worth: %s -> %s",
			before.NetWorth.StringFixed(2), after.NetWorth.StringFixed(2))
	}
	if !before.AssetsTotal.Equal(after.AssetsTotal) {
		t.Errorf("beneficiary tagging changed assets: %s -> %s",
			before.AssetsTotal.StringFixed(2), after.AssetsTotal.StringFixed(2))
	}
}

// TestLedgerNetsMixedSettlement: settled shares drop out, a share assigned to
// the payer is not a debt to themselves, and what remains is what is owed.
func TestLedgerNetsMixedSettlement(t *testing.T) {
	f := newSplitFixture(t)

	// The owner's account paid, so the owner is the creditor. Their own 50 is
	// not a debt; the partner's 50 is.
	var partnerSplitID uuid.UUID
	for _, share := range []struct {
		person uuid.UUID
		amount string
	}{
		{f.ownerPerson, "50.00"},
		{f.partnerPers, "50.00"},
	} {
		row, err := f.q.CreateTransactionSplit(f.ctx, dbgen.CreateTransactionSplitParams{
			TransactionID: f.txnID, PersonID: share.person,
			HouseholdID: f.householdID,
			Amount:      decimal.RequireFromString(share.amount),
		})
		if err != nil {
			t.Fatalf("create split: %v", err)
		}
		if share.person == f.partnerPers {
			partnerSplitID = row.ID
		}
	}

	rows, err := f.q.HouseholdLedger(f.ctx, f.householdID)
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 ledger row (the payer's own share excluded), got %d", len(rows))
	}
	if rows[0].DebtorID != f.partnerPers || rows[0].CreditorID != f.ownerPerson {
		t.Errorf("wrong direction: %s owes %s", rows[0].DebtorName, rows[0].CreditorName)
	}
	if got, want := rows[0].Amount.StringFixed(2), "50.00"; got != want {
		t.Errorf("owed = %s, want %s", got, want)
	}

	// Settling empties the ledger. The row stays for history; the balance goes.
	n, err := f.q.SettleSplit(f.ctx, dbgen.SettleSplitParams{
		ID: partnerSplitID, HouseholdID: f.householdID,
	})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if n != 1 {
		t.Fatalf("settle affected %d rows, want 1", n)
	}

	rows, err = f.q.HouseholdLedger(f.ctx, f.householdID)
	if err != nil {
		t.Fatalf("ledger after settle: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected an empty ledger after settling, got %d rows", len(rows))
	}

	// Settling twice is a no-op rather than a second effect.
	n, err = f.q.SettleSplit(f.ctx, dbgen.SettleSplitParams{
		ID: partnerSplitID, HouseholdID: f.householdID,
	})
	if err != nil {
		t.Fatalf("settle again: %v", err)
	}
	if n != 0 {
		t.Errorf("re-settling affected %d rows, want 0", n)
	}
}
