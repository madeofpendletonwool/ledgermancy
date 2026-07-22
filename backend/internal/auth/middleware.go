package auth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
)

// Identity is the authenticated caller attached to a request context.
type Identity struct {
	UserID      uuid.UUID
	HouseholdID uuid.UUID
	Email       string
	DisplayName string
	SessionID   uuid.UUID
	// TokenHash identifies the caller's own session row. Handlers that revoke
	// "every session except this one" need it, and it is already computed
	// during authentication — recomputing it later would mean re-reading a
	// cookie the handler should not have to know about.
	TokenHash string
}

type contextKey struct{}

var identityKey contextKey

// SessionIdleTTL is how long a session survives without being used.
//
// It sits underneath config.SessionTTL, which is the absolute cap: a session
// dies after 30 days no matter what, or after 7 days of silence. The idle cap
// is what limits a session forgotten on a borrowed or stolen machine.
const SessionIdleTTL = 7 * 24 * time.Hour

// sessionTouchInterval is how stale last_used_at must be before the middleware
// writes to it. Recording activity on literally every request would turn every
// read into a database write for no extra safety.
const sessionTouchInterval = 5 * time.Minute

// SessionLookup is the slice of the data layer the middleware needs. Keeping
// it narrow lets tests supply a stub instead of a live database.
type SessionLookup interface {
	GetSessionUser(ctx context.Context, arg dbgen.GetSessionUserParams) (dbgen.GetSessionUserRow, error)
	TouchSession(ctx context.Context, arg dbgen.TouchSessionParams) error
}

// Middleware resolves the session cookie into an Identity.
type Middleware struct {
	Queries SessionLookup
	Secret  []byte
}

// Authenticate rejects any request without a valid, unexpired session.
func (m Middleware) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, err := m.identify(r)
		if err != nil {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey, identity)))
	})
}

// identify looks up the caller, returning an error if the cookie is missing,
// unknown, or expired. The expiry check lives in the SQL, so a stale session
// simply does not come back.
func (m Middleware) identify(r *http.Request) (Identity, error) {
	cookie, err := r.Cookie(SessionCookieName)
	if err != nil || cookie.Value == "" {
		return Identity{}, errors.New("no session cookie")
	}

	tokenHash := HashToken(m.Secret, cookie.Value)

	row, err := m.Queries.GetSessionUser(r.Context(), dbgen.GetSessionUserParams{
		TokenHash: tokenHash,
		IdleTtl:   Interval(SessionIdleTTL),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Identity{}, errors.New("session not found or expired")
		}
		return Identity{}, err
	}

	// Keep the idle clock alive. This is best effort: a failure here means the
	// session ages a little faster than it should, which is the safe direction,
	// and is not worth failing an otherwise valid request over.
	if time.Since(row.SessionLastUsedAt) >= sessionTouchInterval {
		if err := m.Queries.TouchSession(r.Context(), dbgen.TouchSessionParams{
			ID:     row.SessionID,
			MinAge: Interval(sessionTouchInterval),
		}); err != nil {
			slog.Warn("touch session", "error", err, "session_id", row.SessionID)
		}
	}

	return Identity{
		UserID:      row.UserID,
		HouseholdID: row.HouseholdID,
		Email:       row.Email,
		DisplayName: row.DisplayName,
		SessionID:   row.SessionID,
		TokenHash:   tokenHash,
	}, nil
}

// Interval converts a Go duration into the pgtype.Interval that sqlc emits for
// an `::interval` parameter. Postgres intervals carry months and days as
// separate fields, but microseconds alone expresses any fixed duration exactly,
// which is all the callers here need.
func Interval(d time.Duration) pgtype.Interval {
	return pgtype.Interval{Microseconds: d.Microseconds(), Valid: true}
}

// RequireCSRF enforces the double-submit cookie pattern on unsafe methods: the
// X-CSRF-Token header must match the CSRF cookie. SameSite=Strict already
// blocks most cross-site requests; this is the second layer.
func RequireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}

		cookie, err := r.Cookie(CSRFCookieName)
		header := r.Header.Get(CSRFHeaderName)
		if err != nil || cookie.Value == "" || header == "" ||
			subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(header)) != 1 {
			writeJSONError(w, http.StatusForbidden, "invalid csrf token")
			return
		}

		next.ServeHTTP(w, r)
	})
}

// writeJSONError keeps middleware rejections in the same JSON envelope the
// handlers use, so clients never have to parse two error formats.
func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// FromContext returns the authenticated caller. The bool is false on
// unauthenticated requests.
func FromContext(ctx context.Context) (Identity, bool) {
	identity, ok := ctx.Value(identityKey).(Identity)
	return identity, ok
}

// MustFromContext returns the authenticated caller and panics if there is
// none. Only call this from handlers mounted behind Authenticate, where the
// absence of an identity is a programming error rather than a runtime case.
func MustFromContext(ctx context.Context) Identity {
	identity, ok := FromContext(ctx)
	if !ok {
		panic("auth: no identity in context; handler is not behind Authenticate")
	}
	return identity
}
