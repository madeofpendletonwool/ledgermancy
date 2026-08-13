package db

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/networth"
)

// Per-account balance snapshots for Plaid accounts (MAD-119).
//
// The SQL-level concern these tests pin is the same one net_worth_snapshots
// already carried: a balance that is never written down is lost the moment it
// moves, so the snapshot path has to write the right rows for the right
// accounts and the read endpoint has to reconcile against them. Two behaviours
// matter most and are not visible from Go alone:
//
//   - Source selection. The snapshot writes Plaid accounts only; a manual
//     account's history is the user's writes, and interleaving app-written
//     repeats of their balance would bury the entries that explain a move.
//   - Household scoping. A snapshot is background bookkeeping like the net-worth
//     sweep, so a Plaid account in another household must never get a row from
//     this household's pass — and the read endpoint must still honour
//     per-member visibility.

// plaidAccount seeds a linked account owned by owner through an item, with a
// current_balance the snapshot will record. source='plaid' is the default.
func (f *manualFixture) plaidAccount(t *testing.T, name, kind, balance string) uuid.UUID {
	t.Helper()
	itemID, accountID := uuid.New(), uuid.New()
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted)
		 VALUES ($1, $2, $3, '\x00')`,
		itemID, f.ownerID, itemID.String()); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type, current_balance)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		accountID, itemID, accountID.String(), name, kind,
		decimal.RequireFromString(balance)); err != nil {
		t.Fatalf("seed plaid account: %v", err)
	}
	return accountID
}

// plaidAccountInOtherHousehold seeds an account the owner cannot see, to prove a
// snapshot pass never reaches across households.
func (f *manualFixture) plaidAccountInOtherHousehold(t *testing.T, balance string) uuid.UUID {
	t.Helper()
	otherHousehold, otherUser := uuid.New(), uuid.New()
	itemID, accountID := uuid.New(), uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
			t.Fatalf("seed other-household: %v\n%s", err, sql)
		}
	}
	exec(`INSERT INTO households (id, name) VALUES ($1, 'Other')`, otherHousehold)
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name, role)
	      VALUES ($1,$2,$3,'h','Other','owner')`, otherUser, otherHousehold, otherUser.String()+"@x")
	exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted)
	      VALUES ($1, $2, $3, '\x00')`, itemID, otherUser, itemID.String())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type, current_balance)
	      VALUES ($1, $2, $3, 'Their Checking', 'depository', $4)`,
 		accountID, itemID, accountID.String(), decimal.RequireFromString(balance))
 	return accountID
 }
 
// TestSnapshotAccountBalancesWritesARowPerPlaidAccount is the core premise: a
// Plaid account's balance is otherwise ephemeral, and the snapshot is what makes
// a trend recoverable. After a pass the account must carry exactly one
// reason='snapshot' row holding the current balance.
func TestSnapshotAccountBalancesWritesARowPerPlaidAccount(t *testing.T) {
	f := newManualFixture(t)
	checking := f.plaidAccount(t, "Chase Checking", "depository", "4231.50")

	n, err := networth.SnapshotAccountBalances(f.ctx, f.q, f.householdID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if n != 1 {
		t.Fatalf("wrote %d rows, want 1", n)
	}

	rows, err := f.q.ListAccountBalanceHistory(f.ctx, dbgen.ListAccountBalanceHistoryParams{
		AccountID: checking, HouseholdID: f.householdID, UserID: f.ownerID,
	})
	if err != nil {
		t.Fatalf("list history: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("history rows = %d, want 1", len(rows))
	}
	if rows[0].Reason != "snapshot" {
		t.Errorf("reason = %q, want snapshot", rows[0].Reason)
	}
	if !rows[0].Balance.Equal(decimal.RequireFromString("4231.50")) {
		t.Errorf("balance = %s, want 4231.50", rows[0].Balance)
	}
	if rows[0].AsOf.Format(time.DateOnly) != todayUTC() {
		t.Errorf("as_of = %s, want %s", rows[0].AsOf.Format(time.DateOnly), todayUTC())
	}
}

// TestSnapshotAccountBalancesLeavesManualAccountsAlone: a manual account's
// history is the user's writes. The snapshot path writing a daily repeat of
// their balance would bury the entries that explain a move, so it must not.
func TestSnapshotAccountBalancesLeavesManualAccountsAlone(t *testing.T) {
	f := newManualFixture(t)
	manual := f.account(t, "Cash under the mattress", "depository", true, "200.00")

	if _, err := networth.SnapshotAccountBalances(f.ctx, f.q, f.householdID); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	rows, err := f.q.ListAccountBalanceHistory(f.ctx, dbgen.ListAccountBalanceHistoryParams{
		AccountID: manual, HouseholdID: f.householdID, UserID: f.ownerID,
	})
	if err != nil {
		t.Fatalf("list history: %v", err)
	}
	// The opening-balance row from account creation is the manual account's own
	// trail. Nothing the snapshot path wrote should be in it.
	for _, r := range rows {
		if r.Reason == "snapshot" {
			t.Errorf("manual account got a snapshot row: %+v", r)
		}
	}
}

// TestSnapshotAccountBalancesIsIdempotentPerDay mirrors the rule
// UpsertNetWorthSnapshot carries: re-running on the same day replaces that day's
// row rather than adding a second one. A sync and the daily sweep landing on the
// same household must compose to one point per account per day.
func TestSnapshotAccountBalancesIsIdempotentPerDay(t *testing.T) {
	f := newManualFixture(t)
	checking := f.plaidAccount(t, "Chase Checking", "depository", "100.00")

	for i := 0; i < 3; i++ {
		if _, err := networth.SnapshotAccountBalances(f.ctx, f.q, f.householdID); err != nil {
			t.Fatalf("snapshot pass %d: %v", i, err)
		}
	}

	var n int
	if err := f.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM account_balance_history
		 WHERE account_id = $1 AND as_of = CURRENT_DATE`, checking,
	).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("rows for today = %d, want 1 (same-day writes must replace, not append)", n)
	}
}

// TestSnapshotAccountBalancesIsHouseholdScoped: a Plaid account in another
// household must not get a row from this household's pass. The snapshot is
// background bookkeeping; reaching across households would silently build a
// trend for somebody else's account.
func TestSnapshotAccountBalancesIsHouseholdScoped(t *testing.T) {
	f := newManualFixture(t)
	own := f.plaidAccount(t, "Chase Checking", "depository", "100.00")
	theirs := f.plaidAccountInOtherHousehold(t, "9999.00")

	if _, err := networth.SnapshotAccountBalances(f.ctx, f.q, f.householdID); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	hasRow := func(id uuid.UUID) bool {
		var n int
		if err := f.pool.QueryRow(f.ctx,
			`SELECT count(*) FROM account_balance_history WHERE account_id = $1`, id,
		).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n > 0
	}
	if !hasRow(own) {
		t.Error("own Plaid account got no snapshot row")
	}
	if hasRow(theirs) {
		t.Error("another household's Plaid account got a snapshot row")
	}
}

// TestBalanceHistoryRangeBoundsAPlaidTrend pins the optional from/to window the
// chart relies on. A year of daily snapshots is the realistic load for a linked
// account; without bounding, every render pulls the whole history.
func TestBalanceHistoryRangeBoundsAPlaidTrend(t *testing.T) {
	f := newManualFixture(t)
	checking := f.plaidAccount(t, "Chase Checking", "depository", "100.00")

	// Seed three snapshot rows: old, in-window, and the one the snapshot path
	// writes for today. as_of is a DATE; insert with explicit days so the window
	// boundary is unambiguous.
	seed := func(daysAgo int, balance string) {
		t.Helper()
		if _, err := f.pool.Exec(f.ctx,
			`INSERT INTO account_balance_history (account_id, as_of, balance, reason)
			 VALUES ($1, CURRENT_DATE - $2::int, $3, 'snapshot')
			 ON CONFLICT (account_id, as_of) DO UPDATE SET balance = EXCLUDED.balance`,
			checking, daysAgo, decimal.RequireFromString(balance)); err != nil {
			t.Fatalf("seed history: %v", err)
		}
	}
	seed(400, "10.00") // outside a year
	seed(30, "50.00")  // inside the window
	// today is written by the snapshot path itself
	if _, err := networth.SnapshotAccountBalances(f.ctx, f.q, f.householdID); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	// A one-year window: from = today - 365, to = today.
	from := time.Now().UTC().AddDate(0, 0, -365)
	to := time.Now().UTC()
	rows, err := f.q.ListAccountBalanceHistory(f.ctx, dbgen.ListAccountBalanceHistoryParams{
		AccountID: checking, HouseholdID: f.householdID, UserID: f.ownerID,
		From: &from, To: &to,
	})
	if err != nil {
		t.Fatalf("list history: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("in-window rows = %d, want 2 (30 days ago + today)", len(rows))
	}
	for _, r := range rows {
		if r.AsOf.Before(from) || r.AsOf.After(to.AddDate(0, 0, 1)) {
			t.Errorf("as_of %s outside [%s, %s]", r.AsOf, from, to)
		}
	}

	// Unbounded (the manual editor's call) returns every row including the
	// 400-day-old one — the default behaviour must not change.
	all, err := f.q.ListAccountBalanceHistory(f.ctx, dbgen.ListAccountBalanceHistoryParams{
		AccountID: checking, HouseholdID: f.householdID, UserID: f.ownerID,
	})
	if err != nil {
		t.Fatalf("list history unbounded: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("unbounded rows = %d, want 3", len(all))
	}
}

// TestSnapshotAccountBalancesSkipsAccountsTheReaderCannotSee documents the
// contract between the two halves: the snapshot writes household-wide (so the
// daily sweep covers a quiet account a private member holds), while the read
// still honours per-member visibility. A private Plaid account gets a snapshot
// row, but the other member's read returns nothing — the trend is recorded
// without leaking the figure to somebody who must not see it.
func TestSnapshotAccountBalancesSkipsAccountsTheReaderCannotSee(t *testing.T) {
	f := newManualFixture(t)
	// A private Plaid item: is_shared = false, so the partner cannot see it.
	itemID, private := uuid.New(), uuid.New()
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, is_shared)
		 VALUES ($1, $2, $3, '\x00', FALSE)`,
		itemID, f.ownerID, itemID.String()); err != nil {
		t.Fatalf("seed private item: %v", err)
	}
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type, current_balance)
		 VALUES ($1, $2, $3, 'Secret brokerage', 'brokerage', 5000)`,
		private, itemID, private.String()); err != nil {
		t.Fatalf("seed private account: %v", err)
	}

	if _, err := networth.SnapshotAccountBalances(f.ctx, f.q, f.householdID); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	// The owner sees the trend; the partner does not.
	ownerRows, err := f.q.ListAccountBalanceHistory(f.ctx, dbgen.ListAccountBalanceHistoryParams{
		AccountID: private, HouseholdID: f.householdID, UserID: f.ownerID,
	})
	if err != nil {
		t.Fatalf("owner read: %v", err)
	}
	if len(ownerRows) != 1 {
		t.Errorf("owner sees %d rows, want 1", len(ownerRows))
	}
	partnerRows, err := f.q.ListAccountBalanceHistory(f.ctx, dbgen.ListAccountBalanceHistoryParams{
		AccountID: private, HouseholdID: f.householdID, UserID: f.partnerID,
	})
	if err != nil {
		t.Fatalf("partner read: %v", err)
	}
	if len(partnerRows) != 0 {
		t.Errorf("partner sees %d rows, want 0 (private account leaked)", len(partnerRows))
	}
}

func todayUTC() string { return time.Now().UTC().Truncate(24 * time.Hour).Format(time.DateOnly) }
