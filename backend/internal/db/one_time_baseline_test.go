package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/testdb"
)

// TestOneTimeExclusionAndMedianBaselines pins the two mechanisms that stop a
// single unusual month from rewriting a household's idea of its fixed costs.
//
// The fixture is the real case that motivated them: five months of a $540.22
// car payment, then a month containing both the last payment and a $14,295.54
// PAYOFF. The payoff is real spending and must stay in July's Spending page. It
// must not become $2,383/month of "fixed bills" for the following six months.
func TestOneTimeExclusionAndMedianBaselines(t *testing.T) {
	url := testdb.URL(t)

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
	checkingID := uuid.New()
	loanPaymentsID := uuid.New()

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed exec: %v\n%s", err, sql)
		}
	}

	exec(`INSERT INTO households (id, name) VALUES ($1, 'Payoff Test')`, householdID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id = $1`, householdID)
	})
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
	      VALUES ($1, $2, $3, 'x', 'Tester')`, userID, householdID, userID.String()+"@example.test")
	exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, products, status)
	      VALUES ($1, $2, $3, '\x00', '{transactions}', 'active')`, itemID, userID, itemID.String())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type)
	      VALUES ($1, $2, $3, 'Checking', 'depository')`, checkingID, itemID, checkingID.String())
	exec(`INSERT INTO categories (id, household_id, name, slug, is_fixed)
	      VALUES ($1, $2, 'Loan Payments', 'loan-payments', TRUE)`, loanPaymentsID, householdID)

	// Five ordinary months, plus July's payment.
	for _, date := range []string{
		"2026-02-13", "2026-03-13", "2026-04-13", "2026-05-13", "2026-06-13", "2026-07-13",
	} {
		exec(`INSERT INTO transactions
		        (account_id, amount, currency, date, name, merchant_key, category_id, source)
		      VALUES ($1, '540.22', 'USD', $2, 'TOYOTA ACH RTL', 'toyota ach rtl', $3, 'plaid')`,
			checkingID, date, loanPaymentsID)
	}
	// The payoff, flagged one-time.
	exec(`INSERT INTO transactions
	        (account_id, amount, currency, date, name, merchant_key, category_id, source, is_one_time)
	      VALUES ($1, '14295.54', 'USD', '2026-07-14', 'TOYOTA ACH RTL', 'toyota ach rtl', $2, 'plaid', TRUE)`,
		checkingID, loanPaymentsID)

	julyStart := mustDate(t, "2026-07-01")
	julyEnd := mustDate(t, "2026-07-31")

	// --- The month it happened: the payoff counts. ---------------------------
	july, err := q.GetSpendingSummary(ctx, dbgen.GetSpendingSummaryParams{
		HouseholdID: householdID, UserID: userID, Date: julyStart, Date_2: julyEnd,
	})
	if err != nil {
		t.Fatalf("GetSpendingSummary: %v", err)
	}
	wantJuly := decimal.RequireFromString("14835.76") // 540.22 + 14295.54
	if !july.FixedSpending.Equal(wantJuly) {
		t.Errorf("July fixed_spending = %s, want %s — a one-time flag must not hide real spending",
			july.FixedSpending, wantJuly)
	}

	// --- The same query, opted into the baseline reading. --------------------
	julyBaseline, err := q.GetSpendingSummary(ctx, dbgen.GetSpendingSummaryParams{
		HouseholdID: householdID, UserID: userID, Date: julyStart, Date_2: julyEnd,
		ExcludeOneTime: true,
	})
	if err != nil {
		t.Fatalf("GetSpendingSummary (exclude_one_time): %v", err)
	}
	wantBaseline := decimal.RequireFromString("540.22")
	if !julyBaseline.FixedSpending.Equal(wantBaseline) {
		t.Errorf("July fixed_spending with exclude_one_time = %s, want %s",
			julyBaseline.FixedSpending, wantBaseline)
	}

	// --- The per-category monthly series safe-to-spend medians. --------------
	// Window is the six full months before August, i.e. Feb–Jul: the first month
	// in which the payoff would have poisoned the trailing average.
	rows, err := q.GetMonthlyFixedSpendByCategory(ctx, dbgen.GetMonthlyFixedSpendByCategoryParams{
		HouseholdID: householdID, UserID: userID,
		Date: mustDate(t, "2026-02-01"), Date_2: mustDate(t, "2026-07-31"),
		ExcludeOneTime: true,
	})
	if err != nil {
		t.Fatalf("GetMonthlyFixedSpendByCategory: %v", err)
	}
	if len(rows) != 6 {
		t.Fatalf("got %d monthly rows, want 6 (one per month Feb–Jul)", len(rows))
	}
	for _, r := range rows {
		if !r.Total.Equal(wantBaseline) {
			t.Errorf("%s total = %s, want %s — every month is an ordinary payment once the payoff is excluded",
				r.Month.Format("2006-01"), r.Total, wantBaseline)
		}
	}

	// Without the flag honoured, July carries the payoff and a MEAN over these
	// rows would read 2,922 rather than 540 — the failure mode in one number.
	unflagged, err := q.GetMonthlyFixedSpendByCategory(ctx, dbgen.GetMonthlyFixedSpendByCategoryParams{
		HouseholdID: householdID, UserID: userID,
		Date: mustDate(t, "2026-02-01"), Date_2: mustDate(t, "2026-07-31"),
	})
	if err != nil {
		t.Fatalf("GetMonthlyFixedSpendByCategory (unflagged): %v", err)
	}
	var julyTotal decimal.Decimal
	for _, r := range unflagged {
		if r.Month.Format("2006-01") == "2026-07" {
			julyTotal = r.Total
		}
	}
	if !julyTotal.Equal(wantJuly) {
		t.Errorf("unflagged July total = %s, want %s", julyTotal, wantJuly)
	}
}

// TestRecurringDetectorUsesMedianAmount pins the other half: the bill calendar
// must not promote a car loan at the average of its payments AND its payoff. The
// household never paid that figure once, and the loan no longer exists.
func TestRecurringDetectorUsesMedianAmount(t *testing.T) {
	url := testdb.URL(t)

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
	checkingID := uuid.New()
	loanPaymentsID := uuid.New()

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed exec: %v\n%s", err, sql)
		}
	}

	exec(`INSERT INTO households (id, name) VALUES ($1, 'Median Detector Test')`, householdID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id = $1`, householdID)
	})
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
	      VALUES ($1, $2, $3, 'x', 'Tester')`, userID, householdID, userID.String()+"@example.test")
	exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, products, status)
	      VALUES ($1, $2, $3, '\x00', '{transactions}', 'active')`, itemID, userID, itemID.String())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type)
	      VALUES ($1, $2, $3, 'Checking', 'depository')`, checkingID, itemID, checkingID.String())
	exec(`INSERT INTO categories (id, household_id, name, slug, is_fixed)
	      VALUES ($1, $2, 'Loan Payments', 'loan-payments', TRUE)`, loanPaymentsID, householdID)

	// Seven monthly payments (an odd count, so the median is an observed value),
	// then a payoff on the last one's heels. Dates run up to the `now` passed
	// below so the merchant has not gone quiet.
	for _, date := range []string{
		"2026-01-13", "2026-02-13", "2026-03-13", "2026-04-13",
		"2026-05-13", "2026-06-13", "2026-07-13",
	} {
		exec(`INSERT INTO transactions
		        (account_id, amount, currency, date, name, merchant_key, category_id, source)
		      VALUES ($1, '540.22', 'USD', $2, 'TOYOTA ACH RTL', 'toyota ach rtl', $3, 'plaid')`,
			checkingID, date, loanPaymentsID)
	}
	exec(`INSERT INTO transactions
	        (account_id, amount, currency, date, name, merchant_key, category_id, source)
	      VALUES ($1, '14295.54', 'USD', '2026-07-14', 'TOYOTA ACH RTL', 'toyota ach rtl', $2, 'plaid')`,
		checkingID, loanPaymentsID)

	rows, err := q.GetRecurringMerchants(ctx, dbgen.GetRecurringMerchantsParams{
		HouseholdID: householdID, UserID: userID,
		Date:    mustDate(t, "2023-07-01"),
		Column4: mustDate(t, "2026-07-30"),
	})
	if err != nil {
		t.Fatalf("GetRecurringMerchants: %v", err)
	}

	var found bool
	for _, r := range rows {
		if r.MerchantKey != "toyota ach rtl" {
			continue
		}
		found = true
		want := decimal.RequireFromString("540.22")
		if !r.TypicalAmount.Equal(want) {
			t.Errorf("typical_amount = %s, want %s — the mean here is 2261.14, which was never paid",
				r.TypicalAmount, want)
		}
	}
	if !found {
		t.Fatalf("toyota not detected as recurring; got %d rows", len(rows))
	}
}
