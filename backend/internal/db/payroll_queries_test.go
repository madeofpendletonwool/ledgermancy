package db

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/testdb"
)

// The two invariants doc 23 asks to be tested against a real database, because
// neither can be checked in Go: an unconfirmed paystub is inert, and one
// household member cannot read another's unshared one. Both live in SQL
// predicates, and a predicate that is wrong is wrong silently.
//
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' go test ./internal/db/
func TestPayrollQueries(t *testing.T) {
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
	alice := uuid.New()
	bob := uuid.New()
	itemID := uuid.New()
	acctID := uuid.New()

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed exec: %v\n%s", err, sql)
		}
	}

	exec(`INSERT INTO households (id, name) VALUES ($1, 'Payroll Test')`, householdID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id = $1`, householdID)
	})
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
	      VALUES ($1, $2, $3, 'x', 'Alice')`, alice, householdID, alice.String()+"@example.test")
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
	      VALUES ($1, $2, $3, 'x', 'Bob')`, bob, householdID, bob.String()+"@example.test")
	exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, products, status, is_shared)
	      VALUES ($1, $2, $3, '\x00', '{transactions}', 'active', TRUE)`, itemID, alice, itemID.String())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type)
	      VALUES ($1, $2, $3, 'Checking', 'depository')`, acctID, itemID, acctID.String())

	employer, err := q.CreateEmployer(ctx, dbgen.CreateEmployerParams{
		HouseholdID: householdID, Name: "Acme", PayFrequency: "biweekly",
	})
	if err != nil {
		t.Fatalf("create employer: %v", err)
	}

	dateOf := func(s string) time.Time {
		d, err := time.Parse(time.DateOnly, s)
		if err != nil {
			t.Fatalf("parse date: %v", err)
		}
		return d.UTC()
	}
	dec := func(s string) decimal.Decimal {
		d, err := decimal.NewFromString(s)
		if err != nil {
			t.Fatalf("parse decimal: %v", err)
		}
		return d
	}

	createStub := func(user uuid.UUID, payDate, gross, net string, confirmed, shared bool) dbgen.Paystub {
		t.Helper()
		var confirmedAt *time.Time
		if confirmed {
			now := time.Now().UTC()
			confirmedAt = &now
		}
		stub, err := q.CreatePaystub(ctx, dbgen.CreatePaystubParams{
			UserID: user, EmployerID: employer.ID,
			PeriodStart: dateOf(payDate).AddDate(0, 0, -14),
			PeriodEnd:   dateOf(payDate),
			PayDate:     dateOf(payDate),
			Gross:       dec(gross), Net: dec(net),
			Source: "manual", ConfirmedAt: confirmedAt, IsShared: shared,
		})
		if err != nil {
			t.Fatalf("create paystub: %v", err)
		}
		return stub
	}

	confirmedStub := createStub(alice, "2026-06-12", "3000.00", "2028.15", true, false)
	unconfirmedStub := createStub(alice, "2026-06-26", "9999.00", "9999.00", false, false)

	if _, err := q.CreatePaystubLine(ctx, dbgen.CreatePaystubLineParams{
		PaystubID: confirmedStub.ID, Category: "401k_pre_tax", Label: "401(k)",
		Amount: dec("300.00"), PreTax: true,
	}); err != nil {
		t.Fatalf("create line: %v", err)
	}
	// A deliberately enormous deduction on the UNCONFIRMED stub. If inertness
	// leaks anywhere, this is the value that shows up.
	if _, err := q.CreatePaystubLine(ctx, dbgen.CreatePaystubLineParams{
		PaystubID: unconfirmedStub.ID, Category: "401k_pre_tax", Label: "401(k)",
		Amount: dec("8000.00"), PreTax: true,
	}); err != nil {
		t.Fatalf("create line: %v", err)
	}

	t.Run("unconfirmed paystubs are inert", func(t *testing.T) {
		rows, err := q.ListConfirmedPaystubsForYear(ctx, dbgen.ListConfirmedPaystubsForYearParams{
			HouseholdID: householdID, UserID: alice, TaxYear: 2026,
		})
		if err != nil {
			t.Fatalf("list confirmed: %v", err)
		}
		if len(rows) != 1 || rows[0].ID != confirmedStub.ID {
			t.Fatalf("confirmed roll-up returned %d rows, want only the confirmed stub", len(rows))
		}

		totals, err := q.GetConfirmedGrossForPeriod(ctx, dbgen.GetConfirmedGrossForPeriodParams{
			HouseholdID: householdID, UserID: alice,
			FromDate: dateOf("2026-01-01"), ToDate: dateOf("2026-12-31"),
		})
		if err != nil {
			t.Fatalf("confirmed gross: %v", err)
		}
		// 3,000.00 and one stub — not 12,999.00 and two.
		if !totals.Gross.Equal(dec("3000.00")) || totals.PaystubCount != 1 {
			t.Errorf("gross = %s over %d stubs; want 3000.00 over 1 — the unconfirmed "+
				"stub must contribute nothing", totals.Gross, totals.PaystubCount)
		}

		years, err := q.ListPaystubYears(ctx, dbgen.ListPaystubYearsParams{
			HouseholdID: householdID, UserID: alice,
		})
		if err != nil {
			t.Fatalf("list years: %v", err)
		}
		if len(years) != 1 || years[0] != 2026 {
			t.Errorf("years = %v, want [2026]", years)
		}

		// The review queue, by contrast, is exactly the unconfirmed ones.
		unconfirmed, err := q.CountUnconfirmedPaystubs(ctx, dbgen.CountUnconfirmedPaystubsParams{
			HouseholdID: householdID, UserID: alice,
		})
		if err != nil {
			t.Fatalf("count unconfirmed: %v", err)
		}
		if unconfirmed != 1 {
			t.Errorf("unconfirmed count = %d, want 1", unconfirmed)
		}
	})

	t.Run("another member cannot read an unshared paystub", func(t *testing.T) {
		rows, err := q.ListPaystubs(ctx, dbgen.ListPaystubsParams{
			HouseholdID: householdID, UserID: bob,
		})
		if err != nil {
			t.Fatalf("list paystubs as bob: %v", err)
		}
		if len(rows) != 0 {
			t.Fatalf("bob can see %d of alice's unshared paystubs; want 0", len(rows))
		}

		if _, err := q.GetPaystub(ctx, dbgen.GetPaystubParams{
			ID: confirmedStub.ID, HouseholdID: householdID, UserID: bob,
		}); err == nil {
			t.Error("bob fetched alice's unshared paystub by id")
		}

		// And alice's own private stubs never reach bob's roll-ups either,
		// which is the failure that would show up as a wrong household figure
		// rather than as a visible leak.
		totals, err := q.GetConfirmedGrossForPeriod(ctx, dbgen.GetConfirmedGrossForPeriodParams{
			HouseholdID: householdID, UserID: bob,
			FromDate: dateOf("2026-01-01"), ToDate: dateOf("2026-12-31"),
		})
		if err != nil {
			t.Fatalf("confirmed gross as bob: %v", err)
		}
		if !totals.Gross.IsZero() || totals.PaystubCount != 0 {
			t.Errorf("bob's gross = %s over %d stubs; want zero", totals.Gross, totals.PaystubCount)
		}

		// Sharing is the opt-in, and it works.
		if _, err := q.SetPaystubSharing(ctx, dbgen.SetPaystubSharingParams{
			ID: confirmedStub.ID, UserID: alice, IsShared: true,
		}); err != nil {
			t.Fatalf("share paystub: %v", err)
		}
		rows, err = q.ListPaystubs(ctx, dbgen.ListPaystubsParams{
			HouseholdID: householdID, UserID: bob,
		})
		if err != nil {
			t.Fatalf("list paystubs as bob: %v", err)
		}
		if len(rows) != 1 {
			t.Errorf("bob sees %d shared paystubs, want 1", len(rows))
		}

		// Seeing it is still not owning it: the write path is scoped to the
		// owner and must not resolve for bob.
		if _, err := q.GetOwnedPaystub(ctx, dbgen.GetOwnedPaystubParams{
			ID: confirmedStub.ID, UserID: bob, HouseholdID: householdID,
		}); err == nil {
			t.Error("bob resolved alice's shared paystub through the write path")
		}
	})

	t.Run("identical deposits are proposed, never auto-matched", func(t *testing.T) {
		// The two-earner case: two deposits of exactly the stub's net pay,
		// three days apart, in the same account.
		exec(`INSERT INTO transactions (account_id, amount, currency, date, name, source)
		      VALUES ($1, '-2028.15', 'USD', '2026-06-12', 'ACME PAYROLL', 'plaid')`, acctID)
		exec(`INSERT INTO transactions (account_id, amount, currency, date, name, source)
		      VALUES ($1, '-2028.15', 'USD', '2026-06-11', 'GLOBEX PAYROLL', 'plaid')`, acctID)

		matches, err := q.MatchDepositsForPaystub(ctx, dbgen.MatchDepositsForPaystubParams{
			HouseholdID: householdID, UserID: alice,
			Net:      dec("2028.15"),
			PayDate:  dateOf("2026-06-12"),
			FromDate: dateOf("2026-06-07"), ToDate: dateOf("2026-06-17"),
			PaystubID: confirmedStub.ID, MaxResults: 10,
		})
		if err != nil {
			t.Fatalf("match deposits: %v", err)
		}
		if len(matches) != 2 {
			t.Fatalf("got %d candidates, want both identical deposits offered for a "+
				"human to choose between", len(matches))
		}
		for _, m := range matches {
			if !m.Delta.IsZero() {
				t.Errorf("delta = %s for an exact-amount deposit, want 0", m.Delta)
			}
		}

		// The stub is still unlinked: matching proposes and never writes.
		reread, err := q.GetPaystub(ctx, dbgen.GetPaystubParams{
			ID: confirmedStub.ID, HouseholdID: householdID, UserID: alice,
		})
		if err != nil {
			t.Fatalf("re-read paystub: %v", err)
		}
		if reread.TransactionID != nil {
			t.Error("the matcher wrote a link; it must only ever propose")
		}

		// Once a deposit is claimed by a DIFFERENT stub it stops being offered,
		// so two stubs can never be linked to the same deposit.
		claimed := matches[0].ID
		if _, err := q.SetPaystubTransaction(ctx, dbgen.SetPaystubTransactionParams{
			ID: unconfirmedStub.ID, UserID: alice, TransactionID: &claimed,
		}); err != nil {
			t.Fatalf("link deposit: %v", err)
		}
		matches, err = q.MatchDepositsForPaystub(ctx, dbgen.MatchDepositsForPaystubParams{
			HouseholdID: householdID, UserID: alice,
			Net:      dec("2028.15"),
			PayDate:  dateOf("2026-06-12"),
			FromDate: dateOf("2026-06-07"), ToDate: dateOf("2026-06-17"),
			PaystubID: confirmedStub.ID, MaxResults: 10,
		})
		if err != nil {
			t.Fatalf("match deposits: %v", err)
		}
		if len(matches) != 1 {
			t.Errorf("got %d candidates after one was claimed, want 1", len(matches))
		}
		for _, m := range matches {
			if m.ID == claimed {
				t.Error("a deposit already linked to another paystub was offered again")
			}
		}
	})

	t.Run("a duplicate import is refused", func(t *testing.T) {
		// The same PDF dropped in twice is the expected mistake, and it would
		// silently double every figure derived from period amounts.
		if _, err := q.CreatePaystub(ctx, dbgen.CreatePaystubParams{
			UserID: alice, EmployerID: employer.ID,
			PeriodStart: dateOf("2026-05-29"), PeriodEnd: dateOf("2026-06-12"),
			PayDate: dateOf("2026-06-12"),
			Gross:   dec("3000.00"), Net: dec("2028.15"),
			Source: "pdf",
		}); err == nil {
			t.Error("a second paystub for the same employer and pay date was accepted")
		}
	})
}
