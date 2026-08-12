package db

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/jobs"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/testdb"
)

// Manual accounts and scheduled posting (doc 30).
//
// Two things here are worth testing at the SQL level rather than in Go, because
// that is where they are actually decided:
//
//   - Visibility. An account can now be owned two different ways — through a
//     Plaid item or directly — and account_access resolves both. A bug there
//     does not error, it leaks one member's private institution into the
//     other's view, so the negative case matters more than the positive one.
//   - Posting idempotency. The worker moves money into a retirement account. A
//     redelivered job that credits twice is invisible until it has been
//     compounding for a year.

type manualFixture struct {
	ctx         context.Context
	pool        *pgxpool.Pool
	q           *dbgen.Queries
	householdID uuid.UUID
	ownerID     uuid.UUID
	partnerID   uuid.UUID
}

func newManualFixture(t *testing.T) *manualFixture {
	t.Helper()

	url := testdb.URL(t)

	ctx := context.Background()
	pool, err := Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	f := &manualFixture{
		ctx: ctx, pool: pool, q: dbgen.New(pool),
		householdID: uuid.New(), ownerID: uuid.New(), partnerID: uuid.New(),
	}

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed exec: %v\n%s", err, sql)
		}
	}
	exec(`INSERT INTO households (id, name) VALUES ($1, 'Manual Test')`, f.householdID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM households WHERE id = $1`, f.householdID)
	})
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name, role)
	      VALUES ($1,$2,$3,'h','Alex','owner')`, f.ownerID, f.householdID, f.ownerID.String()+"@x")
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name, role)
	      VALUES ($1,$2,$3,'h','Sam','member')`, f.partnerID, f.householdID, f.partnerID.String()+"@x")

	return f
}

// account creates a manual account owned by owner, shared or not.
func (f *manualFixture) account(t *testing.T, name, kind string, shared bool, balance string) uuid.UUID {
	t.Helper()
	var bal decimal.NullDecimal
	if balance != "" {
		bal = decimal.NewNullDecimal(decimal.RequireFromString(balance))
	}
	a, err := f.q.CreateManualAccount(f.ctx, dbgen.CreateManualAccountParams{
		UserID: f.ownerID, HouseholdID: f.householdID, IsShared: shared,
		Name: name, Type: kind, Currency: "USD", CurrentBalance: bal,
	})
	if err != nil {
		t.Fatalf("create manual account: %v", err)
	}
	return a.ID
}

// TestManualAccountIsVisibleWithoutAPlaidItem is the whole premise: before
// 00053 an account could not exist without an item, and every scoped read
// reached the household through one. A manual account must come back from the
// ordinary list, not a special one.
func TestManualAccountIsVisibleWithoutAPlaidItem(t *testing.T) {
	f := newManualFixture(t)
	id := f.account(t, "Voya 401(k)", "investment", true, "10000.00")

	rows, err := f.q.ListVisibleAccounts(f.ctx, dbgen.ListVisibleAccountsParams{
		HouseholdID: f.householdID, UserID: f.ownerID,
	})
	if err != nil {
		t.Fatalf("list visible accounts: %v", err)
	}

	var found *dbgen.ListVisibleAccountsRow
	for i := range rows {
		if rows[i].ID == id {
			found = &rows[i]
		}
	}
	if found == nil {
		t.Fatal("manual account missing from ListVisibleAccounts")
	}
	if found.Source != "manual" {
		t.Errorf("source = %q, want manual", found.Source)
	}
	if found.PlaidItemID != nil {
		t.Error("manual account should carry no plaid item")
	}
	// The resolved owner, not the raw column. Reading accounts.user_id straight
	// would report every Plaid account as unowned.
	if found.OwnerID != f.ownerID {
		t.Errorf("owner_id = %v, want %v", found.OwnerID, f.ownerID)
	}
}

// TestPrivateManualAccountIsHiddenFromTheOtherMember is the negative case, and
// the one that actually matters: is_shared=false must be honoured through
// account_access exactly as plaid_items.is_shared always was.
func TestPrivateManualAccountIsHiddenFromTheOtherMember(t *testing.T) {
	f := newManualFixture(t)
	private := f.account(t, "Private brokerage", "brokerage", false, "500.00")
	shared := f.account(t, "Joint savings", "depository", true, "500.00")

	visible := func(userID uuid.UUID) map[uuid.UUID]bool {
		rows, err := f.q.ListVisibleAccounts(f.ctx, dbgen.ListVisibleAccountsParams{
			HouseholdID: f.householdID, UserID: userID,
		})
		if err != nil {
			t.Fatalf("list visible accounts: %v", err)
		}
		out := map[uuid.UUID]bool{}
		for _, r := range rows {
			out[r.ID] = true
		}
		return out
	}

	if !visible(f.ownerID)[private] {
		t.Error("owner cannot see their own private account")
	}
	if visible(f.partnerID)[private] {
		t.Error("private manual account leaked to the other household member")
	}
	if !visible(f.partnerID)[shared] {
		t.Error("shared manual account missing for the other member")
	}
}

// TestManualMutationsRefuseAPlaidAccount: the manual endpoints must be unable
// to reach a linked account even with a valid id. An edit that stuck until the
// next sync silently reverted it is worse than a refusal.
func TestManualMutationsRefuseAPlaidAccount(t *testing.T) {
	f := newManualFixture(t)

	itemID, plaidAccountID := uuid.New(), uuid.New()
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted)
		 VALUES ($1, $2, $3, '\x00')`,
		itemID, f.ownerID, itemID.String()); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type, current_balance)
		 VALUES ($1, $2, $3, 'Chase Checking', 'depository', 100)`,
		plaidAccountID, itemID, plaidAccountID.String()); err != nil {
		t.Fatalf("seed plaid account: %v", err)
	}

	n, err := f.q.DeleteManualAccount(f.ctx, dbgen.DeleteManualAccountParams{
		ID: plaidAccountID, HouseholdID: f.householdID, UserID: f.ownerID,
	})
	if err != nil {
		t.Fatalf("delete manual account: %v", err)
	}
	if n != 0 {
		t.Error("a Plaid account was deleted through the manual endpoint")
	}

	if _, err := f.q.SetManualAccountBalance(f.ctx, dbgen.SetManualAccountBalanceParams{
		ID: plaidAccountID, HouseholdID: f.householdID, UserID: f.ownerID,
		Balance: decimal.NewNullDecimal(decimal.RequireFromString("999999")),
	}); err == nil {
		t.Error("a Plaid account's balance was overwritten through the manual endpoint")
	}

	// And the balance really is untouched, not merely un-returned.
	var balance decimal.Decimal
	if err := f.pool.QueryRow(f.ctx,
		`SELECT current_balance FROM accounts WHERE id = $1`, plaidAccountID,
	).Scan(&balance); err != nil {
		t.Fatalf("read balance: %v", err)
	}
	if !balance.Equal(decimal.RequireFromString("100")) {
		t.Errorf("Plaid balance = %s, want 100", balance)
	}
}

// obligation seeds an auto-posting obligation anchored `daysAgo` days back.
func (f *manualFixture) obligation(
	t *testing.T, target uuid.UUID, amount string, anchor time.Time, unit string,
) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO recurring_obligations
		   (id, household_id, user_id, label, amount, interval_count, interval_unit,
		    anchor_date, source, auto_post, posting_account_id)
		 VALUES ($1,$2,$3,'Voya contribution',$4,1,$5,$6,'manual',TRUE,$7)`,
		id, f.householdID, f.ownerID, decimal.RequireFromString(amount), unit,
		anchor, target); err != nil {
		t.Fatalf("seed obligation: %v", err)
	}
	return id
}

func (f *manualFixture) countRows(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// TestScheduledPostingMovesEverythingExactlyOnce covers the full effect of one
// due contribution and, more importantly, that running the worker again
// produces nothing further. The cursor and the partial unique index are two
// independent guards; this exercises them together, which is how they run.
func TestScheduledPostingMovesEverythingExactlyOnce(t *testing.T) {
	f := newManualFixture(t)
	voya := f.account(t, "Voya 401(k)", "investment", true, "10000.00")

	now := time.Now().UTC().Truncate(24 * time.Hour)
	// Exactly one occurrence due. Anchoring a full month back would give two —
	// the anchor itself and today — which is correct behaviour but makes this
	// test about expansion rather than about posting once.
	obligationID := f.obligation(t, voya, "500.00", now.AddDate(0, 0, -10), "month")

	for run := 1; run <= 2; run++ {
		if err := jobs.PostDueScheduledTransactions(f.ctx, f.pool, f.q, now); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}

		if n := f.countRows(t,
			`SELECT count(*) FROM transactions WHERE obligation_id = $1`, obligationID); n != 1 {
			t.Fatalf("after run %d: %d transactions, want 1", run, n)
		}
		if n := f.countRows(t,
			`SELECT count(*) FROM investment_transactions
			  WHERE account_id = $1 AND source = 'scheduled'`, voya); n != 1 {
			t.Fatalf("after run %d: %d investment transactions, want 1", run, n)
		}
		if n := f.countRows(t,
			`SELECT count(*) FROM account_balance_history
			  WHERE account_id = $1 AND reason = 'scheduled'`, voya); n != 1 {
			t.Fatalf("after run %d: %d balance history rows, want 1", run, n)
		}

		// The balance moved by exactly the contribution, once. This is the
		// figure a double-post would silently inflate.
		var balance decimal.Decimal
		if err := f.pool.QueryRow(f.ctx,
			`SELECT current_balance FROM accounts WHERE id = $1`, voya).Scan(&balance); err != nil {
			t.Fatalf("read balance: %v", err)
		}
		if !balance.Equal(decimal.RequireFromString("10500")) {
			t.Fatalf("after run %d: balance = %s, want 10500", run, balance)
		}
	}

	// Sign convention: money INTO the portfolio is negative in
	// investment_transactions, which is what reporting.ExternalFlows expects.
	// Getting this backwards inverts every return figure rather than erroring.
	var amount decimal.Decimal
	if err := f.pool.QueryRow(f.ctx,
		`SELECT amount FROM investment_transactions
		  WHERE account_id = $1 AND source = 'scheduled'`, voya).Scan(&amount); err != nil {
		t.Fatalf("read investment amount: %v", err)
	}
	if !amount.Equal(decimal.RequireFromString("-500")) {
		t.Errorf("investment transaction amount = %s, want -500", amount)
	}

	// transactions.amount keeps Plaid's convention: positive is money out.
	var txAmount decimal.Decimal
	if err := f.pool.QueryRow(f.ctx,
		`SELECT amount FROM transactions WHERE obligation_id = $1`, obligationID).Scan(&txAmount); err != nil {
		t.Fatalf("read transaction amount: %v", err)
	}
	if !txAmount.Equal(decimal.RequireFromString("500")) {
		t.Errorf("transaction amount = %s, want 500", txAmount)
	}
}

// TestScheduledPostingClampsMonthEnds is doc 13's cadence rule, now that it
// materialises rows rather than only projecting them. An obligation anchored on
// the 31st must land on the 28th in February, not roll into March — which is
// exactly what Go's time.AddDate would do, and why the expansion is in SQL.
func TestScheduledPostingClampsMonthEnds(t *testing.T) {
	f := newManualFixture(t)
	voya := f.account(t, "Voya 401(k)", "investment", true, "0")

	// Anchored 31 Jan; run as at 5 March so January and February are both due.
	anchor := time.Date(2026, time.January, 31, 0, 0, 0, 0, time.UTC)
	obligationID := f.obligation(t, voya, "100.00", anchor, "month")
	asAt := time.Date(2026, time.March, 5, 0, 0, 0, 0, time.UTC)

	// The 90-day floor is measured from `now`, so both occurrences are in range.
	if err := jobs.PostDueScheduledTransactions(f.ctx, f.pool, f.q, asAt); err != nil {
		t.Fatalf("post: %v", err)
	}

	rows, err := f.pool.Query(f.ctx,
		`SELECT date FROM transactions WHERE obligation_id = $1 ORDER BY date`, obligationID)
	if err != nil {
		t.Fatalf("read dates: %v", err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var d time.Time
		if err := rows.Scan(&d); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, d.Format(time.DateOnly))
	}

	want := []string{"2026-01-31", "2026-02-28"}
	if len(got) != len(want) {
		t.Fatalf("posted %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("occurrence %d = %s, want %s", i, got[i], want[i])
		}
	}
}

// TestScheduledPostingIgnoresAnEndedObligation: end_date in the past must post
// nothing and raise nothing. A worker that errored here would retry forever.
func TestScheduledPostingIgnoresAnEndedObligation(t *testing.T) {
	f := newManualFixture(t)
	voya := f.account(t, "Closed plan", "investment", true, "0")

	now := time.Now().UTC().Truncate(24 * time.Hour)
	obligationID := f.obligation(t, voya, "100.00", now.AddDate(0, -2, 0), "month")
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE recurring_obligations SET end_date = $2 WHERE id = $1`,
		obligationID, now.AddDate(0, 0, -45)); err != nil {
		t.Fatalf("set end date: %v", err)
	}

	if err := jobs.PostDueScheduledTransactions(f.ctx, f.pool, f.q, now); err != nil {
		t.Fatalf("post: %v", err)
	}
	// The occurrence one month ago falls after end_date, so nothing posts.
	if n := f.countRows(t,
		`SELECT count(*) FROM transactions WHERE obligation_id = $1 AND date > $2`,
		obligationID, now.AddDate(0, 0, -45)); n != 0 {
		t.Errorf("%d transactions posted past the end date, want 0", n)
	}
}

// TestBalanceHistoryUpsertsWithinADay: one balance per account per day, so a
// correction an hour after the first entry replaces it rather than creating a
// second row the trend would draw as a step.
func TestBalanceHistoryUpsertsWithinADay(t *testing.T) {
	f := newManualFixture(t)
	id := f.account(t, "Savings", "depository", true, "")

	asOf := time.Now().UTC().Truncate(24 * time.Hour)
	for _, v := range []string{"100.00", "250.00"} {
		if _, err := f.q.InsertAccountBalanceHistory(f.ctx,
			dbgen.InsertAccountBalanceHistoryParams{
				AccountID: id, AsOf: asOf,
				Balance: decimal.RequireFromString(v), Reason: "manual",
			}); err != nil {
			t.Fatalf("insert balance history %s: %v", v, err)
		}
	}

	rows, err := f.q.ListAccountBalanceHistory(f.ctx, dbgen.ListAccountBalanceHistoryParams{
		AccountID: id, HouseholdID: f.householdID, UserID: f.ownerID,
	})
	if err != nil {
		t.Fatalf("list balance history: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("%d history rows for one day, want 1", len(rows))
	}
	if !rows[0].Balance.Equal(decimal.RequireFromString("250")) {
		t.Errorf("balance = %s, want the corrected 250", rows[0].Balance)
	}
}
