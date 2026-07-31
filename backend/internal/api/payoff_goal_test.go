package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// The debt-payoff goal HTTP surface against a real Postgres.
//
// The cases that matter are the honesty ones: the balance to eliminate is
// captured server-side rather than taken from the client, progress counts debt
// retired rather than money held, a payment below the interest is reported as
// "never" rather than as a large number, and an account with no loan terms says
// so instead of rendering a schedule made of zeros.
//
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' go test ./internal/api/ -run TestPayoffGoal
func TestPayoffGoalEndpoints(t *testing.T) {
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

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, sql)
		}
	}

	householdID, userID, itemID := uuid.New(), uuid.New(), uuid.New()
	cardID, loanID, checkingID := uuid.New(), uuid.New(), uuid.New()
	mortgageID, clearedID := uuid.New(), uuid.New()

	exec(`INSERT INTO households (id, name) VALUES ($1, 'Payoff Test')`, householdID)
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
	      VALUES ($1, $2, $3, 'x', 'Tester')`, userID, householdID, userID.String()+"@example.test")
	exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, products, status)
	      VALUES ($1, $2, $3, '\x00', '{liabilities}', 'active')`, itemID, userID, itemID.String())
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id = $1`, householdID)
	})

	// A card that amortizes: $5,000 at 18.99%, minimum $200 — 33 payments.
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type, subtype, current_balance)
	      VALUES ($1, $2, $3, 'Rewards Card', 'credit', 'credit card', 5000.00)`,
		cardID, itemID, cardID.String())
	exec(`INSERT INTO liabilities (account_id, kind, apr, balance, minimum_payment)
	      VALUES ($1, 'credit', 18.99, 4800.00, 200.00)`, cardID)

	// The same debt on a $75 minimum, which is below the $79.13 it accrues.
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type, subtype, current_balance)
	      VALUES ($1, $2, $3, 'Store Card', 'credit', 'credit card', 5000.00)`,
		loanID, itemID, loanID.String())
	exec(`INSERT INTO liabilities (account_id, kind, apr, balance, minimum_payment)
	      VALUES ($1, 'credit', 18.99, 5000.00, 75.00)`, loanID)

	// A depository account, which has no liabilities row at all.
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type, subtype, current_balance)
	      VALUES ($1, $2, $3, 'Checking', 'depository', 'checking', 2500.00)`,
		checkingID, itemID, checkingID.String())

	// A mortgage with NO liabilities row — the ordinary case, not an edge one.
	// Plaid serves its Liabilities product at a minority of institutions, so most
	// real debts look exactly like this.
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type, subtype, current_balance)
	      VALUES ($1, $2, $3, 'Mortgage', 'loan', 'mortgage', 159201.08)`,
		mortgageID, itemID, mortgageID.String())

	// A cleared card: a debt account with nothing owed on it.
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type, subtype, current_balance)
	      VALUES ($1, $2, $3, 'Paid Card', 'credit', 'credit card', 0.00)`,
		clearedID, itemID, clearedID.String())

	caller := auth.Identity{UserID: userID, HouseholdID: householdID, DisplayName: "Tester", Role: "adult"}
	withCaller := func(r *http.Request) *http.Request {
		return r.WithContext(auth.ContextWithIdentity(ctx, caller))
	}
	post := func(t *testing.T, body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.handleCreateGoal(rec, withCaller(
			httptest.NewRequest(http.MethodPost, "/api/goals", strings.NewReader(body))))
		return rec
	}
	decodeGoal := func(t *testing.T, rec *httptest.ResponseRecorder) goalResponse {
		t.Helper()
		var g goalResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &g); err != nil {
			t.Fatalf("decode: %v (%s)", err, rec.Body.String())
		}
		return g
	}

	// A year out, so the required-payment path has a deadline to aim at.
	targetDate := time.Now().AddDate(1, 0, 0).Format(time.DateOnly)

	t.Run("target amount is captured from the account, not the client", func(t *testing.T) {
		rec := post(t, `{"kind":"debt_payoff","name":"Clear the card","account_id":"`+cardID.String()+`"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		g := decodeGoal(t, rec)

		// The balance to eliminate is the ACCOUNT's current balance, not the
		// liability's last statement balance of 4,800.
		if g.TargetAmount != "5000.00" {
			t.Errorf("target_amount = %s, want 5000.00 (the account balance)", g.TargetAmount)
		}
		// Nothing paid off yet: progress is original − owed, which is zero.
		if g.CurrentAmount != "0.00" {
			t.Errorf("current_amount = %s, want 0.00 — progress is debt retired, not money held", g.CurrentAmount)
		}
		if g.Kind != goalKindPayoff {
			t.Errorf("kind = %s, want debt_payoff", g.Kind)
		}
	})

	t.Run("an amortizing card reports its schedule", func(t *testing.T) {
		rec := post(t, `{"kind":"debt_payoff","name":"Card by next year","account_id":"`+
			cardID.String()+`","target_date":"`+targetDate+`"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		g := decodeGoal(t, rec)

		if g.Payoff == nil {
			t.Fatal("a payoff goal must carry a payoff block")
		}
		p := g.Payoff
		if !p.Available {
			t.Fatalf("expected an available schedule, got reason %q", p.Reason)
		}
		if p.NeverPaysOff {
			t.Fatal("$200/mo against $5,000 at 18.99% does amortize")
		}
		// The hand-checked fixture: 33 payments, $1,414.47 of interest.
		if p.Months != 33 {
			t.Errorf("months = %d, want 33", p.Months)
		}
		if p.TotalInterest != "1414.47" {
			t.Errorf("total_interest = %s, want 1414.47", p.TotalInterest)
		}
		if p.MonthlyInterest != "79.13" {
			t.Errorf("monthly_interest = %s, want 79.13", p.MonthlyInterest)
		}
		if p.Balance != "5000.00" || p.MonthlyPayment != "200.00" || p.APR != "18.99" {
			t.Errorf("terms = %s at %s%% paying %s, want 5000.00 / 18.99 / 200.00",
				p.Balance, p.APR, p.MonthlyPayment)
		}
		if p.PayoffDate == nil {
			t.Error("an amortizing debt has a payoff date")
		}

		// 33 months is past a 12-month deadline, so the goal is behind — and the
		// required payment is interest-aware, comfortably above 5000/12 = 416.67.
		if g.OnTrack {
			t.Error("$200/mo cannot clear the card within a year")
		}
		if !p.TargetReachable {
			t.Fatal("a one-year deadline on a $5,000 card is reachable")
		}
		required := decFromString(t, p.RequiredMonthly)
		if !required.GreaterThan(decFromString(t, "416.67")) {
			t.Errorf("required_monthly = %s, must exceed the interest-free 5000/12 = 416.67",
				p.RequiredMonthly)
		}
	})

	t.Run("a payment below the interest is never, not a big number", func(t *testing.T) {
		rec := post(t, `{"kind":"debt_payoff","name":"Store card","account_id":"`+loanID.String()+`"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		g := decodeGoal(t, rec)

		if g.Payoff == nil || !g.Payoff.Available {
			t.Fatalf("expected an available schedule for a card with terms, got %+v", g.Payoff)
		}
		if !g.Payoff.NeverPaysOff {
			t.Fatalf("$75/mo is below the $79.13 interest, got %d months", g.Payoff.Months)
		}
		if g.Payoff.Months != 0 || g.Payoff.TotalInterest != "0.00" || g.Payoff.PayoffDate != nil {
			t.Errorf("a never-paying debt reports no schedule: %+v", g.Payoff)
		}
		if g.Payoff.MonthlyInterest != "79.13" {
			t.Errorf("monthly_interest = %s, want 79.13 — the figure that explains 'never'",
				g.Payoff.MonthlyInterest)
		}
	})

	t.Run("a non-debt account is refused", func(t *testing.T) {
		rec := post(t, `{"kind":"debt_payoff","name":"Pay off checking","account_id":"`+checkingID.String()+`"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("a payoff goal with no account is refused", func(t *testing.T) {
		rec := post(t, `{"kind":"debt_payoff","name":"Pay off something"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("an unknown kind is refused", func(t *testing.T) {
		rec := post(t, `{"kind":"retirement","name":"Retire","target_amount":"100.00"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body.String())
		}
	})

	// THE REGRESSION. A household with a mortgage and two credit cards could not
	// create a single payoff goal, because the gate was "did Plaid serve loan
	// terms for this account?" rather than "is this account a debt?". Plaid
	// serves that product at a minority of institutions, so the answer was no
	// every time and the picker was empty.
	t.Run("a debt account with no liabilities row is accepted", func(t *testing.T) {
		rec := post(t, `{"kind":"debt_payoff","name":"Clear the mortgage","account_id":"`+
			mortgageID.String()+`","target_date":"`+targetDate+`"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status %d, want 201 — a loan with no reported terms is still a debt: %s",
				rec.Code, rec.Body.String())
		}
		g := decodeGoal(t, rec)

		if g.TargetAmount != "159201.08" {
			t.Errorf("target_amount = %s, want 159201.08 captured from the account", g.TargetAmount)
		}
		if g.Payoff == nil {
			t.Fatal("a payoff goal must carry a payoff block")
		}
		if g.Payoff.Available {
			t.Error("with no rate and no payment there is no schedule to report")
		}
		if g.Payoff.APRSource != "" || g.Payoff.PaymentSource != "" {
			t.Errorf("sources = %q/%q, want empty — nobody has supplied either figure",
				g.Payoff.APRSource, g.Payoff.PaymentSource)
		}
		if !strings.Contains(g.Payoff.Reason, "Accounts page") {
			t.Errorf("reason must point at the fix, got %q", g.Payoff.Reason)
		}
		// The number that makes the goal useful even with no terms at all.
		if !g.Payoff.TargetReachable || g.Payoff.RequiredMonthly == "0.00" {
			t.Errorf("a deadline still yields a required payment, got %+v", g.Payoff)
		}
	})

	t.Run("manual terms produce a real schedule", func(t *testing.T) {
		exec(`INSERT INTO account_terms (account_id, apr, minimum_payment)
		      VALUES ($1, 6.5, 1200.00)`, mortgageID)
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(),
				`DELETE FROM account_terms WHERE account_id = $1`, mortgageID)
		})

		rec := post(t, `{"kind":"debt_payoff","name":"Mortgage with terms","account_id":"`+
			mortgageID.String()+`"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		g := decodeGoal(t, rec)

		if g.Payoff == nil || !g.Payoff.Available {
			t.Fatalf("typed terms must produce a schedule, got %+v", g.Payoff)
		}
		if g.Payoff.APRSource != "manual" || g.Payoff.PaymentSource != "manual" {
			t.Errorf("sources = %q/%q, want manual/manual",
				g.Payoff.APRSource, g.Payoff.PaymentSource)
		}
		// THREE decimals, because this account is a loan: note rates are quoted
		// in eighths, so rounding to two places renders a 6.775% mortgage as
		// "6.78%" — a figure on no statement the household read it off of. See
		// aprDecimals (debt_terms.go) and payoffResponse.APR's comment.
		if g.Payoff.APR != "6.500" || g.Payoff.MonthlyPayment != "1200.00" {
			t.Errorf("terms = %s%% paying %s, want 6.500 / 1200.00",
				g.Payoff.APR, g.Payoff.MonthlyPayment)
		}
		if g.Payoff.PayoffDate == nil || g.Payoff.Months == 0 {
			t.Error("a schedule built on typed terms has a date and a month count")
		}
		if g.Payoff.TotalInterest == "0.00" {
			t.Error("a 6.5% mortgage accrues interest")
		}
	})

	// Per-field precedence. Taking the whole row from one source would throw
	// away half of what is known.
	t.Run("manual terms beat plaid terms, field by field", func(t *testing.T) {
		exec(`INSERT INTO account_terms (account_id, apr) VALUES ($1, 9.99)`, cardID)
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(),
				`DELETE FROM account_terms WHERE account_id = $1`, cardID)
		})

		rec := post(t, `{"kind":"debt_payoff","name":"Card at my rate","account_id":"`+cardID.String()+`"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		g := decodeGoal(t, rec)

		if g.Payoff == nil {
			t.Fatal("expected a payoff block")
		}
		if g.Payoff.APR != "9.99" || g.Payoff.APRSource != "manual" {
			t.Errorf("apr = %s (%s), want 9.99 (manual) overriding Plaid's 18.99",
				g.Payoff.APR, g.Payoff.APRSource)
		}
		// The payment was never typed, so it still comes from the institution.
		if g.Payoff.MonthlyPayment != "200.00" || g.Payoff.PaymentSource != "plaid" {
			t.Errorf("payment = %s (%s), want 200.00 (plaid) — precedence is per field, not per row",
				g.Payoff.MonthlyPayment, g.Payoff.PaymentSource)
		}
	})

	// The constraint the whole account_terms table exists to satisfy. If anyone
	// later "simplifies" this by folding manual terms into the liabilities row,
	// this is the test that stops them: UpsertLiability rewrites every column on
	// conflict, so the typed rate would be gone on the next sync.
	t.Run("a plaid sync does not clobber manual terms", func(t *testing.T) {
		exec(`INSERT INTO account_terms (account_id, apr) VALUES ($1, 9.99)`, cardID)
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(),
				`DELETE FROM account_terms WHERE account_id = $1`, cardID)
		})

		// Exactly what SyncLiabilities runs.
		if err := srv.Queries.UpsertLiability(ctx, dbgen.UpsertLiabilityParams{
			AccountID:      cardID,
			Kind:           "credit",
			Apr:            decNullable(t, "18.99"),
			Balance:        decNullable(t, "4800.00"),
			MinimumPayment: decNullable(t, "200.00"),
		}); err != nil {
			t.Fatalf("upsert liability: %v", err)
		}

		rec := post(t, `{"kind":"debt_payoff","name":"Card after sync","account_id":"`+cardID.String()+`"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		g := decodeGoal(t, rec)

		if g.Payoff == nil || g.Payoff.APR != "9.99" || g.Payoff.APRSource != "manual" {
			t.Errorf("a sync must not overwrite a typed rate, got %+v", g.Payoff)
		}
	})

	// Newly reachable: a liabilities row without an APR is rare, but a household
	// that types a payment and leaves the rate blank is not.
	t.Run("a payment with no rate is a real schedule", func(t *testing.T) {
		exec(`INSERT INTO account_terms (account_id, minimum_payment)
		      VALUES ($1, 1500.00)`, mortgageID)
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(),
				`DELETE FROM account_terms WHERE account_id = $1`, mortgageID)
		})

		rec := post(t, `{"kind":"debt_payoff","name":"Mortgage, payment only","account_id":"`+
			mortgageID.String()+`"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		g := decodeGoal(t, rec)

		if g.Payoff == nil || !g.Payoff.Available {
			t.Fatalf("a known payment is enough for a schedule, got %+v", g.Payoff)
		}
		if g.Payoff.APRSource != "" {
			t.Errorf("apr_source = %q, want empty so the UI can caveat the date", g.Payoff.APRSource)
		}
		// 159201.08 / 1500 = 106.13 → 107 interest-free payments.
		if g.Payoff.Months != 107 {
			t.Errorf("months = %d, want 107 — a zero rate amortizes interest-free", g.Payoff.Months)
		}
		if g.Payoff.TotalInterest != "0.00" {
			t.Errorf("total_interest = %s, want 0.00 with no rate", g.Payoff.TotalInterest)
		}
	})

	// Pins the check the picker now mirrors client-side.
	t.Run("a debt account with nothing owed is refused", func(t *testing.T) {
		rec := post(t, `{"kind":"debt_payoff","name":"Pay off the paid card","account_id":"`+
			clearedID.String()+`"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("progress counts debt retired as the balance falls", func(t *testing.T) {
		rec := post(t, `{"kind":"debt_payoff","name":"Paying it down","account_id":"`+cardID.String()+`"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		created := decodeGoal(t, rec)

		// The user pays $1,000 off; the target stays at the captured original.
		exec(`UPDATE accounts SET current_balance = 4000.00 WHERE id = $1`, cardID)
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(),
				`UPDATE accounts SET current_balance = 5000.00 WHERE id = $1`, cardID)
		})

		g, err := srv.Queries.GetGoal(ctx, dbgen.GetGoalParams{
			ID: created.ID, HouseholdID: householdID, UserID: &userID, AllPersonGoals: true,
		})
		if err != nil {
			t.Fatalf("get goal: %v", err)
		}
		out, err := srv.buildGoalResponse(ctx, g, time.Now())
		if err != nil {
			t.Fatalf("build goal response: %v", err)
		}

		if out.TargetAmount != "5000.00" {
			t.Errorf("target_amount = %s, want the captured original 5000.00 — re-reading the "+
				"balance would pin progress at zero forever", out.TargetAmount)
		}
		if out.CurrentAmount != "1000.00" {
			t.Errorf("current_amount = %s, want 1000.00 (5000 original − 4000 still owed)", out.CurrentAmount)
		}
		if out.Payoff == nil || out.Payoff.Balance != "4000.00" {
			t.Errorf("the schedule must amortize what is still owed, got %+v", out.Payoff)
		}
	})
}
