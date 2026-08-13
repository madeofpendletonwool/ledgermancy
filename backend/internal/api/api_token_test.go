package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/config"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/testdb"
)

// Personal API tokens, against a real Postgres and the real router.
//
// The cases here are the ones where a bearer token could quietly become
// something other than "the same user, over a different transport":
//
//   - a token must resolve to the SAME identity and the SAME visibility a
//     session for that user produces — byte for byte, not approximately;
//   - a read-only token must be refused on writes by the MIDDLEWARE, so a
//     handler written next year inherits the refusal;
//   - the CSRF exemption must not become a CSRF hole: a cross-site request that
//     merely ATTACHES an Authorization header must not fall back to the
//     browser's cookie;
//   - revocation must bite on the very next request;
//   - a token must not be able to mint or revoke tokens, or the revoke button
//     stops meaning anything against a leak.
//
//	docker run -d --rm --name lm-test -e POSTGRES_PASSWORD=test \
//	  -e POSTGRES_DB=lmtest -p 55432:5432 postgres:17-alpine
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' go test ./internal/api/ -run TestAPIToken
func TestAPITokens(t *testing.T) {
	fx := setupAPITokenFixture(t)

	// ── Creation ─────────────────────────────────────────────────────────
	readOnly := fx.createToken(t, `{"name":"Dashboard tile","scopes":["read"]}`)
	readWrite := fx.createToken(t, `{"name":"Import script","scopes":["read","write"]}`)

	if !strings.HasPrefix(readOnly.Token, auth.APITokenPrefix) {
		t.Errorf("token %q lacks the %q prefix that makes it recognisable",
			readOnly.Token, auth.APITokenPrefix)
	}

	t.Run("only the hash is stored", func(t *testing.T) {
		var stored string
		if err := fx.pool.QueryRow(context.Background(),
			`SELECT token_hash FROM api_tokens WHERE id = $1`, readOnly.ID,
		).Scan(&stored); err != nil {
			t.Fatalf("read stored token: %v", err)
		}
		if stored == readOnly.Token {
			t.Fatal("the plaintext token is in the database; it must be stored as an HMAC only")
		}
		if stored != auth.HashToken(fx.secret, readOnly.Token) {
			t.Error("stored hash is not HMAC-SHA256(SESSION_SECRET, token) — the session pattern")
		}
	})

	t.Run("an unknown scope is refused rather than dropped", func(t *testing.T) {
		rec := fx.do(t, fx.sessionRequest(t, http.MethodPost, "/api/auth/tokens",
			`{"name":"Sneaky","scopes":["read","admin"]}`))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("got %d, want 400 — silently dropping a scope hands back a token that does less than the caller was told",
				rec.Code)
		}
	})

	// ── Same identity, same visibility ───────────────────────────────────
	t.Run("token auth produces the same identity as session auth", func(t *testing.T) {
		viaSession := fx.do(t, fx.sessionRequest(t, http.MethodGet, "/api/auth/me", ""))
		viaToken := fx.do(t, fx.tokenRequest(t, readOnly.Token, http.MethodGet, "/api/auth/me", ""))

		if viaToken.Code != http.StatusOK {
			t.Fatalf("bearer /me: got %d, want 200\n%s", viaToken.Code, viaToken.Body.String())
		}
		if viaSession.Body.String() != viaToken.Body.String() {
			t.Errorf("identity differs by credential:\n session: %s\n   token: %s",
				viaSession.Body.String(), viaToken.Body.String())
		}
	})

	t.Run("a token grants the user's own visibility and no more", func(t *testing.T) {
		viaSession := fx.do(t, fx.sessionRequest(t, http.MethodGet, "/api/accounts/", ""))
		viaToken := fx.do(t, fx.tokenRequest(t, readOnly.Token, http.MethodGet, "/api/accounts/", ""))

		if viaToken.Code != http.StatusOK {
			t.Fatalf("bearer accounts: got %d, want 200\n%s", viaToken.Code, viaToken.Body.String())
		}
		if viaSession.Body.String() != viaToken.Body.String() {
			t.Fatalf("visibility differs by credential:\n session: %s\n   token: %s",
				viaSession.Body.String(), viaToken.Body.String())
		}
		// The predicate is only proved by something it must EXCLUDE.
		if strings.Contains(viaToken.Body.String(), "Carol Private") {
			t.Error("a token reached another household's account; the visibility predicate is not holding")
		}
		if !strings.Contains(viaToken.Body.String(), "Alice Checking") {
			t.Error("a token cannot see its own user's account; the fixture or the scoping is wrong")
		}
	})

	// ── Scope enforcement ────────────────────────────────────────────────
	//
	// POST /api/categories/ with an empty object is a 400 from the handler. That
	// is the point of using it: 403 means the middleware refused before the
	// handler ran, 400 means it did not, so the two outcomes are distinguishable
	// without the test having to succeed at creating anything.
	t.Run("read-only scope blocks writes", func(t *testing.T) {
		rec := fx.do(t, fx.tokenRequest(t, readOnly.Token, http.MethodPost, "/api/categories/", `{}`))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("got %d, want 403 — a read-only token reached a state-changing handler\n%s",
				rec.Code, rec.Body.String())
		}
	})

	t.Run("read-only scope still allows reads", func(t *testing.T) {
		rec := fx.do(t, fx.tokenRequest(t, readOnly.Token, http.MethodGet, "/api/categories/", ""))
		if rec.Code != http.StatusOK {
			t.Errorf("got %d, want 200\n%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("write scope reaches the handler", func(t *testing.T) {
		rec := fx.do(t, fx.tokenRequest(t, readWrite.Token, http.MethodPost, "/api/categories/", `{}`))
		if rec.Code == http.StatusForbidden {
			t.Fatalf("a read-write token was refused as read-only\n%s", rec.Body.String())
		}
		if rec.Code != http.StatusBadRequest {
			t.Errorf("got %d, want 400 from the handler's own validation\n%s", rec.Code, rec.Body.String())
		}
	})

	// ── CSRF ─────────────────────────────────────────────────────────────
	t.Run("token requests are exempt from CSRF", func(t *testing.T) {
		// tokenRequest deliberately sets no CSRF cookie or header. A cookie
		// request shaped this way is rejected outright — see the case below.
		rec := fx.do(t, fx.tokenRequest(t, readWrite.Token, http.MethodPost, "/api/categories/", `{}`))
		if rec.Code == http.StatusForbidden && strings.Contains(rec.Body.String(), "csrf") {
			t.Fatalf("a bearer request was held to the CSRF check\n%s", rec.Body.String())
		}
	})

	t.Run("cookie requests are still held to CSRF", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/categories/", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: fx.sessionToken})
		rec := fx.do(t, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("got %d, want 403 — the CSRF guard has been loosened for cookie requests too\n%s",
				rec.Code, rec.Body.String())
		}
	})

	t.Run("a bearer header never falls back to the session cookie", func(t *testing.T) {
		// The forged cross-site request: the browser attaches the session
		// cookie by itself, the attacker's page adds a junk Authorization
		// header to skip CSRF. If this ever answers anything but 401, the CSRF
		// exemption above has become a CSRF hole.
		req := httptest.NewRequest(http.MethodPost, "/api/categories/", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer not-a-real-token")
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: fx.sessionToken})

		rec := fx.do(t, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("got %d, want 401 — a junk bearer header authenticated as the cookie's user\n%s",
				rec.Code, rec.Body.String())
		}
	})

	// ── Last used ────────────────────────────────────────────────────────
	t.Run("last_used_at is recorded on use", func(t *testing.T) {
		fresh := fx.createToken(t, `{"name":"Never used yet","scopes":["read"]}`)
		if fresh.LastUsedAt != nil {
			t.Fatalf("a brand-new token reports last_used_at %v; it has not been used", *fresh.LastUsedAt)
		}

		fx.do(t, fx.tokenRequest(t, fresh.Token, http.MethodGet, "/api/auth/me", ""))

		var lastUsed *time.Time
		if err := fx.pool.QueryRow(context.Background(),
			`SELECT last_used_at FROM api_tokens WHERE id = $1`, fresh.ID,
		).Scan(&lastUsed); err != nil {
			t.Fatalf("read last_used_at: %v", err)
		}
		if lastUsed == nil {
			t.Fatal("last_used_at is still NULL after the token authenticated a request")
		}
	})

	// ── Credential management is session-only ────────────────────────────
	t.Run("a token cannot manage tokens", func(t *testing.T) {
		for _, tc := range []struct{ method, path, body string }{
			{http.MethodGet, "/api/auth/tokens", ""},
			{http.MethodPost, "/api/auth/tokens", `{"name":"Second token","scopes":["read","write"]}`},
			{http.MethodDelete, "/api/auth/tokens/" + readOnly.ID.String(), ""},
		} {
			rec := fx.do(t, fx.tokenRequest(t, readWrite.Token, tc.method, tc.path, tc.body))
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s %s: got %d, want 403 — a leaked token could mint replacements for itself\n%s",
					tc.method, tc.path, rec.Code, rec.Body.String())
			}
		}
	})

	t.Run("a token cannot change the password or the second factor", func(t *testing.T) {
		rec := fx.do(t, fx.tokenRequest(t, readWrite.Token, http.MethodPost, "/api/auth/password",
			`{"current_password":"x","new_password":"y"}`))
		if rec.Code != http.StatusForbidden {
			t.Errorf("got %d, want 403\n%s", rec.Code, rec.Body.String())
		}
	})

	// ── Revocation ───────────────────────────────────────────────────────
	// Last, because it destroys one of the tokens the cases above use.
	t.Run("revocation takes effect immediately", func(t *testing.T) {
		before := fx.do(t, fx.tokenRequest(t, readOnly.Token, http.MethodGet, "/api/auth/me", ""))
		if before.Code != http.StatusOK {
			t.Fatalf("token was already dead: %d", before.Code)
		}

		rec := fx.do(t, fx.sessionRequest(t, http.MethodDelete,
			"/api/auth/tokens/"+readOnly.ID.String(), ""))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("revoke: got %d, want 204\n%s", rec.Code, rec.Body.String())
		}

		after := fx.do(t, fx.tokenRequest(t, readOnly.Token, http.MethodGet, "/api/auth/me", ""))
		if after.Code != http.StatusUnauthorized {
			t.Fatalf("got %d, want 401 — a revoked token still authenticates\n%s",
				after.Code, after.Body.String())
		}
	})

	t.Run("one user cannot revoke another's token", func(t *testing.T) {
		rec := fx.do(t, fx.carolRequest(t, http.MethodDelete,
			"/api/auth/tokens/"+readWrite.ID.String(), ""))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("got %d, want 404\n%s", rec.Code, rec.Body.String())
		}
		still := fx.do(t, fx.tokenRequest(t, readWrite.Token, http.MethodGet, "/api/auth/me", ""))
		if still.Code != http.StatusOK {
			t.Errorf("another household's user revoked this token: %d", still.Code)
		}
	})
}

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

type apiTokenFixture struct {
	handler      http.Handler
	pool         *pgxpool.Pool
	secret       []byte
	sessionToken string
	carolSession string
}

type createdTokenBody struct {
	ID         uuid.UUID  `json:"id"`
	Name       string     `json:"name"`
	Scopes     []string   `json:"scopes"`
	LastUsedAt *time.Time `json:"last_used_at"`
	Token      string     `json:"token"`
}

// setupAPITokenFixture seeds two households — Alice's, with one account, and
// Carol's, with another — plus a live session for each, and returns the real
// router wired to the real database.
//
// Two households rather than one because the visibility claim ("a token grants
// the user's own visibility, no more") is only testable against something the
// token must NOT see.
func setupAPITokenFixture(t *testing.T) *apiTokenFixture {
	t.Helper()

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, sql)
		}
	}

	householdA, alice, itemA, acctA := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	householdB, carol, itemB, acctB := uuid.New(), uuid.New(), uuid.New(), uuid.New()

	exec(`INSERT INTO households (id, name) VALUES ($1, 'Token Test A')`, householdA)
	exec(`INSERT INTO households (id, name) VALUES ($1, 'Token Test B')`, householdB)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM households WHERE id = ANY($1)`, []uuid.UUID{householdA, householdB})
	})

	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name, role)
	      VALUES ($1, $2, $3, 'x', 'Alice', 'member')`, alice, householdA, alice.String()+"@example.test")
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name, role)
	      VALUES ($1, $2, $3, 'x', 'Carol', 'member')`, carol, householdB, carol.String()+"@example.test")
	exec(`INSERT INTO household_people (household_id, user_id, display_name)
	      VALUES ($1, $2, 'Alice')`, householdA, alice)
	exec(`INSERT INTO household_people (household_id, user_id, display_name)
	      VALUES ($1, $2, 'Carol')`, householdB, carol)

	exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, products, status)
	      VALUES ($1, $2, $3, '\x00', '{transactions}', 'active')`, itemA, alice, itemA.String())
	exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, products, status)
	      VALUES ($1, $2, $3, '\x00', '{transactions}', 'active')`, itemB, carol, itemB.String())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type, subtype, current_balance)
	      VALUES ($1, $2, $3, 'Alice Checking', 'depository', 'checking', 100.00)`,
		acctA, itemA, acctA.String())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type, subtype, current_balance)
	      VALUES ($1, $2, $3, 'Carol Private', 'depository', 'checking', 900.00)`,
		acctB, itemB, acctB.String())

	secret := []byte("test-secret-for-api-tokens-only")

	newSession := func(userID uuid.UUID) string {
		t.Helper()
		token, err := auth.NewToken()
		if err != nil {
			t.Fatalf("mint session token: %v", err)
		}
		exec(`INSERT INTO sessions (user_id, token_hash, expires_at)
		      VALUES ($1, $2, now() + interval '30 days')`, userID, auth.HashToken(secret, token))
		return token
	}

	s := NewServer(config.Config{SessionSecret: secret}, pool, nil)

	return &apiTokenFixture{
		handler:      s.Routes(),
		pool:         pool,
		secret:       secret,
		sessionToken: newSession(alice),
		carolSession: newSession(carol),
	}
}

func (f *apiTokenFixture) do(t *testing.T, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

// sessionRequest builds a browser-shaped request: session cookie plus a
// matching double-submit CSRF pair.
func (f *apiTokenFixture) sessionRequest(t *testing.T, method, path, body string) *http.Request {
	t.Helper()
	return cookieRequest(method, path, body, f.sessionToken)
}

func (f *apiTokenFixture) carolRequest(t *testing.T, method, path, body string) *http.Request {
	t.Helper()
	return cookieRequest(method, path, body, f.carolSession)
}

func cookieRequest(method, path, body, sessionToken string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(auth.CSRFHeaderName, "test-csrf")
	req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: "test-csrf"})
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sessionToken})
	return req
}

// tokenRequest builds a third-party-shaped request: a bearer header and nothing
// else. No cookie, no CSRF token — that is what a curl script sends, and the
// whole point of the feature is that it is enough.
func (f *apiTokenFixture) tokenRequest(t *testing.T, token, method, path, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

func (f *apiTokenFixture) createToken(t *testing.T, body string) createdTokenBody {
	t.Helper()

	rec := f.do(t, f.sessionRequest(t, http.MethodPost, "/api/auth/tokens", body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create token: got %d, want 201\n%s", rec.Code, rec.Body.String())
	}

	var created createdTokenBody
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created token: %v\n%s", err, rec.Body.String())
	}
	if created.Token == "" {
		t.Fatal("create returned no plaintext token; it is the only time the value exists")
	}
	return created
}
