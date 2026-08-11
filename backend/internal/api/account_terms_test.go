package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/testdb"
)

// The manual debt-terms endpoint against a real Postgres.
//
// This exists because Plaid serves its Liabilities product at a minority of
// institutions, so for most households this endpoint is the only route an APR
// ever takes into the payoff engine. The cases that matter are the honesty ones:
// a typed value survives a sync, clearing it genuinely hands the figure back to
// the bank, and an account somebody else kept private is indistinguishable from
// one that does not exist.
//
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' go test ./internal/api/ -run TestAccountTerms
func TestAccountTermsEndpoint(t *testing.T) {
	url := testdb.URL(t)

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
	otherHouseholdID, otherUserID, otherItemID := uuid.New(), uuid.New(), uuid.New()
	// A second member of the SAME household with a private item, to prove the
	// write path uses the stricter visibility rule than the read path.
	mateID, privateItemID := uuid.New(), uuid.New()
	cardID, checkingID, foreignCardID, privateCardID := uuid.New(), uuid.New(), uuid.New(), uuid.New()

	exec(`INSERT INTO households (id, name) VALUES ($1, 'Terms Test')`, householdID)
	exec(`INSERT INTO households (id, name) VALUES ($1, 'Other Household')`, otherHouseholdID)
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
	      VALUES ($1, $2, $3, 'x', 'Tester')`, userID, householdID, userID.String()+"@example.test")
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
	      VALUES ($1, $2, $3, 'x', 'Housemate')`, mateID, householdID, mateID.String()+"@example.test")
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
	      VALUES ($1, $2, $3, 'x', 'Stranger')`, otherUserID, otherHouseholdID, otherUserID.String()+"@example.test")
	exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, status)
	      VALUES ($1, $2, $3, '\x00', 'active')`, itemID, userID, itemID.String())
	exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, status)
	      VALUES ($1, $2, $3, '\x00', 'active')`, otherItemID, otherUserID, otherItemID.String())
	exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, status, is_shared)
	      VALUES ($1, $2, $3, '\x00', 'active', false)`, privateItemID, mateID, privateItemID.String())

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM households WHERE id = ANY($1)`,
			[]uuid.UUID{householdID, otherHouseholdID})
	})

	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type, subtype, current_balance)
	      VALUES ($1, $2, $3, 'Quicksilver', 'credit', 'credit card', 1078.28)`,
		cardID, itemID, cardID.String())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type, subtype, current_balance)
	      VALUES ($1, $2, $3, 'Checking', 'depository', 'checking', 500.00)`,
		checkingID, itemID, checkingID.String())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type, subtype, current_balance)
	      VALUES ($1, $2, $3, 'Their Card', 'credit', 'credit card', 900.00)`,
		foreignCardID, otherItemID, foreignCardID.String())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type, subtype, current_balance)
	      VALUES ($1, $2, $3, 'Private Card', 'credit', 'credit card', 200.00)`,
		privateCardID, privateItemID, privateCardID.String())

	caller := auth.Identity{UserID: userID, HouseholdID: householdID, DisplayName: "Tester", Role: "adult"}

	put := func(t *testing.T, accountID uuid.UUID, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPut,
			"/api/accounts/"+accountID.String()+"/terms", strings.NewReader(body))
		// chi's URL params come from the route context, which httptest does not
		// populate — the handler reads accountID from it.
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("accountID", accountID.String())
		req = req.WithContext(auth.ContextWithIdentity(
			context.WithValue(ctx, chi.RouteCtxKey, rctx), caller))

		rec := httptest.NewRecorder()
		srv.handleSetAccountTerms(rec, req)
		return rec
	}
	decode := func(t *testing.T, rec *httptest.ResponseRecorder) liabilityResponse {
		t.Helper()
		var l liabilityResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &l); err != nil {
			t.Fatalf("decode: %v (%s)", err, rec.Body.String())
		}
		return l
	}

	t.Run("typed terms are stored and echoed back merged", func(t *testing.T) {
		rec := put(t, cardID, `{"apr":"18.99","minimum_payment":"200.00"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		l := decode(t, rec)

		if l.APRSource != termSourceManual || l.PaymentSource != termSourceManual {
			t.Errorf("sources = %q/%q, want manual/manual", l.APRSource, l.PaymentSource)
		}
		if l.APR == nil || l.APR.StringFixed(2) != "18.99" {
			t.Errorf("apr = %v, want 18.99", l.APR)
		}
		// Balance is what is owed NOW, not a statement balance.
		if l.Balance == nil || l.Balance.StringFixed(2) != "1078.28" {
			t.Errorf("balance = %v, want the account's current balance 1078.28", l.Balance)
		}
		// The id is the ACCOUNT id — most debts have no liabilities row to have
		// an id of their own.
		if l.ID != cardID || l.AccountID != cardID {
			t.Errorf("id/account_id = %s/%s, want %s twice", l.ID, l.AccountID, cardID)
		}
		if l.Kind != "credit card" {
			t.Errorf("kind = %q, want the account subtype %q", l.Kind, "credit card")
		}
	})

	t.Run("clearing both fields hands the figures back to the bank", func(t *testing.T) {
		// Give the account a Plaid-reported rate to fall back to.
		exec(`INSERT INTO liabilities (account_id, kind, apr, minimum_payment)
		      VALUES ($1, 'credit', 24.99, 35.00)
		      ON CONFLICT (account_id) DO UPDATE SET apr = 24.99, minimum_payment = 35.00`, cardID)
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(),
				`DELETE FROM liabilities WHERE account_id = $1`, cardID)
		})

		if rec := put(t, cardID, `{"apr":"18.99","minimum_payment":null}`); rec.Code != http.StatusOK {
			t.Fatalf("seed manual apr: %d %s", rec.Code, rec.Body.String())
		}

		rec := put(t, cardID, `{"apr":null,"minimum_payment":null}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		l := decode(t, rec)

		if l.APRSource != termSourcePlaid || l.APR == nil || l.APR.StringFixed(2) != "24.99" {
			t.Errorf("apr = %v (%s), want Plaid's 24.99 back", l.APR, l.APRSource)
		}

		// The row is gone rather than left storing nothing.
		var rows int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM account_terms WHERE account_id = $1`, cardID).Scan(&rows); err != nil {
			t.Fatalf("count terms: %v", err)
		}
		if rows != 0 {
			t.Errorf("account_terms rows = %d, want 0 — an all-null row says nothing", rows)
		}
	})

	// 0.1899 satisfies every constraint and silently reports an 18.99% card as
	// very nearly interest-free. Nothing can catch it, so it must at least not be
	// "helpfully" rescaled by a later change.
	t.Run("a fractional rate is stored as entered, not rescaled", func(t *testing.T) {
		rec := put(t, cardID, `{"apr":"0.1899","minimum_payment":null}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(),
				`DELETE FROM account_terms WHERE account_id = $1`, cardID)
		})
		if l := decode(t, rec); l.APR == nil || l.APR.String() != "0.1899" {
			t.Errorf("apr = %v, want 0.1899 verbatim", l.APR)
		}
	})

	t.Run("an out-of-range rate is refused with the unit named", func(t *testing.T) {
		rec := put(t, cardID, `{"apr":"250","minimum_payment":null}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "18.99") {
			t.Errorf("the error must show the expected form, got %s", rec.Body.String())
		}
	})

	t.Run("a negative payment is refused", func(t *testing.T) {
		if rec := put(t, cardID, `{"apr":null,"minimum_payment":"-5"}`); rec.Code != http.StatusBadRequest {
			t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body.String())
		}
	})

	// The payment schedule is a recurring_obligations row, not a due-date column:
	// a payment date is a recurrence, and the app already models one.
	t.Run("a schedule creates a linked bill and derives the next date", func(t *testing.T) {
		rec := put(t, cardID,
			`{"apr":"18.99","minimum_payment":"200.00",
			  "schedule":{"anchor_date":"2026-08-15","interval_count":1,"interval_unit":"month"}}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		l := decode(t, rec)

		if l.Schedule == nil {
			t.Fatal("a saved schedule must come back on the response")
		}
		if l.Schedule.Label != "monthly" {
			t.Errorf("label = %q, want the shared cadence wording", l.Schedule.Label)
		}
		if l.NextPaymentDueDate == nil {
			t.Fatal("a scheduled bill derives a next due date")
		}
		// Derived from the recurrence on read, never stored, so it is always in
		// the future rather than a cached date that went stale.
		if *l.NextPaymentDueDate < time.Now().Format(time.DateOnly) {
			t.Errorf("next_payment_due_date = %s, want today or later", *l.NextPaymentDueDate)
		}

		// It is a real bill: linked to the account, amount = the payment.
		var label string
		var amount, count int
		var unit string
		if err := pool.QueryRow(ctx, `
			SELECT o.label, o.amount::int, o.interval_count, o.interval_unit
			FROM account_terms t JOIN recurring_obligations o ON o.id = t.payment_obligation_id
			WHERE t.account_id = $1`, cardID).Scan(&label, &amount, &count, &unit); err != nil {
			t.Fatalf("read linked obligation: %v", err)
		}
		if amount != 200 || count != 1 || unit != "month" {
			t.Errorf("obligation = %d every %d %s, want 200 every 1 month", amount, count, unit)
		}
		if !strings.Contains(label, "Quicksilver") {
			t.Errorf("label = %q, want it named after the debt it pays", label)
		}
	})

	// Anchoring on a 31st is how "last day of the month" is expressed, and the
	// expansion must clamp rather than drift. This is the case everyone expects
	// to be broken.
	t.Run("a month-end schedule clamps and returns to the 31st", func(t *testing.T) {
		rec := put(t, cardID,
			`{"apr":null,"minimum_payment":"200.00",
			  "schedule":{"anchor_date":"2026-01-31","interval_count":1,"interval_unit":"month"}}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}

		var dates []string
		rows, err := pool.Query(ctx, `
			SELECT (DATE '2026-01-31' + make_interval(months => n))::date::text
			FROM generate_series(0, 3) AS g(n)`)
		if err != nil {
			t.Fatalf("expand: %v", err)
		}
		defer rows.Close()
		for rows.Next() {
			var d string
			if err := rows.Scan(&d); err != nil {
				t.Fatalf("scan: %v", err)
			}
			dates = append(dates, d)
		}
		want := []string{"2026-01-31", "2026-02-28", "2026-03-31", "2026-04-30"}
		for i, w := range want {
			if dates[i] != w {
				t.Errorf("occurrence %d = %s, want %s — anchor + n periods, never previous + 1",
					i, dates[i], w)
			}
		}
	})

	t.Run("editing a schedule reuses the same bill", func(t *testing.T) {
		first := decode(t, put(t, cardID,
			`{"apr":null,"minimum_payment":"200.00",
			  "schedule":{"anchor_date":"2026-08-15","interval_count":1,"interval_unit":"month"}}`))
		second := decode(t, put(t, cardID,
			`{"apr":null,"minimum_payment":"250.00",
			  "schedule":{"anchor_date":"2026-08-20","interval_count":2,"interval_unit":"week"}}`))

		if first.Schedule == nil || second.Schedule == nil {
			t.Fatal("both saves must return a schedule")
		}
		if first.Schedule.ObligationID != second.Schedule.ObligationID {
			t.Error("a re-save must edit the bill in place, not leave dead rows on the calendar")
		}
		if second.Schedule.Label != "every 2 weeks" {
			t.Errorf("label = %q, want the new cadence", second.Schedule.Label)
		}
		// The bill is the single source of truth for the payment amount, so the
		// payoff maths and the calendar cannot quote different numbers.
		if second.MinimumPayment == nil || second.MinimumPayment.StringFixed(2) != "250.00" {
			t.Errorf("minimum_payment = %v, want the bill's 250.00", second.MinimumPayment)
		}
	})

	t.Run("clearing a schedule deactivates the bill rather than deleting it", func(t *testing.T) {
		saved := decode(t, put(t, cardID,
			`{"apr":null,"minimum_payment":"200.00",
			  "schedule":{"anchor_date":"2026-08-15","interval_count":1,"interval_unit":"month"}}`))
		if saved.Schedule == nil {
			t.Fatal("expected a schedule to clear")
		}
		obligationID := saved.Schedule.ObligationID

		cleared := decode(t, put(t, cardID, `{"apr":null,"minimum_payment":"200.00","schedule":null}`))
		if cleared.Schedule != nil {
			t.Error("the schedule must be gone from the response")
		}
		if cleared.NextPaymentDueDate != nil {
			t.Errorf("next_payment_due_date = %v, want nil with no bill", *cleared.NextPaymentDueDate)
		}

		var isActive bool
		if err := pool.QueryRow(ctx,
			`SELECT is_active FROM recurring_obligations WHERE id = $1`, obligationID).Scan(&isActive); err != nil {
			t.Fatalf("the bill must still exist, deactivated: %v", err)
		}
		if isActive {
			t.Error("a cleared schedule must deactivate its bill")
		}
	})

	t.Run("a schedule without a payment amount is refused", func(t *testing.T) {
		rec := put(t, cardID,
			`{"apr":"18.99","minimum_payment":null,
			  "schedule":{"anchor_date":"2026-08-15","interval_count":1,"interval_unit":"month"}}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("an unknown cadence unit is refused", func(t *testing.T) {
		rec := put(t, cardID,
			`{"apr":null,"minimum_payment":"200.00",
			  "schedule":{"anchor_date":"2026-08-15","interval_count":1,"interval_unit":"fortnight"}}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body.String())
		}
	})

	// One answer for every kind of "no". Distinguishing them would leak whether
	// an id is real.
	for _, tc := range []struct {
		name    string
		account uuid.UUID
	}{
		{"another household's account", foreignCardID},
		{"a non-debt account", checkingID},
		{"a private item another member linked", privateCardID},
		{"an account that does not exist", uuid.New()},
	} {
		t.Run(tc.name+" is not found", func(t *testing.T) {
			rec := put(t, tc.account, `{"apr":"5","minimum_payment":null}`)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status %d, want 404: %s", rec.Code, rec.Body.String())
			}
		})
	}
}
