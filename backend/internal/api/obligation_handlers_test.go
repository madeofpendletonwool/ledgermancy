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

// The bill calendar's HTTP surface, end to end against a real Postgres: a manual
// bill round-trips with a derived next-due date, a bad body is refused before it
// reaches the table, and neither reading nor deleting reaches another household.
//
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' go test ./internal/api/
func TestObligationEndpoints(t *testing.T) {
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

	callerHousehold, callerUser := uuid.New(), uuid.New()
	otherHousehold, otherUser := uuid.New(), uuid.New()
	seedHousehold := func(hh, u uuid.UUID, name string) {
		exec(`INSERT INTO households (id, name) VALUES ($1, $2)`, hh, name)
		exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
		      VALUES ($1, $2, $3, 'x', 'Tester')`, u, hh, u.String()+"@example.test")
	}
	seedHousehold(callerHousehold, callerUser, "Obligations Caller")
	seedHousehold(otherHousehold, otherUser, "Obligations Other")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id = ANY($1)`,
			[]uuid.UUID{callerHousehold, otherHousehold})
	})

	// A bill in the other household, and a detected one in the caller's — the
	// latter to check that "delete" retires it rather than removing it, which is
	// the only outcome the promotion pass would not immediately undo.
	foreignID := uuid.New()
	exec(`INSERT INTO recurring_obligations
	      (id, household_id, user_id, label, amount, interval_count, interval_unit, anchor_date)
	      VALUES ($1, $2, $3, 'Not yours', '99.00', 1, 'month', '2026-07-01')`,
		foreignID, otherHousehold, otherUser)

	detectedID := uuid.New()
	exec(`INSERT INTO recurring_obligations
	      (id, household_id, label, amount, interval_count, interval_unit, anchor_date, source, merchant_key)
	      VALUES ($1, $2, 'Streaming', '12.00', 1, 'month', '2026-07-08', 'detected', 'streaming')`,
		detectedID, callerHousehold)

	caller := auth.Identity{
		UserID: callerUser, HouseholdID: callerHousehold, DisplayName: "Tester",
	}
	withCaller := func(r *http.Request) *http.Request {
		return r.WithContext(auth.ContextWithIdentity(ctx, caller))
	}
	// Stands in for the router, which is what normally resolves {obligationID}.
	withParam := func(r *http.Request, id uuid.UUID) *http.Request {
		routeCtx := chi.NewRouteContext()
		routeCtx.URLParams.Add("obligationID", id.String())
		return r.WithContext(context.WithValue(
			auth.ContextWithIdentity(ctx, caller), chi.RouteCtxKey, routeCtx))
	}

	var created obligationResponse

	t.Run("create derives a next due date without storing one", func(t *testing.T) {
		// Anchored on the 31st, so the derived dates have to clamp rather than
		// spill into the following month.
		body := `{"label":"Rent","amount":"1200.00","interval_count":1,` +
			`"interval_unit":"month","anchor_date":"2026-01-31"}`
		req := withCaller(httptest.NewRequest(http.MethodPost, "/api/obligations", strings.NewReader(body)))
		rec := httptest.NewRecorder()

		srv.handleCreateObligation(rec, req)

		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body: %s", rec.Code, rec.Body.String())
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if created.Amount != "1200.00" {
			t.Errorf("amount = %q, want a fixed-2 string \"1200.00\"", created.Amount)
		}
		if created.Cadence != "monthly" {
			t.Errorf("cadence = %q, want \"monthly\"", created.Cadence)
		}
		if created.MonthlyEstimate != "1200.00" {
			t.Errorf("monthly_estimate = %q, want \"1200.00\"", created.MonthlyEstimate)
		}
		if created.NextDue == nil {
			t.Fatal("next_due is null; it must be derived on create, not left for a later read")
		}
		// Whatever today is, the next occurrence must be a real clamped month end
		// derived from the 31st — never a 31st in a month that has none.
		next, perr := time.Parse(time.DateOnly, *created.NextDue)
		if perr != nil {
			t.Fatalf("next_due %q does not parse: %v", *created.NextDue, perr)
		}
		if next.Day() != 31 && next.Day() != next.AddDate(0, 1, -next.Day()).Day() {
			t.Errorf("next_due = %s, which is neither the 31st nor a month end", *created.NextDue)
		}
	})

	t.Run("a bad body is refused before it reaches the table", func(t *testing.T) {
		cases := []struct{ name, body string }{
			{"no label", `{"label":"","amount":"10.00","interval_count":1,"interval_unit":"month","anchor_date":"2026-01-01"}`},
			{"zero amount", `{"label":"x","amount":"0","interval_count":1,"interval_unit":"month","anchor_date":"2026-01-01"}`},
			{"amount not a decimal", `{"label":"x","amount":"lots","interval_count":1,"interval_unit":"month","anchor_date":"2026-01-01"}`},
			{"unknown unit", `{"label":"x","amount":"10.00","interval_count":1,"interval_unit":"fortnight","anchor_date":"2026-01-01"}`},
			{"zero interval", `{"label":"x","amount":"10.00","interval_count":0,"interval_unit":"month","anchor_date":"2026-01-01"}`},
			{"bad anchor", `{"label":"x","amount":"10.00","interval_count":1,"interval_unit":"month","anchor_date":"01/01/2026"}`},
			{"end before anchor", `{"label":"x","amount":"10.00","interval_count":1,"interval_unit":"month","anchor_date":"2026-06-01","end_date":"2026-01-01"}`},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				req := withCaller(httptest.NewRequest(http.MethodPost, "/api/obligations", strings.NewReader(tc.body)))
				rec := httptest.NewRecorder()
				srv.handleCreateObligation(rec, req)
				if rec.Code != http.StatusBadRequest {
					t.Errorf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
				}
			})
		}
	})

	t.Run("the list is scoped to the caller's household", func(t *testing.T) {
		req := withCaller(httptest.NewRequest(http.MethodGet, "/api/obligations", nil))
		rec := httptest.NewRecorder()
		srv.handleListObligations(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
		}
		var list []obligationResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
			t.Fatalf("decode: %v", err)
		}
		for _, o := range list {
			if o.ID == foreignID {
				t.Fatal("another household's obligation is visible")
			}
		}
		if len(list) != 2 {
			t.Errorf("got %d obligations, want the caller's 2", len(list))
		}
	})

	t.Run("editing stamps user_edited so promotion stops overwriting it", func(t *testing.T) {
		body := `{"label":"Streaming plus","amount":"15.00","interval_count":1,` +
			`"interval_unit":"month","anchor_date":"2026-07-08"}`
		req := withParam(httptest.NewRequest(http.MethodPut, "/api/obligations/x", strings.NewReader(body)), detectedID)
		rec := httptest.NewRecorder()

		srv.handleUpdateObligation(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
		}
		var updated obligationResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !updated.UserEdited {
			t.Error("user_edited was not stamped, so the next promotion pass would revert this")
		}
		if updated.Label != "Streaming plus" {
			t.Errorf("label = %q, want the edit to stick", updated.Label)
		}
	})

	t.Run("editing another household's obligation is a 404", func(t *testing.T) {
		body := `{"label":"Mine now","amount":"1.00","interval_count":1,` +
			`"interval_unit":"month","anchor_date":"2026-07-01"}`
		req := withParam(httptest.NewRequest(http.MethodPut, "/api/obligations/x", strings.NewReader(body)), foreignID)
		rec := httptest.NewRecorder()

		srv.handleUpdateObligation(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body: %s", rec.Code, rec.Body.String())
		}
		var row dbgen.RecurringObligation
		if err := pool.QueryRow(ctx,
			`SELECT label FROM recurring_obligations WHERE id = $1`, foreignID).Scan(&row.Label); err != nil {
			t.Fatalf("reload: %v", err)
		}
		if row.Label != "Not yours" {
			t.Errorf("the other household's obligation was modified: label = %q", row.Label)
		}
	})

	t.Run("deleting a detected obligation retires it instead", func(t *testing.T) {
		req := withParam(httptest.NewRequest(http.MethodDelete, "/api/obligations/x", nil), detectedID)
		rec := httptest.NewRecorder()

		srv.handleDeleteObligation(rec, req)

		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204; body: %s", rec.Code, rec.Body.String())
		}
		// Still there, but inactive: a hard delete would be undone by the next
		// promotion pass, which is exactly what the user asked not to happen.
		var isActive bool
		if err := pool.QueryRow(ctx,
			`SELECT is_active FROM recurring_obligations WHERE id = $1`, detectedID).Scan(&isActive); err != nil {
			t.Fatalf("a detected obligation was hard-deleted: %v", err)
		}
		if isActive {
			t.Error("a deleted detected obligation is still active")
		}
	})

	t.Run("deleting a manual obligation removes it", func(t *testing.T) {
		req := withParam(httptest.NewRequest(http.MethodDelete, "/api/obligations/x", nil), created.ID)
		rec := httptest.NewRecorder()

		srv.handleDeleteObligation(rec, req)

		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204; body: %s", rec.Code, rec.Body.String())
		}
		var count int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM recurring_obligations WHERE id = $1`, created.ID).Scan(&count); err != nil {
			t.Fatalf("count: %v", err)
		}
		if count != 0 {
			t.Error("a manual obligation survived deletion")
		}
	})

	t.Run("deleting another household's obligation is a 404", func(t *testing.T) {
		req := withParam(httptest.NewRequest(http.MethodDelete, "/api/obligations/x", nil), foreignID)
		rec := httptest.NewRecorder()

		srv.handleDeleteObligation(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body: %s", rec.Code, rec.Body.String())
		}
		var count int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM recurring_obligations WHERE id = $1`, foreignID).Scan(&count); err != nil {
			t.Fatalf("count: %v", err)
		}
		if count != 1 {
			t.Error("another household's obligation was deleted")
		}
	})

	t.Run("the projection horizon is clamped", func(t *testing.T) {
		// A caller asking for ten years gets a year: past a few months the answer
		// is dominated by obligations nobody has entered yet.
		req := withCaller(httptest.NewRequest(http.MethodGet, "/api/obligations/projection?days=4000", nil))
		rec := httptest.NewRecorder()

		srv.handleObligationProjection(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
		}
		var resp balanceProjectionResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		from, _ := time.Parse(time.DateOnly, resp.From)
		to, _ := time.Parse(time.DateOnly, resp.To)
		if days := int(to.Sub(from).Hours() / 24); days != 365 {
			t.Errorf("horizon = %d days, want it clamped to 365", days)
		}
	})
}
