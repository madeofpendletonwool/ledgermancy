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

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// Roles. A role is about what a LOGIN may do; it says nothing about who a
// person is. A child with no login has no role at all, which is the normal
// state for a young one — see household_people.
const (
	// RoleOwner created the household. Everything a member can do, plus role
	// management and household settings.
	RoleOwner = "owner"
	// RoleMember is a full adult member: today's behaviour for everybody.
	RoleMember = "member"
	// RoleChild is a deliberately reduced login a parent enables. It sees its
	// own allowance, its own goals, and the accounts held for it — nothing else.
	RoleChild = "child"
)

// Identity is the authenticated caller attached to a request context.
type Identity struct {
	UserID      uuid.UUID
	HouseholdID uuid.UUID
	Email       string
	DisplayName string
	SessionID   uuid.UUID
	// Role is one of RoleOwner / RoleMember / RoleChild. It comes from the same
	// query that resolves the session, so no authorization decision needs a
	// second round trip.
	Role string
	// PersonID is the household_people row this login belongs to. Nil only if
	// that row was removed out from under the login, which the LEFT JOIN in
	// GetSessionUser tolerates rather than failing authentication over.
	PersonID *uuid.UUID
	// TokenHash identifies the caller's own session row. Handlers that revoke
	// "every session except this one" need it, and it is already computed
	// during authentication — recomputing it later would mean re-reading a
	// cookie the handler should not have to know about.
	TokenHash string
	// APITokenID is the personal API token this request authenticated with, and
	// is nil for the ordinary cookie path.
	//
	// Everything ABOVE this field is identical either way, and that is the
	// invariant the feature rests on: a token resolves to the same user, the
	// same household, the same role and the same person as a session for that
	// user, so every visibility predicate in the app keeps holding without
	// knowing which credential arrived. These last two fields say how the caller
	// got here, not what they may see.
	APITokenID *uuid.UUID
	// Scopes is what the API token permits. Empty for a session, which is not a
	// restriction — a session is the full account, and the scope check only ever
	// runs against a token. See ViaAPIToken.
	Scopes []string
}

// ViaAPIToken reports whether the caller presented a personal API token rather
// than a session cookie.
func (i Identity) ViaAPIToken() bool { return i.APITokenID != nil }

// AllowsWrite reports whether this caller may make a state-changing request.
//
// A session always may; the account's own role guards are what limit it. A
// token may only if it carries ScopeWrite. Note the direction: this is about
// the CREDENTIAL, and it composes with the role guards rather than replacing
// them — a read-write token held by a child login still gets nothing a child
// could not already do.
func (i Identity) AllowsWrite() bool {
	return !i.ViaAPIToken() || ScopesAllowWrite(i.Scopes)
}

// IsChild reports whether the caller is a reduced child login.
func (i Identity) IsChild() bool { return i.Role == RoleChild }

// IsAdult reports whether the caller is an owner or a full member. Written as
// an allowlist rather than `!IsChild()` on purpose: a role added later is
// non-adult until somebody decides otherwise, so a new role cannot silently
// inherit permission over the household's money.
func (i Identity) IsAdult() bool {
	return i.Role == RoleOwner || i.Role == RoleMember
}

// IsOwner reports whether the caller owns the household.
func (i Identity) IsOwner() bool { return i.Role == RoleOwner }

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
//
// API tokens are coalesced on the same interval, and for a stronger reason: an
// integration on a one-minute timer is the expected client, so writing on every
// request would mean a token costs more database writes than the requests it is
// serving. A never-used token has a NULL last_used_at and is always written, so
// the first call still shows up straight away.
const sessionTouchInterval = 5 * time.Minute

// SessionLookup is the slice of the data layer the middleware needs. Keeping
// it narrow lets tests supply a stub instead of a live database.
type SessionLookup interface {
	GetSessionUser(ctx context.Context, arg dbgen.GetSessionUserParams) (dbgen.GetSessionUserRow, error)
	TouchSession(ctx context.Context, arg dbgen.TouchSessionParams) error
	GetAPITokenUser(ctx context.Context, tokenHash string) (dbgen.GetAPITokenUserRow, error)
	TouchAPIToken(ctx context.Context, arg dbgen.TouchAPITokenParams) error
}

// Middleware resolves the session cookie into an Identity.
type Middleware struct {
	Queries SessionLookup
	Secret  []byte
}

// Authenticate rejects any request without a valid, unexpired credential —
// either a session cookie or a personal API token bearer header.
//
// The read-only scope is enforced HERE rather than per handler, and that
// placement is the whole guarantee. A check spread across handlers is a check
// somebody forgets on the one route that mattered; mounted on the authenticator,
// a read-only token cannot reach a state-changing handler at all, including
// every handler written after this code.
func (m Middleware) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, err := m.identify(r)
		if err != nil {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if IsStateChanging(r.Method) && !identity.AllowsWrite() {
			writeJSONError(w, http.StatusForbidden, "this API token is read-only")
			return
		}
		next.ServeHTTP(w, r.WithContext(ContextWithIdentity(r.Context(), identity)))
	})
}

// RequireSession rejects a caller who authenticated with an API token.
//
// It guards the handful of routes that manage credentials themselves: token
// creation and revocation, password changes, and second-factor enrolment.
// Without it a leaked read-write token is not one credential but a credential
// FACTORY — it could mint fresh tokens for itself and revoke the ones the user
// would have used to lock it out, and the revoke button in Settings would stop
// meaning anything. Requiring the cookie means taking the account back only ever
// needs the password, which is the property the whole design rests on.
func RequireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, ok := FromContext(r.Context())
		if !ok {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if identity.ViaAPIToken() {
			writeJSONError(w, http.StatusForbidden, "sign in to manage credentials; an API token cannot")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ContextWithIdentity attaches an authenticated caller to a context, the way
// Authenticate does. Handler tests use it to exercise a handler directly
// without minting a real session.
func ContextWithIdentity(ctx context.Context, identity Identity) context.Context {
	return context.WithValue(ctx, identityKey, identity)
}

// identify looks up the caller, returning an error if the credential is
// missing, unknown, or expired. The expiry check lives in the SQL for both
// paths, so a stale credential simply does not come back.
//
// A request carrying an Authorization header is resolved ONLY as a token, never
// falling through to the cookie, and that is load-bearing rather than tidy.
// Token requests are exempt from CSRF (see RequireCSRF), so if a garbage bearer
// header could fall back to the cookie, a cross-site page could bypass CSRF by
// attaching a junk Authorization header and have the request run as the signed-in
// user. Refusing the fallback is what makes the exemption safe: a bearer request
// either authenticates as its own token or authenticates as nobody.
func (m Middleware) identify(r *http.Request) (Identity, error) {
	if token, present := BearerToken(r); present {
		return m.identifyToken(r, token)
	}
	return m.identifySession(r)
}

// identifySession resolves the session cookie.
func (m Middleware) identifySession(r *http.Request) (Identity, error) {
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
		Role:        row.Role,
		PersonID:    row.PersonID,
		TokenHash:   tokenHash,
	}, nil
}

// identifyToken resolves a personal API token into the SAME Identity a session
// for that user would produce.
//
// SessionID is left zero and TokenHash is left empty on purpose. Both name a
// session row, and there is not one — a handler that reaches for either is
// asking a question about a browser, and a zero value is a more honest answer
// than a plausible-looking one. The routes that do ask (revoke-others) are
// behind RequireSession, so they never see this identity.
func (m Middleware) identifyToken(r *http.Request, token string) (Identity, error) {
	if token == "" {
		return Identity{}, errors.New("empty bearer token")
	}

	row, err := m.Queries.GetAPITokenUser(r.Context(), HashToken(m.Secret, token))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Covers revoked as well as unknown and expired: revocation deletes
			// the row, so the very next request fails here. There is no cached
			// identity anywhere above this to outlive it.
			return Identity{}, errors.New("api token not found or expired")
		}
		return Identity{}, err
	}

	// Best effort, exactly as for sessions: losing a last-used write means the
	// Settings list reads a little stale, which is not worth failing a valid
	// request over.
	if row.TokenLastUsedAt == nil || time.Since(*row.TokenLastUsedAt) >= sessionTouchInterval {
		if err := m.Queries.TouchAPIToken(r.Context(), dbgen.TouchAPITokenParams{
			ID:     row.TokenID,
			MinAge: Interval(sessionTouchInterval),
		}); err != nil {
			slog.Warn("touch api token", "error", err, "token_id", row.TokenID)
		}
	}

	tokenID := row.TokenID
	return Identity{
		UserID:      row.UserID,
		HouseholdID: row.HouseholdID,
		Email:       row.Email,
		DisplayName: row.DisplayName,
		Role:        row.Role,
		PersonID:    row.PersonID,
		APITokenID:  &tokenID,
		Scopes:      row.TokenScopes,
	}, nil
}

// RequireAdult rejects child logins.
//
// This is the blunt instrument, and it is mounted on whole route groups rather
// than sprinkled per handler: a restriction applied to some routes and not
// others implies protection that is not there. Where a child legitimately needs
// one route out of a protected group, that route moves to its own group rather
// than the group's guard being loosened.
//
// 403 rather than 404: the caller is authenticated and the resource exists, so
// pretending otherwise would just make a confusing UI. Where the existence of
// the resource is itself the secret, the handler returns 404 on its own.
func RequireAdult(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, ok := FromContext(r.Context())
		if !ok {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if !identity.IsAdult() {
			writeJSONError(w, http.StatusForbidden, "not permitted for this account")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireOwner rejects anyone but the household owner. Used for the handful of
// actions that change who can do what.
func RequireOwner(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, ok := FromContext(r.Context())
		if !ok {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if !identity.IsOwner() {
			writeJSONError(w, http.StatusForbidden, "only the household owner can do this")
			return
		}
		next.ServeHTTP(w, r)
	})
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
//
// Bearer-authenticated requests are EXEMPT, stated here explicitly rather than
// left to a handler. CSRF defends against a request the browser attaches an
// ambient credential to without the caller's involvement; a token is not
// ambient — a program has to put it in a header on purpose, and no cross-site
// page can make the browser produce a header it does not know. Demanding a CSRF
// cookie of a curl script would only mean every third-party client fetches a
// cookie it has no other use for.
//
// The exemption is safe for exactly one reason, and it lives in
// Middleware.identify: a request carrying an Authorization header is NEVER
// resolved from the session cookie. So the worst a forged cross-site request
// with a junk bearer header achieves is a 401.
func RequireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !IsStateChanging(r.Method) {
			next.ServeHTTP(w, r)
			return
		}

		if _, present := BearerToken(r); present {
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
