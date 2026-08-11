package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/testdb"
)

// The counting-flags endpoint. The cases that matter: each flag moves
// independently (so a UI toggle never clobbers the other), the flags apply to
// Plaid-synced rows (a loan payoff arrives from Plaid — this is the whole
// point), and another household's transaction is a 404 rather than an edit.
//
//	TEST_DATABASE_URL='postgres://...' go test ./internal/api/ -run TestTransactionFlags
func TestTransactionFlagsEndpoint(t *testing.T) {
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

	householdID, userID, itemID, accountID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	payoffID := uuid.New()

	exec(`INSERT INTO households (id, name) VALUES ($1, 'Flags Test')`, householdID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id = $1`, householdID)
	})
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
	      VALUES ($1, $2, $3, 'x', 'Tester')`, userID, householdID, userID.String()+"@example.test")
	exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, products, status)
	      VALUES ($1, $2, $3, '\x00', '{transactions}', 'active')`, itemID, userID, itemID.String())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type)
	      VALUES ($1, $2, $3, 'Checking', 'depository')`, accountID, itemID, accountID.String())
	// source = 'plaid': the flags must work on synced rows.
	exec(`INSERT INTO transactions (id, account_id, amount, currency, date, name, source)
	      VALUES ($1, $2, '14295.54', 'USD', '2026-07-14', 'TOYOTA ACH RTL', 'plaid')`,
		payoffID, accountID)

	caller := auth.Identity{UserID: userID, HouseholdID: householdID, DisplayName: "Tester", Role: "adult"}
	patch := func(t *testing.T, id uuid.UUID, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodPatch,
			"/api/transactions/"+id.String()+"/flags", strings.NewReader(body))
		rc := chi.NewRouteContext()
		rc.URLParams.Add("transactionID", id.String())
		r = r.WithContext(auth.ContextWithIdentity(
			context.WithValue(ctx, chi.RouteCtxKey, rc), caller))

		rec := httptest.NewRecorder()
		srv.handleSetTransactionFlags(rec, r)
		return rec
	}
	decodeFlags := func(t *testing.T, rec *httptest.ResponseRecorder) transactionFlagsResponse {
		t.Helper()
		var out transactionFlagsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v (%s)", err, rec.Body.String())
		}
		return out
	}

	t.Run("marks a synced row one-time", func(t *testing.T) {
		rec := patch(t, payoffID, `{"is_one_time":true}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		got := decodeFlags(t, rec)
		if !got.IsOneTime {
			t.Error("is_one_time = false, want true")
		}
		if got.ExcludedFromReports {
			t.Error("excluded_from_reports was set as a side effect")
		}
	})

	t.Run("an omitted flag is left alone", func(t *testing.T) {
		// Set the other flag; the one_time set above must survive untouched.
		rec := patch(t, payoffID, `{"excluded_from_reports":true}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		got := decodeFlags(t, rec)
		if !got.ExcludedFromReports {
			t.Error("excluded_from_reports = false, want true")
		}
		if !got.IsOneTime {
			t.Error("is_one_time was cleared by a request that never mentioned it")
		}
	})

	t.Run("flags clear again", func(t *testing.T) {
		rec := patch(t, payoffID, `{"is_one_time":false,"excluded_from_reports":false}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		got := decodeFlags(t, rec)
		if got.IsOneTime || got.ExcludedFromReports {
			t.Errorf("flags = %+v, want both false — a flag must be reversible", got)
		}
	})

	t.Run("an empty body is rejected", func(t *testing.T) {
		if rec := patch(t, payoffID, `{}`); rec.Code != http.StatusBadRequest {
			t.Errorf("status %d, want 400 — a no-op write is a caller bug", rec.Code)
		}
	})

	t.Run("another household's transaction is not found", func(t *testing.T) {
		otherHousehold, otherUser := uuid.New(), uuid.New()
		otherItem, otherAccount := uuid.New(), uuid.New()
		otherTxn := uuid.New()

		exec(`INSERT INTO households (id, name) VALUES ($1, 'Someone Else')`, otherHousehold)
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id = $1`, otherHousehold)
		})
		exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
		      VALUES ($1, $2, $3, 'x', 'Other')`, otherUser, otherHousehold, otherUser.String()+"@example.test")
		exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, products, status)
		      VALUES ($1, $2, $3, '\x00', '{transactions}', 'active')`, otherItem, otherUser, otherItem.String())
		exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type)
		      VALUES ($1, $2, $3, 'Their Checking', 'depository')`, otherAccount, otherItem, otherAccount.String())
		exec(`INSERT INTO transactions (id, account_id, amount, currency, date, name, source)
		      VALUES ($1, $2, '10.00', 'USD', '2026-07-14', 'Coffee', 'plaid')`, otherTxn, otherAccount)

		if rec := patch(t, otherTxn, `{"is_one_time":true}`); rec.Code != http.StatusNotFound {
			t.Errorf("status %d, want 404 — a valid id from another household must not be editable", rec.Code)
		}

		var flagged bool
		if err := pool.QueryRow(ctx,
			`SELECT is_one_time FROM transactions WHERE id = $1`, otherTxn).Scan(&flagged); err != nil {
			t.Fatalf("read back: %v", err)
		}
		if flagged {
			t.Error("the other household's row was modified")
		}
	})
}
