package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/apex42group/ledgermancy/backend/internal/auth"
	"github.com/apex42group/ledgermancy/backend/internal/config"
	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
	"github.com/apex42group/ledgermancy/backend/internal/ratelimit"
)

// minPasswordLength follows NIST guidance: length is the requirement that
// matters, so there are no composition rules to work around.
const minPasswordLength = 12

// inviteTTL is how long a household invitation stays valid.
const inviteTTL = 7 * 24 * time.Hour

type registerRequest struct {
	Email         string `json:"email"`
	Password      string `json:"password"`
	DisplayName   string `json:"display_name"`
	HouseholdName string `json:"household_name"`
	InviteToken   string `json:"invite_token"`
}

type userResponse struct {
	ID          uuid.UUID `json:"id"`
	HouseholdID uuid.UUID `json:"household_id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name"`
}

// handleRegister creates an account.
//
// The first ever user bootstraps a new household. After that, registration is
// invite-only — this is a private household ledger, not a public service, so
// an open sign-up form would just be an attack surface.
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	req.DisplayName = strings.TrimSpace(req.DisplayName)

	if _, err := mail.ParseAddress(req.Email); err != nil {
		writeError(w, http.StatusBadRequest, "a valid email address is required")
		return
	}
	if req.DisplayName == "" {
		writeError(w, http.StatusBadRequest, "display_name is required")
		return
	}
	if msg, ok := validatePassword(req.Password, req.Email, req.DisplayName); !ok {
		writeError(w, http.StatusBadRequest, msg)
		return
	}

	ctx := r.Context()

	userCount, err := s.Queries.CountUsers(ctx)
	if err != nil {
		s.internalError(w, "count users", err)
		return
	}

	var householdID uuid.UUID
	var inviteID *uuid.UUID

	if userCount == 0 {
		// Bootstrap: create the household this first user will own.
		name := strings.TrimSpace(req.HouseholdName)
		if name == "" {
			name = req.DisplayName + "'s Household"
		}
		household, err := s.Queries.CreateHousehold(ctx, name)
		if err != nil {
			s.internalError(w, "create household", err)
			return
		}
		householdID = household.ID
	} else {
		if req.InviteToken == "" {
			writeError(w, http.StatusForbidden,
				"registration is invite-only; ask a household member for an invite")
			return
		}
		invite, err := s.Queries.GetInviteByTokenHash(ctx,
			auth.HashToken(s.Config.SessionSecret, req.InviteToken))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusForbidden, "invite is invalid or has expired")
				return
			}
			s.internalError(w, "look up invite", err)
			return
		}
		// Bind the invite to the address it was issued for. Without this the
		// token alone is the whole credential, so anyone who intercepted the
		// invite link could claim household membership under any address they
		// like. The email is not a secret, but requiring both means an
		// intercepted link is not by itself enough.
		if !strings.EqualFold(invite.Email, req.Email) {
			writeError(w, http.StatusForbidden,
				"this invite was issued for a different email address")
			return
		}

		householdID = invite.HouseholdID
		inviteID = &invite.ID
	}

	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		s.internalError(w, "hash password", err)
		return
	}

	user, err := s.Queries.CreateUser(ctx, dbgen.CreateUserParams{
		HouseholdID:  householdID,
		Lower:        req.Email,
		PasswordHash: hash,
		DisplayName:  req.DisplayName,
	})
	if err != nil {
		// The unique index on lower(email) is the authority on duplicates,
		// so a pre-check would only add a race, not remove one.
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "an account with that email already exists")
			return
		}
		s.internalError(w, "create user", err)
		return
	}

	if inviteID != nil {
		if err := s.Queries.MarkInviteAccepted(ctx, *inviteID); err != nil {
			// The account exists and is usable; a stale invite row is a much
			// smaller problem than failing the request at this point.
			slog.Error("mark invite accepted", "error", err, "invite_id", *inviteID)
		}
	}

	if err := s.startSession(w, r, user.ID); err != nil {
		s.internalError(w, "start session", err)
		return
	}

	s.audit(ctx, r, user.ID, "", eventRegistered,
		map[string]any{"bootstrap": userCount == 0})
	if inviteID != nil {
		s.audit(ctx, r, user.ID, "", eventInviteAccepted, nil)
	}

	writeJSON(w, http.StatusCreated, userResponse{
		ID:          user.ID,
		HouseholdID: user.HouseholdID,
		Email:       user.Email,
		DisplayName: user.DisplayName,
	})
}

type csrfResponse struct {
	Token string `json:"csrf_token"`
}

// handleCSRFToken issues a CSRF token to a client that does not have one yet.
//
// Without this, the double-submit check is unsatisfiable on the very first
// request: login and register are state-changing, but a brand-new client has
// no CSRF cookie to echo back. The frontend calls this once on load, then
// includes the token on every unsafe request. It is a safe method, so it is
// not itself subject to the CSRF check.
func (s *Server) handleCSRFToken(w http.ResponseWriter, r *http.Request) {
	// Reuse an existing token so that calling this twice does not invalidate
	// a request the client is already preparing.
	if cookie, err := r.Cookie(auth.CSRFCookieName); err == nil && cookie.Value != "" {
		writeJSON(w, http.StatusOK, csrfResponse{Token: cookie.Value})
		return
	}

	token, err := auth.NewToken()
	if err != nil {
		s.internalError(w, "generate csrf token", err)
		return
	}
	auth.SetCSRFCookie(w, s.cookieOptions(), token, time.Now().Add(config.SessionTTL))
	writeJSON(w, http.StatusOK, csrfResponse{Token: token})
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// mfaRequiredResponse tells the client the password was right but the login is
// not finished. It deliberately carries no user detail: nothing about the
// account should be readable until both factors are satisfied.
type mfaRequiredResponse struct {
	MFARequired bool `json:"mfa_required"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx := r.Context()
	email := strings.TrimSpace(req.Email)

	user, err := s.Queries.GetUserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Spend the same work as a real verification so response timing
			// does not reveal whether the address is registered.
			_ = auth.VerifyPassword(req.Password, decoyHash)
			s.audit(ctx, r, uuid.Nil, email, eventLoginFailed,
				map[string]any{"reason": "unknown_email"})
			writeError(w, http.StatusUnauthorized, "invalid email or password")
			return
		}
		s.internalError(w, "look up user", err)
		return
	}

	// A locked account still gets the generic message. Saying "locked" would
	// confirm the address exists and would hand anyone who knows an email a
	// way to keep that account locked indefinitely.
	if user.LockedUntil != nil && user.LockedUntil.After(time.Now()) {
		_ = auth.VerifyPassword(req.Password, decoyHash)
		s.audit(ctx, r, user.ID, email, eventLoginLocked, nil)
		writeError(w, http.StatusUnauthorized, "invalid email or password")
		return
	}

	if err := auth.VerifyPassword(req.Password, user.PasswordHash); err != nil {
		if !errors.Is(err, auth.ErrMismatchedPassword) {
			// A malformed stored hash is corruption, not a failed login.
			slog.Error("verify password", "error", err, "user_id", user.ID)
		}
		s.recordFailedLogin(ctx, r, user.ID, email)
		writeError(w, http.StatusUnauthorized, "invalid email or password")
		return
	}

	// The password is correct from here on.

	if err := s.Queries.ClearFailedLogins(ctx, user.ID); err != nil {
		s.internalError(w, "clear failed logins", err)
		return
	}

	// Honour the promise made in auth/password.go: a hash made under weaker
	// parameters is upgraded now, while the plaintext is in hand. Failure is
	// logged and ignored — the user authenticated correctly and must not be
	// turned away because a maintenance write did not land.
	if auth.NeedsRehash(user.PasswordHash) {
		if hash, err := auth.HashPassword(req.Password); err != nil {
			slog.Error("rehash password", "error", err, "user_id", user.ID)
		} else if err := s.Queries.UpdateUserPassword(ctx, dbgen.UpdateUserPasswordParams{
			ID: user.ID, PasswordHash: hash,
		}); err != nil {
			slog.Error("store rehashed password", "error", err, "user_id", user.ID)
		}
	}

	if user.TotpEnabled {
		if err := s.startMFAChallenge(w, r, user.ID); err != nil {
			s.internalError(w, "start mfa challenge", err)
			return
		}
		s.audit(ctx, r, user.ID, "", eventMFAChallenged, nil)
		writeJSON(w, http.StatusOK, mfaRequiredResponse{MFARequired: true})
		return
	}

	if err := s.startSession(w, r, user.ID); err != nil {
		s.internalError(w, "start session", err)
		return
	}

	s.audit(ctx, r, user.ID, "", eventLoginSucceeded, map[string]any{"mfa": false})
	s.loginLimiter.Reset(ratelimit.ClientIP(r))

	writeJSON(w, http.StatusOK, userResponse{
		ID:          user.ID,
		HouseholdID: user.HouseholdID,
		Email:       user.Email,
		DisplayName: user.DisplayName,
	})
}

// loginFailureGrace is how many wrong passwords are tolerated before the
// durable per-account backoff starts. Fat fingers are common; five is well
// clear of honest mistakes and far below what guessing needs.
const loginFailureGrace = 5

// maxLoginLockout caps the backoff. An unbounded doubling would eventually
// lock a real user out for weeks, which is a denial of service against them
// rather than protection.
const maxLoginLockout = 15 * time.Minute

// recordFailedLogin bumps the durable counter and applies exponential backoff.
//
// This complements the in-process rate limiter rather than duplicating it: the
// limiter caps attempts from one address and forgets everything on restart,
// while this follows the account wherever the attempts come from and survives.
func (s *Server) recordFailedLogin(ctx context.Context, r *http.Request, userID uuid.UUID, email string) {
	count, err := s.Queries.RecordFailedLogin(ctx, userID)
	if err != nil {
		slog.Error("record failed login", "error", err, "user_id", userID)
		return
	}

	if int(count) > loginFailureGrace {
		backoff := time.Duration(1<<min(int(count)-loginFailureGrace, 20)) * time.Second
		lockUntil := time.Now().Add(min(backoff, maxLoginLockout))

		if err := s.Queries.LockUser(ctx, dbgen.LockUserParams{
			ID: userID, LockedUntil: &lockUntil,
		}); err != nil {
			slog.Error("lock user", "error", err, "user_id", userID)
		}
	}

	s.audit(ctx, r, userID, email, eventLoginFailed,
		map[string]any{"reason": "bad_password", "consecutive_failures": count})
}

// startMFAChallenge records a half-authenticated login and sets its cookie.
func (s *Server) startMFAChallenge(w http.ResponseWriter, r *http.Request, userID uuid.UUID) error {
	token, err := auth.NewToken()
	if err != nil {
		return err
	}

	expiresAt := time.Now().Add(auth.MFAChallengeTTL)

	var userAgent, clientIP *string
	if ua := r.UserAgent(); ua != "" {
		userAgent = &ua
	}
	if ip := ratelimit.ClientIP(r); ip != "" {
		clientIP = &ip
	}

	if _, err := s.Queries.CreateMFAChallenge(r.Context(), dbgen.CreateMFAChallengeParams{
		UserID:    userID,
		TokenHash: auth.HashToken(s.Config.SessionSecret, token),
		UserAgent: userAgent,
		ClientIp:  clientIP,
		ExpiresAt: expiresAt,
	}); err != nil {
		return err
	}

	auth.SetMFACookie(w, s.cookieOptions(), token, expiresAt)
	return nil
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	// Logout is deliberately not behind Authenticate: signing out must work
	// even from an already-dead session, or a user staring at a stale tab has
	// no way to clear it. That means there is no Identity in the context, so
	// the session is resolved here to attribute the audit event.
	if cookie, err := r.Cookie(auth.SessionCookieName); err == nil && cookie.Value != "" {
		tokenHash := auth.HashToken(s.Config.SessionSecret, cookie.Value)

		if row, err := s.Queries.GetSessionUser(r.Context(), dbgen.GetSessionUserParams{
			TokenHash: tokenHash, IdleTtl: auth.Interval(auth.SessionIdleTTL),
		}); err == nil {
			s.audit(r.Context(), r, row.UserID, "", eventLogout, nil)
		}

		if err := s.Queries.DeleteSession(r.Context(), tokenHash); err != nil {
			s.internalError(w, "delete session", err)
			return
		}
	}

	// Also drop any half-finished MFA challenge, so signing out really does
	// leave nothing behind that could be completed later.
	if cookie, err := r.Cookie(auth.MFACookieName); err == nil && cookie.Value != "" {
		if challenge, err := s.Queries.GetMFAChallenge(r.Context(),
			auth.HashToken(s.Config.SessionSecret, cookie.Value)); err == nil {
			if err := s.Queries.DeleteMFAChallenge(r.Context(), challenge.ID); err != nil {
				slog.Error("clear mfa challenge on logout", "error", err)
			}
		}
	}

	auth.ClearAuthCookies(w, s.cookieOptions())
	w.WriteHeader(http.StatusNoContent)
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
	// Required only when MFA is enabled. A password change is exactly the kind
	// of account takeover step the second factor exists to stop.
	Code string `json:"code"`
}

// handleChangePassword rotates the caller's password.
//
// Every other session for the account is destroyed. That is the whole point:
// if the reason for the change is that the old password may be known to
// someone else, leaving their session alive would achieve nothing. The
// caller's own browser is re-issued a fresh session so they are not bounced to
// the login screen for doing the right thing.
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	var req changePasswordRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if msg, ok := validatePassword(req.NewPassword, identity.Email, identity.DisplayName); !ok {
		writeError(w, http.StatusBadRequest, msg)
		return
	}

	ctx := r.Context()
	user, err := s.Queries.GetUserByID(ctx, identity.UserID)
	if err != nil {
		s.internalError(w, "look up user", err)
		return
	}

	if err := auth.VerifyPassword(req.CurrentPassword, user.PasswordHash); err != nil {
		s.audit(ctx, r, identity.UserID, "", eventLoginFailed,
			map[string]any{"reason": "bad_password", "stage": "change_password"})
		writeError(w, http.StatusUnauthorized, "your current password is not correct")
		return
	}

	if user.TotpEnabled {
		row, err := s.Queries.GetUserMFA(ctx, identity.UserID)
		if err != nil {
			s.internalError(w, "get mfa state", err)
			return
		}
		if ok, err := s.consumeTOTP(ctx, row, req.Code); err != nil {
			s.internalError(w, "verify totp", err)
			return
		} else if !ok {
			s.audit(ctx, r, identity.UserID, "", eventMFAFailed,
				map[string]any{"stage": "change_password"})
			writeError(w, http.StatusUnauthorized, "that code is not correct")
			return
		}
	}

	hash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		s.internalError(w, "hash password", err)
		return
	}
	if err := s.Queries.UpdateUserPassword(ctx, dbgen.UpdateUserPasswordParams{
		ID: identity.UserID, PasswordHash: hash,
	}); err != nil {
		s.internalError(w, "update password", err)
		return
	}

	// Drop every session including this one, then immediately issue a new one
	// for this browser. Revoking first and re-issuing second means there is no
	// moment where an old session survives the change.
	if err := s.Queries.DeleteUserSessions(ctx, identity.UserID); err != nil {
		s.internalError(w, "revoke sessions", err)
		return
	}
	if err := s.startSession(w, r, identity.UserID); err != nil {
		s.internalError(w, "start session", err)
		return
	}

	s.audit(ctx, r, identity.UserID, "", eventPasswordChanged, nil)
	w.WriteHeader(http.StatusNoContent)
}

type sessionResponse struct {
	ID         uuid.UUID `json:"id"`
	UserAgent  *string   `json:"user_agent"`
	ClientIP   *string   `json:"client_ip"`
	LastUsedAt time.Time `json:"last_used_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	CreatedAt  time.Time `json:"created_at"`
	// Marks the browser making this request, so the UI can label it and warn
	// before revoking it.
	IsCurrent bool `json:"is_current"`
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	rows, err := s.Queries.ListUserSessions(r.Context(), identity.UserID)
	if err != nil {
		s.internalError(w, "list sessions", err)
		return
	}

	sessions := make([]sessionResponse, 0, len(rows))
	for _, row := range rows {
		sessions = append(sessions, sessionResponse{
			ID:         row.ID,
			UserAgent:  row.UserAgent,
			ClientIP:   row.ClientIp,
			LastUsedAt: row.LastUsedAt,
			ExpiresAt:  row.ExpiresAt,
			CreatedAt:  row.CreatedAt,
			IsCurrent:  row.ID == identity.SessionID,
		})
	}
	writeJSON(w, http.StatusOK, sessions)
}

func (s *Server) handleRevokeSession(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	sessionID, err := uuid.Parse(chi.URLParam(r, "sessionID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid session id")
		return
	}

	// user_id is in the DELETE predicate, so a caller cannot revoke another
	// user's session even with a valid id — the same shape as DeleteInvite.
	affected, err := s.Queries.DeleteSessionByID(r.Context(), dbgen.DeleteSessionByIDParams{
		ID: sessionID, UserID: identity.UserID,
	})
	if err != nil {
		s.internalError(w, "revoke session", err)
		return
	}
	if affected == 0 {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}

	if sessionID == identity.SessionID {
		auth.ClearAuthCookies(w, s.cookieOptions())
	}

	s.audit(r.Context(), r, identity.UserID, "", eventSessionRevoked,
		map[string]any{"session_id": sessionID.String(), "scope": "one"})
	w.WriteHeader(http.StatusNoContent)
}

// handleRevokeOtherSessions signs the user out everywhere but here.
func (s *Server) handleRevokeOtherSessions(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	affected, err := s.Queries.DeleteOtherUserSessions(r.Context(), dbgen.DeleteOtherUserSessionsParams{
		UserID: identity.UserID, TokenHash: identity.TokenHash,
	})
	if err != nil {
		s.internalError(w, "revoke sessions", err)
		return
	}

	s.audit(r.Context(), r, identity.UserID, "", eventSessionRevoked,
		map[string]any{"scope": "others", "count": affected})
	w.WriteHeader(http.StatusNoContent)
}

type authEventResponse struct {
	EventType string          `json:"event_type"`
	ClientIP  *string         `json:"client_ip"`
	UserAgent *string         `json:"user_agent"`
	Metadata  json.RawMessage `json:"metadata"`
	CreatedAt time.Time       `json:"created_at"`
}

// authEventLimit is how much history the security page shows. Enough to spot
// something unfamiliar, short enough to render without pagination.
const authEventLimit = 50

func (s *Server) handleListAuthEvents(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	rows, err := s.Queries.ListUserAuthEvents(r.Context(), dbgen.ListUserAuthEventsParams{
		UserID: &identity.UserID, Limit: authEventLimit,
	})
	if err != nil {
		s.internalError(w, "list auth events", err)
		return
	}

	events := make([]authEventResponse, 0, len(rows))
	for _, row := range rows {
		events = append(events, authEventResponse{
			EventType: row.EventType,
			ClientIP:  row.ClientIp,
			UserAgent: row.UserAgent,
			Metadata:  json.RawMessage(row.Metadata),
			CreatedAt: row.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, events)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	writeJSON(w, http.StatusOK, userResponse{
		ID:          identity.UserID,
		HouseholdID: identity.HouseholdID,
		Email:       identity.Email,
		DisplayName: identity.DisplayName,
	})
}

// startSession issues a session plus its CSRF token and sets both cookies.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, userID uuid.UUID) error {
	token, err := auth.NewToken()
	if err != nil {
		return err
	}
	csrfToken, err := auth.NewToken()
	if err != nil {
		return err
	}

	expiresAt := time.Now().Add(config.SessionTTL)

	var userAgent, clientIP *string
	if ua := r.UserAgent(); ua != "" {
		userAgent = &ua
	}
	if ip := ratelimit.ClientIP(r); ip != "" {
		clientIP = &ip
	}

	if _, err := s.Queries.CreateSession(r.Context(), dbgen.CreateSessionParams{
		UserID:    userID,
		TokenHash: auth.HashToken(s.Config.SessionSecret, token),
		UserAgent: userAgent,
		ClientIp:  clientIP,
		ExpiresAt: expiresAt,
	}); err != nil {
		return err
	}

	opts := s.cookieOptions()
	auth.SetSessionCookie(w, opts, token, expiresAt)
	auth.SetCSRFCookie(w, opts, csrfToken, expiresAt)
	return nil
}

// validatePassword applies the password policy, returning the message to show
// when it fails.
//
// Following NIST guidance, length is the only hard requirement — composition
// rules ("one symbol, one digit") push people towards predictable patterns
// without adding real entropy. The extra checks here reject the two things a
// length rule alone still lets through: a password that is simply the user's
// own email or name, and a single repeated character.
func validatePassword(password, email, displayName string) (string, bool) {
	if len([]rune(password)) < minPasswordLength {
		return "password must be at least 12 characters", false
	}

	// Trimming can empty the string entirely — twelve spaces passes the length
	// check above. Reject that here rather than letting the repeated-character
	// check below index into nothing.
	lowered := strings.ToLower(strings.TrimSpace(password))
	if lowered == "" {
		return "password must not be only whitespace", false
	}

	if local, _, found := strings.Cut(strings.ToLower(email), "@"); found &&
		containsIdentifier(lowered, local) {
		return "password must not contain your email address", false
	}
	if containsIdentifier(lowered, strings.ToLower(strings.TrimSpace(displayName))) {
		return "password must not contain your name", false
	}

	if strings.Count(lowered, string([]rune(lowered)[0])) == len([]rune(lowered)) {
		return "password must not be a single repeated character", false
	}

	return "", true
}

// minIdentifierLength is the shortest name or email local part worth checking
// a password against.
//
// Below this, a match is coincidence rather than password reuse: a user called
// "Al" would have every password containing the letters "al" — including
// "personal", "final", "normally" — refused for no security benefit. The check
// exists to stop someone literally using their own name as their password, and
// a fragment this short cannot distinguish that from an ordinary word.
const minIdentifierLength = 4

func containsIdentifier(loweredPassword, identifier string) bool {
	return len(identifier) >= minIdentifierLength &&
		strings.Contains(loweredPassword, identifier)
}

// decoyHash is a real argon2id hash of a value nobody can log in with. It
// gives the unknown-email path the same cost as a genuine password check.
var decoyHash = func() string {
	h, err := auth.HashPassword("ledgermancy-timing-equalizer")
	if err != nil {
		panic("auth: cannot build decoy hash: " + err.Error())
	}
	return h
}()
