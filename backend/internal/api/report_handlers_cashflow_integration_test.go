package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// handleCashFlow, end to end against a real Postgres. The unit test covers the
// reconciliation maths in isolation; this one wires the handler to the actual
// queries and proves the SAME money rules every other report honours hold here
// too — transfers and card payments excluded, income and spending summed in
// NUMERIC, and the spending flows plus the uncategorised gap tying out to the
// headline the Spending page tiles show.
//
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' go test ./internal/api/
func TestHandleCashFlowReconcilesWithDB(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

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

	householdID, userID := uuid.New(), uuid.New()
	itemID, acctID := uuid.New(), uuid.New()
	paycheckID, rentID, foodID, transferID := uuid.New(), uuid.New(), uuid.New(), uuid.New()

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed exec: %v\n%s", err, sql)
		}
	}

	exec(`INSERT INTO households (id, name) VALUES ($1, 'Cash-flow Test')`, householdID)
	t.Cleanup(func() {
		// Cascades to items, accounts, transactions and household categories.
		_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id = $1`, householdID)
	})
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
	      VALUES ($1, $2, $3, 'x', 'Tester')`, userID, householdID, userID.String()+"@example.test")
	exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, products, status)
	      VALUES ($1, $2, $3, '\x00', '{transactions}', 'active')`, itemID, userID, itemID.String())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type)
	      VALUES ($1, $2, $3, 'Checking', 'depository')`, acctID, itemID, acctID.String())

	exec(`INSERT INTO categories (id, household_id, name, slug, is_income) VALUES ($1, $2, 'Paycheck', 'paycheck', TRUE)`, paycheckID, householdID)
	exec(`INSERT INTO categories (id, household_id, name, slug, is_fixed) VALUES ($1, $2, 'Rent', 'rent', TRUE)`, rentID, householdID)
	exec(`INSERT INTO categories (id, household_id, name, slug) VALUES ($1, $2, 'Food', 'food')`, foodID, householdID)
	// A transfer category: card payments and savings moves land here and must be
	// excluded from BOTH income and spending, exactly as they are everywhere else.
	exec(`INSERT INTO categories (id, household_id, name, slug, is_transfer) VALUES ($1, $2, 'Transfer Out', 'transfer-out', TRUE)`, transferID, householdID)

	addTx := func(amount, date, name string, cat *uuid.UUID) {
		t.Helper()
		var catArg any
		if cat != nil {
			catArg = *cat
		}
		exec(`INSERT INTO transactions
		        (account_id, amount, currency, date, name, merchant_name, merchant_key, category_id, source)
		      VALUES ($1, $2, 'USD', $3, $4, $5, $6, $7, 'plaid')`,
			acctID, amount, date, name, name, name, catArg)
	}

	// Income: two paychecks (deposits are negative on Plaid's sign convention).
	addTx("-2000.00", "2026-06-01", "ACME PAYROLL", &paycheckID)
	addTx("-1000.00", "2026-06-15", "ACME PAYROLL", &paycheckID)
	// Spending: rent + two food charges.
	addTx("2000.00", "2026-06-03", "LANDLORD", &rentID)
	addTx("300.00", "2026-06-05", "GROCER", &foodID)
	addTx("250.00", "2026-06-06", "DINER", &foodID)
	// An uncategorised charge: present in summary spending, absent from category
	// rows — the gap the handler must surface so the Sankey still balances.
	addTx("150.00", "2026-06-07", "MYSTERY", nil)
	// A transfer: must NOT appear in income or spending totals at all.
	addTx("500.00", "2026-06-08", "CARD PAYMENT", &transferID)

	rec := callCashFlow(t, srv, householdID, userID, "2026-01-01", "2026-12-31")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var got cashFlowResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, rec.Body.String())
	}

	assertDec(t, "income_total", got.IncomeTotal, "3000.00") // -(-2000 + -1000)
	assertDec(t, "spending_total", got.SpendingTotal, "2700.00") // 2000+300+250+150
	assertDec(t, "leftover", got.Leftover, "300.00")             // 3000 - 2700
	assertDec(t, "uncategorized_spending", got.UncategorizedSpending, "150.00")

	// Spending categories sum to the categorised portion; plus the gap = total.
	categorised := sumCats(got.SpendingCategories)
	assertDec(t, "spending categories sum", categorised, "2550.00") // 2000+300+250
	if total := categorised.Add(got.UncategorizedSpending); !total.Equal(got.SpendingTotal) {
		t.Errorf("categories (%s) + uncategorised (%s) = %s, want spending_total %s",
			categorised, got.UncategorizedSpending, total, got.SpendingTotal)
	}

	// Income sources sum to the income band — there is no uncategorised income.
	if incomeSum := sumSources(got.IncomeSources); !incomeSum.Equal(got.IncomeTotal) {
		t.Errorf("income sources sum to %s, want income_total %s", incomeSum, got.IncomeTotal)
	}

	// The transfer must not have leaked into any category or total.
	for _, c := range got.SpendingCategories {
		if c.Name == "Transfer Out" {
			t.Errorf("transfer category must be excluded from spending; got %v", c)
		}
	}
	if got.UncategorizedSpending.Equal(decimal.RequireFromString("650.00")) {
		t.Errorf("uncategorised = 650 looks like the transfer leaked in (150+500)")
	}
}

// The deficit path: spending outruns income, so leftover is negative. The chart
// uses that sign to draw a deficit inflow rather than a savings outflow, so the
// handler reporting it faithfully is the whole contract.
func TestHandleCashFlowDeficitWithDB(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

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
	householdID, userID := uuid.New(), uuid.New()
	itemID, acctID := uuid.New(), uuid.New()
	paycheckID, foodID := uuid.New(), uuid.New()

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed exec: %v\n%s", err, sql)
		}
	}
	exec(`INSERT INTO households (id, name) VALUES ($1, 'Cash-flow Deficit Test')`, householdID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id = $1`, householdID)
	})
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
	      VALUES ($1, $2, $3, 'x', 'Tester')`, userID, householdID, userID.String()+"@example.test")
	exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, products, status)
	      VALUES ($1, $2, $3, '\x00', '{transactions}', 'active')`, itemID, userID, itemID.String())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type)
	      VALUES ($1, $2, $3, 'Checking', 'depository')`, acctID, itemID, acctID.String())
	exec(`INSERT INTO categories (id, household_id, name, slug, is_income) VALUES ($1, $2, 'Paycheck', 'paycheck', TRUE)`, paycheckID, householdID)
	exec(`INSERT INTO categories (id, household_id, name, slug) VALUES ($1, $2, 'Food', 'food')`, foodID, householdID)
	exec(`INSERT INTO transactions (account_id, amount, currency, date, name, merchant_name, merchant_key, category_id, source)
	      VALUES ($1, '-1000.00', 'USD', '2026-06-01', 'ACME', 'ACME', 'acme', $2, 'plaid')`, acctID, paycheckID)
	exec(`INSERT INTO transactions (account_id, amount, currency, date, name, merchant_name, merchant_key, category_id, source)
	      VALUES ($1, '1500.00', 'USD', '2026-06-02', 'GROCER', 'GROCER', 'grocer', $2, 'plaid')`, acctID, foodID)

	rec := callCashFlow(t, srv, householdID, userID, "2026-01-01", "2026-12-31")

	var got cashFlowResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	assertDec(t, "income_total", got.IncomeTotal, "1000.00")
	assertDec(t, "spending_total", got.SpendingTotal, "1500.00")
	assertDec(t, "leftover (deficit)", got.Leftover, "-500.00")
}

func callCashFlow(t *testing.T, srv *Server, householdID, userID uuid.UUID, from, to string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/reports/cash-flow?from="+from+"&to="+to, nil)
	req = req.WithContext(auth.ContextWithIdentity(req.Context(), auth.Identity{
		UserID:      userID,
		HouseholdID: householdID,
		Role:        auth.RoleOwner,
	}))
	rec := httptest.NewRecorder()
	srv.handleCashFlow(rec, req)
	return rec
}

func assertDec(t *testing.T, label string, got decimal.Decimal, want string) {
	t.Helper()
	wantDec := decimal.RequireFromString(want)
	if !got.Equal(wantDec) {
		t.Errorf("%s = %s, want %s", label, got, want)
	}
}
