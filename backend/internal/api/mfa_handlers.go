package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/apex42group/ledgermancy/backend/internal/auth"
	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
	"github.com/apex42group/ledgermancy/backend/internal/ratelimit"
)

// totpIssuer is the label an authenticator app shows next to the account.
const totpIssuer = "Ledgermancy"

type mfaStatusResponse struct {
	Enabled                bool       `json:"enabled"`
	ConfirmedAt            *time.Time `json:"confirmed_at"`
	RecoveryCodesRemaining int64      `json:"recovery_codes_remaining"`
	// True when a secret has been generated but never confirmed, so the UI can
	// offer to resume or restart a half-finished enrolment.
	SetupPending bool `json:"setup_pending"`
}

func (s *Server) handleMFAStatus(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	row, err := s.Queries.GetUserMFA(r.Context(), identity.UserID)
	if err != nil {
		s.internalError(w, "get mfa status", err)
		return
	}

	remaining, err := s.Queries.CountUnusedRecoveryCodes(r.Context(), identity.UserID)
	if err != nil {
		s.internalError(w, "count recovery codes", err)
		return
	}

	writeJSON(w, http.StatusOK, mfaStatusResponse{
		Enabled:                row.TotpEnabled,
		ConfirmedAt:            row.TotpConfirmedAt,
		RecoveryCodesRemaining: remaining,
		SetupPending:           !row.TotpEnabled && len(row.TotpSecretEncrypted) > 0,
	})
}

type mfaSetupRequest struct {
	Password string `json:"password"`
}

type mfaSetupResponse struct {
	// QRPNG is a self-contained data: URI, so the page needs no external image
	// host and no QR library in the browser.
	QRPNG   string `json:"qr_png"`
	Secret  string `json:"secret"`
	Account string `json:"account"`
}

// handleMFASetup generates a secret and returns it for the user to scan.
//
// It requires the password again even though the caller already holds a valid
// session. Without that, anyone who has stolen a session could bind their own
// authenticator to the account and lock the real owner out — turning MFA from
// a defence into the attacker's tool. The secret is stored disabled: enrolment
// is not complete until handleMFAActivate proves the user can generate a code.
func (s *Server) handleMFASetup(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	var req mfaSetupRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if !s.reauthenticate(w, r, identity.UserID, req.Password) {
		return
	}

	key, err := auth.GenerateTOTPSecret(totpIssuer, identity.Email)
	if err != nil {
		s.internalError(w, "generate totp secret", err)
		return
	}

	sealed, err := s.Cipher.SealString(key.Secret())
	if err != nil {
		s.internalError(w, "encrypt totp secret", err)
		return
	}

	if err := s.Queries.SetPendingTOTPSecret(r.Context(), dbgen.SetPendingTOTPSecretParams{
		ID:                  identity.UserID,
		TotpSecretEncrypted: sealed,
	}); err != nil {
		s.internalError(w, "store totp secret", err)
		return
	}

	qr, err := auth.TOTPQRDataURI(key)
	if err != nil {
		s.internalError(w, "render totp qr", err)
		return
	}

	writeJSON(w, http.StatusOK, mfaSetupResponse{
		QRPNG:   qr,
		Secret:  key.Secret(),
		Account: identity.Email,
	})
}

type mfaCodeRequest struct {
	Code string `json:"code"`
}

type recoveryCodesResponse struct {
	// Shown exactly once. Only HMAC hashes are stored, so these cannot be
	// retrieved later — the user must save them now or regenerate.
	RecoveryCodes []string `json:"recovery_codes"`
}

// handleMFAActivate completes enrolment.
func (s *Server) handleMFAActivate(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	var req mfaCodeRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	row, err := s.Queries.GetUserMFA(r.Context(), identity.UserID)
	if err != nil {
		s.internalError(w, "get mfa state", err)
		return
	}
	if len(row.TotpSecretEncrypted) == 0 {
		writeError(w, http.StatusBadRequest, "start setup before confirming a code")
		return
	}
	if row.TotpEnabled {
		writeError(w, http.StatusConflict, "two-factor authentication is already enabled")
		return
	}

	secret, err := s.Cipher.OpenString(row.TotpSecretEncrypted)
	if err != nil {
		s.internalError(w, "decrypt totp secret", err)
		return
	}

	step, ok := auth.ValidateTOTP(secret, req.Code, time.Now())
	if !ok {
		s.audit(r.Context(), r, identity.UserID, "", eventMFAFailed,
			map[string]any{"stage": "activate"})
		writeError(w, http.StatusUnauthorized, "that code is not correct; check your app and try again")
		return
	}

	if err := s.Queries.ActivateTOTP(r.Context(), dbgen.ActivateTOTPParams{
		ID: identity.UserID, TotpLastStep: &step,
	}); err != nil {
		s.internalError(w, "activate totp", err)
		return
	}

	codes, err := s.regenerateRecoveryCodes(r.Context(), identity.UserID)
	if err != nil {
		s.internalError(w, "generate recovery codes", err)
		return
	}

	// Turning on MFA should evict anyone already holding a session for this
	// account — that is a large part of the point of enabling it. The caller's
	// own session survives so they are not bounced out mid-setup.
	if _, err := s.Queries.DeleteOtherUserSessions(r.Context(), dbgen.DeleteOtherUserSessionsParams{
		UserID: identity.UserID, TokenHash: identity.TokenHash,
	}); err != nil {
		s.internalError(w, "revoke other sessions", err)
		return
	}

	s.audit(r.Context(), r, identity.UserID, "", eventMFAEnabled, nil)
	writeJSON(w, http.StatusOK, recoveryCodesResponse{RecoveryCodes: codes})
}

type mfaDisableRequest struct {
	Password string `json:"password"`
	Code     string `json:"code"`
}

// handleMFADisable turns MFA off, requiring both factors to do it.
//
// Requiring the code as well as the password is the point: if a stolen
// password alone could strip the second factor, the second factor would not be
// protecting anything.
func (s *Server) handleMFADisable(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	var req mfaDisableRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if !s.reauthenticate(w, r, identity.UserID, req.Password) {
		return
	}

	row, err := s.Queries.GetUserMFA(r.Context(), identity.UserID)
	if err != nil {
		s.internalError(w, "get mfa state", err)
		return
	}
	if !row.TotpEnabled {
		writeError(w, http.StatusBadRequest, "two-factor authentication is not enabled")
		return
	}

	if ok, err := s.consumeTOTP(r.Context(), row, req.Code); err != nil {
		s.internalError(w, "verify totp", err)
		return
	} else if !ok {
		s.audit(r.Context(), r, identity.UserID, "", eventMFAFailed,
			map[string]any{"stage": "disable"})
		writeError(w, http.StatusUnauthorized, "that code is not correct")
		return
	}

	if err := s.Queries.DisableTOTP(r.Context(), identity.UserID); err != nil {
		s.internalError(w, "disable totp", err)
		return
	}
	if err := s.Queries.DeleteUserRecoveryCodes(r.Context(), identity.UserID); err != nil {
		s.internalError(w, "clear recovery codes", err)
		return
	}

	s.audit(r.Context(), r, identity.UserID, "", eventMFADisabled, nil)
	w.WriteHeader(http.StatusNoContent)
}

// handleMFARecoveryCodes issues a fresh set, invalidating the old ones.
func (s *Server) handleMFARecoveryCodes(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	var req mfaSetupRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if !s.reauthenticate(w, r, identity.UserID, req.Password) {
		return
	}

	row, err := s.Queries.GetUserMFA(r.Context(), identity.UserID)
	if err != nil {
		s.internalError(w, "get mfa state", err)
		return
	}
	if !row.TotpEnabled {
		writeError(w, http.StatusBadRequest, "enable two-factor authentication first")
		return
	}

	codes, err := s.regenerateRecoveryCodes(r.Context(), identity.UserID)
	if err != nil {
		s.internalError(w, "generate recovery codes", err)
		return
	}

	s.audit(r.Context(), r, identity.UserID, "", eventRecoveryRotated, nil)
	writeJSON(w, http.StatusOK, recoveryCodesResponse{RecoveryCodes: codes})
}

type mfaVerifyRequest struct {
	Code         string `json:"code"`
	RecoveryCode string `json:"recovery_code"`
}

// handleMFAVerify completes a login that stopped at the second factor.
//
// This is the only unauthenticated MFA route. It is reached with the
// httpOnly challenge cookie set by handleLogin, and it is the single place
// where passing the second factor turns into an actual session.
func (s *Server) handleMFAVerify(w http.ResponseWriter, r *http.Request) {
	var req mfaVerifyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	cookie, err := r.Cookie(auth.MFACookieName)
	if err != nil || cookie.Value == "" {
		writeError(w, http.StatusUnauthorized, "no pending sign-in; start again")
		return
	}

	ctx := r.Context()
	challenge, err := s.Queries.GetMFAChallenge(ctx, auth.HashToken(s.Config.SessionSecret, cookie.Value))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			auth.ClearMFACookie(w, s.cookieOptions())
			writeError(w, http.StatusUnauthorized, "this sign-in expired; please start again")
			return
		}
		s.internalError(w, "look up mfa challenge", err)
		return
	}

	// Count the attempt before checking it, so a crash or disconnect mid-check
	// cannot be used to get a free guess.
	attempts, err := s.Queries.IncrementMFAChallengeAttempts(ctx, challenge.ID)
	if err != nil {
		s.internalError(w, "record mfa attempt", err)
		return
	}
	if attempts > auth.MaxMFAAttempts {
		s.failChallenge(w, r, challenge, "too many attempts; please sign in again")
		return
	}

	row, err := s.Queries.GetUserMFA(ctx, challenge.UserID)
	if err != nil {
		s.internalError(w, "get mfa state", err)
		return
	}

	verified := false
	usedRecovery := false

	switch {
	case req.RecoveryCode != "":
		affected, err := s.Queries.ConsumeRecoveryCode(ctx, dbgen.ConsumeRecoveryCodeParams{
			UserID: challenge.UserID,
			CodeHash: auth.HashToken(s.Config.SessionSecret,
				auth.NormalizeRecoveryCode(req.RecoveryCode)),
		})
		if err != nil {
			s.internalError(w, "consume recovery code", err)
			return
		}
		verified = affected > 0
		usedRecovery = verified

	case req.Code != "":
		ok, err := s.consumeTOTP(ctx, row, req.Code)
		if err != nil {
			s.internalError(w, "verify totp", err)
			return
		}
		verified = ok

	default:
		writeError(w, http.StatusBadRequest, "a code is required")
		return
	}

	if !verified {
		s.audit(ctx, r, challenge.UserID, "", eventMFAFailed,
			map[string]any{"stage": "login", "attempt": attempts})
		writeError(w, http.StatusUnauthorized, "that code is not correct")
		return
	}

	// One challenge, one session. Deleting it here means a code cannot be
	// replayed against the same pending login even if the response is lost.
	if err := s.Queries.DeleteMFAChallenge(ctx, challenge.ID); err != nil {
		s.internalError(w, "clear mfa challenge", err)
		return
	}
	auth.ClearMFACookie(w, s.cookieOptions())

	if err := s.Queries.ClearFailedLogins(ctx, challenge.UserID); err != nil {
		s.internalError(w, "clear failed logins", err)
		return
	}

	if err := s.startSession(w, r, challenge.UserID); err != nil {
		s.internalError(w, "start session", err)
		return
	}

	if usedRecovery {
		remaining, err := s.Queries.CountUnusedRecoveryCodes(ctx, challenge.UserID)
		if err != nil {
			// Not worth failing a successful login over; the count is
			// cosmetic and the security settings page reports it accurately.
			remaining = -1
		}
		s.audit(ctx, r, challenge.UserID, "", eventRecoveryUsed,
			map[string]any{"remaining": remaining})
	}
	s.audit(ctx, r, challenge.UserID, "", eventMFASucceeded,
		map[string]any{"method": methodLabel(usedRecovery)})

	s.loginLimiter.Reset(ratelimit.ClientIP(r))

	writeJSON(w, http.StatusOK, userResponse{
		ID:          challenge.UserID,
		HouseholdID: challenge.HouseholdID,
		Email:       challenge.Email,
		DisplayName: challenge.DisplayName,
	})
}

func methodLabel(usedRecovery bool) string {
	if usedRecovery {
		return "recovery_code"
	}
	return "totp"
}

// failChallenge destroys a challenge that has run out of attempts.
func (s *Server) failChallenge(
	w http.ResponseWriter, r *http.Request, challenge dbgen.GetMFAChallengeRow, message string,
) {
	if err := s.Queries.DeleteMFAChallenge(r.Context(), challenge.ID); err != nil {
		s.internalError(w, "clear mfa challenge", err)
		return
	}
	auth.ClearMFACookie(w, s.cookieOptions())
	s.audit(r.Context(), r, challenge.UserID, "", eventMFAFailed,
		map[string]any{"stage": "login", "reason": "attempts_exhausted"})
	writeError(w, http.StatusUnauthorized, message)
}

// consumeTOTP validates a code and claims its time step in one operation.
//
// The claim is what stops replay. A code stays valid for 90 seconds, so
// without recording the step it matched, anyone who observed it — over a
// shoulder, in a log, through a proxy — could reuse it inside that window.
// The conditional UPDATE also settles the race between two simultaneous
// requests carrying the same code: exactly one moves the step forward.
func (s *Server) consumeTOTP(ctx context.Context, row dbgen.GetUserMFARow, code string) (bool, error) {
	if len(row.TotpSecretEncrypted) == 0 {
		return false, nil
	}

	secret, err := s.Cipher.OpenString(row.TotpSecretEncrypted)
	if err != nil {
		return false, err
	}

	step, ok := auth.ValidateTOTP(secret, code, time.Now())
	if !ok {
		return false, nil
	}
	if row.TotpLastStep != nil && step <= *row.TotpLastStep {
		return false, nil // already used
	}

	affected, err := s.Queries.SetTOTPLastStep(ctx, dbgen.SetTOTPLastStepParams{
		ID: row.ID, Step: &step,
	})
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

// regenerateRecoveryCodes replaces a user's codes and returns the plaintext.
func (s *Server) regenerateRecoveryCodes(ctx context.Context, userID uuid.UUID) ([]string, error) {
	codes, err := auth.NewRecoveryCodes(auth.RecoveryCodeCount)
	if err != nil {
		return nil, err
	}

	if err := s.Queries.DeleteUserRecoveryCodes(ctx, userID); err != nil {
		return nil, err
	}

	for _, code := range codes {
		if err := s.Queries.CreateRecoveryCode(ctx, dbgen.CreateRecoveryCodeParams{
			UserID: userID,
			CodeHash: auth.HashToken(s.Config.SessionSecret,
				auth.NormalizeRecoveryCode(code)),
		}); err != nil {
			return nil, err
		}
	}
	return codes, nil
}

// reauthenticate re-checks the caller's password, writing the error response
// itself. It reports whether the handler should continue.
//
// Every MFA change funnels through this. Holding a session is not sufficient
// authority to alter the factors that guard the account.
func (s *Server) reauthenticate(
	w http.ResponseWriter, r *http.Request, userID uuid.UUID, password string,
) bool {
	user, err := s.Queries.GetUserByID(r.Context(), userID)
	if err != nil {
		s.internalError(w, "look up user", err)
		return false
	}

	if err := auth.VerifyPassword(password, user.PasswordHash); err != nil {
		writeError(w, http.StatusUnauthorized, "that password is not correct")
		return false
	}
	return true
}
