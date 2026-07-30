package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/config"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db"
)

// The login response has to describe the account fully.
//
// The client caches exactly what login returns as its session — that is
// deliberate, so the app can draw itself without a second round trip — and the
// role in it decides WHICH app gets drawn: the household dashboard, or the
// reduced allowance view a child sees. An omitted role read as "not an adult",
// so every account, owners included, landed on the child view until the next
// reload of /api/auth/me quietly corrected it.
//
//	docker run -d --rm --name lm-test -e POSTGRES_PASSWORD=test \
//	  -e POSTGRES_DB=lmtest -p 55432:5432 postgres:17-alpine
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' go test ./internal/api/
func TestLoginResponseCarriesRoleAndPerson(t *testing.T) {
	const password = "correct-horse-battery"

	for _, role := range []string{auth.RoleOwner, auth.RoleMember, auth.RoleChild} {
		t.Run(role, func(t *testing.T) {
			handler, email := setupLoginFixture(t, role, password)

			body := `{"email":` + quote(email) + `,"password":` + quote(password) + `}`
			req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			// Double-submit CSRF: the header and the cookie only have to agree.
			req.Header.Set(auth.CSRFHeaderName, "test-csrf")
			req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: "test-csrf"})

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("login: got %d, want 200\n%s", rec.Code, rec.Body.String())
			}

			var got struct {
				Role     string     `json:"role"`
				PersonID *uuid.UUID `json:"person_id"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode login response: %v\n%s", err, rec.Body.String())
			}

			if got.Role != role {
				t.Errorf("role: got %q, want %q — the client draws the wrong app for this",
					got.Role, role)
			}
			if got.PersonID == nil {
				t.Error("person_id is null; the client cannot address its own person until a reload")
			}
		})
	}
}

// setupLoginFixture seeds one household with a single login of the given role
// and its person row, and returns a router wired to the real database.
func setupLoginFixture(t *testing.T, role, password string) (http.Handler, string) {
	t.Helper()

	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}

	householdID, userID := uuid.New(), uuid.New()
	email := userID.String() + "@example.test"

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed exec: %v\n%s", err, sql)
		}
	}
	exec(`INSERT INTO households (id, name) VALUES ($1, 'Login Test')`, householdID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id = $1`, householdID)
	})
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name, role)
	      VALUES ($1, $2, $3, $4, 'Tester', $5)`, userID, householdID, email, hash, role)
	exec(`INSERT INTO household_people (household_id, user_id, display_name, is_dependent)
	      VALUES ($1, $2, 'Tester', $3)`, householdID, userID, role == auth.RoleChild)

	s := NewServer(config.Config{
		SessionSecret: []byte("test-secret-for-login-response-only"),
	}, pool, nil)
	return s.Routes(), email
}

// quote renders a JSON string literal, so the fixture's generated email can be
// dropped into a request body without hand-escaping.
func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
