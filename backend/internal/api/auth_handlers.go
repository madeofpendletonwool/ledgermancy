package api

import (
	"errors"
	"log/slog"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/apex42group/ledgermancy/backend/internal/auth"
	"github.com/apex42group/ledgermancy/backend/internal/config"
	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
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
	if len([]rune(req.Password)) < minPasswordLength {
		writeError(w, http.StatusBadRequest,
			"password must be at least 12 characters")
		return
	}
	if req.DisplayName == "" {
		writeError(w, http.StatusBadRequest, "display_name is required")
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

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx := r.Context()
	user, err := s.Queries.GetUserByEmail(ctx, strings.TrimSpace(req.Email))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Spend the same work as a real verification so response timing
			// does not reveal whether the address is registered.
			_ = auth.VerifyPassword(req.Password, decoyHash)
			writeError(w, http.StatusUnauthorized, "invalid email or password")
			return
		}
		s.internalError(w, "look up user", err)
		return
	}

	if err := auth.VerifyPassword(req.Password, user.PasswordHash); err != nil {
		if !errors.Is(err, auth.ErrMismatchedPassword) {
			// A malformed stored hash is corruption, not a failed login.
			slog.Error("verify password", "error", err, "user_id", user.ID)
		}
		writeError(w, http.StatusUnauthorized, "invalid email or password")
		return
	}

	if err := s.startSession(w, r, user.ID); err != nil {
		s.internalError(w, "start session", err)
		return
	}

	writeJSON(w, http.StatusOK, userResponse{
		ID:          user.ID,
		HouseholdID: user.HouseholdID,
		Email:       user.Email,
		DisplayName: user.DisplayName,
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(auth.SessionCookieName); err == nil && cookie.Value != "" {
		if err := s.Queries.DeleteSession(r.Context(),
			auth.HashToken(s.Config.SessionSecret, cookie.Value)); err != nil {
			s.internalError(w, "delete session", err)
			return
		}
	}
	auth.ClearAuthCookies(w, s.cookieOptions())
	w.WriteHeader(http.StatusNoContent)
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

	var userAgent *string
	if ua := r.UserAgent(); ua != "" {
		userAgent = &ua
	}

	if _, err := s.Queries.CreateSession(r.Context(), dbgen.CreateSessionParams{
		UserID:    userID,
		TokenHash: auth.HashToken(s.Config.SessionSecret, token),
		UserAgent: userAgent,
		ExpiresAt: expiresAt,
	}); err != nil {
		return err
	}

	opts := s.cookieOptions()
	auth.SetSessionCookie(w, opts, token, expiresAt)
	auth.SetCSRFCookie(w, opts, csrfToken, expiresAt)
	return nil
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
