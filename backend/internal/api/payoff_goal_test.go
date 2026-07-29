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
