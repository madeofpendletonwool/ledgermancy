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
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/config"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/crypto"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/plaid"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/testdb"
)

// Reconnect mints a Link token against a live bank connection and clears an
// item's error state, so both endpoints must refuse an item the caller does not
// own. The Plaid client here points at sandbox with junk credentials and the
// test never reaches the network: a request that got as far as calling Plaid
// would fail rather than return 404, which is exactly the regression to catch.
//
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' go test ./internal/api/
func TestItemReconnectOwnership(t *testing.T) {
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

	cipher, err := crypto.New(make([]byte, 32))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	plaidClient, err := plaid.New(config.PlaidConfig{
		Env:      "sandbox",
		ClientID: "test-client",
		Secret:   "test-secret",
		Products: []string{"transactions"},
	})
	if err != nil {
		t.Fatalf("plaid client: %v", err)
	}

	srv := &Server{
		Pool:    pool,
		Queries: dbgen.New(pool),
		Cipher:  cipher,
		Plaid:   plaidClient,
	}

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed exec: %v\n%s", err, sql)
		}
	}

	callerHousehold, callerUser := uuid.New(), uuid.New()
	otherHousehold, otherUser, otherItem := uuid.New(), uuid.New(), uuid.New()

	seedHousehold := func(hh, u uuid.UUID, name string) {
		exec(`INSERT INTO households (id, name) VALUES ($1, $2)`, hh, name)
		exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
		      VALUES ($1, $2, $3, 'x', 'Tester')`, u, hh, u.String()+"@example.test")
	}
	seedHousehold(callerHousehold, callerUser, "Reconnect Caller")
	seedHousehold(otherHousehold, otherUser, "Reconnect Other")

	sealed, err := cipher.SealString("access-sandbox-not-yours")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	seedBrokenItem := func(item, owner uuid.UUID) {
		exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, products, status, error_code)
		      VALUES ($1, $2, $3, $4, '{transactions}', 'login_required', 'ITEM_LOGIN_REQUIRED')`,
			item, owner, item.String(), sealed)
	}
	callerItem := uuid.New()
	seedBrokenItem(otherItem, otherUser)
	seedBrokenItem(callerItem, callerUser)

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id = ANY($1)`,
			[]uuid.UUID{callerHousehold, otherHousehold})
	})

	caller := auth.Identity{
		UserID:      callerUser,
		HouseholdID: callerHousehold,
		DisplayName: "Tester",
	}

	t.Run("link token", func(t *testing.T) {
		body := `{"item_id":"` + otherItem.String() + `"}`
		req := httptest.NewRequest(http.MethodPost, "/api/plaid/link-token", strings.NewReader(body))
		req = req.WithContext(auth.ContextWithIdentity(ctx, caller))
		rec := httptest.NewRecorder()

		srv.handleCreateLinkToken(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body: %s", rec.Code, rec.Body.String())
		}
		// No token may be minted for an item outside the caller's reach.
		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if _, ok := resp["link_token"]; ok {
			t.Error("a link token was returned for another user's item")
		}
	})

	// reconnected posts to the callback as the caller, for whichever item id.
	reconnected := func(itemID uuid.UUID) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost,
			"/api/plaid/items/"+itemID.String()+"/reconnected", nil)

		// Stand in for the router, which is what normally resolves {itemID}.
		routeCtx := chi.NewRouteContext()
		routeCtx.URLParams.Add("itemID", itemID.String())
		req = req.WithContext(context.WithValue(
			auth.ContextWithIdentity(ctx, caller), chi.RouteCtxKey, routeCtx))

		rec := httptest.NewRecorder()
		srv.handleItemReconnected(rec, req)
		return rec
	}

	t.Run("reconnected callback rejects another user's item", func(t *testing.T) {
		rec := reconnected(otherItem)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body: %s", rec.Code, rec.Body.String())
		}

		// The item must be untouched — still broken, still owned by the other user.
		item, err := srv.Queries.GetPlaidItem(ctx, otherItem)
		if err != nil {
			t.Fatalf("reload item: %v", err)
		}
		if item.Status != "login_required" {
			t.Errorf("status = %q, want it left as login_required", item.Status)
		}
	})

	// The positive control: without this, a handler that rejected everything
	// would pass the tests above.
	t.Run("reconnected callback clears the caller's own item", func(t *testing.T) {
		rec := reconnected(callerItem)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
		}

		item, err := srv.Queries.GetPlaidItem(ctx, callerItem)
		if err != nil {
			t.Fatalf("reload item: %v", err)
		}
		if item.Status != "active" {
			t.Errorf("status = %q, want active", item.Status)
		}
		if item.ErrorCode != nil {
			t.Errorf("error_code = %q, want it cleared", *item.ErrorCode)
		}
	})
}
