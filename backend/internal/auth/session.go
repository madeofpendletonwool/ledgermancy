package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"
)

// SessionCookieName is the cookie carrying the opaque session token.
const SessionCookieName = "ledgermancy_session"

// CSRFCookieName carries a CSRF token readable by JavaScript, which the
// frontend echoes back in the X-CSRF-Token header on unsafe requests.
const CSRFCookieName = "ledgermancy_csrf"

// CSRFHeaderName is the header the frontend must send on unsafe requests.
const CSRFHeaderName = "X-CSRF-Token"

// MFACookieName carries a pending multi-factor challenge: the password was
// correct, but the second factor has not been presented yet.
//
// It is httpOnly for the same reason the session cookie is — a half-completed
// login is still worth stealing — and short-lived, because a challenge left
// open is an attacker's second chance at the code.
const MFACookieName = "ledgermancy_mfa"

// MFAChallengeTTL is how long a user has to enter their code after the
// password step. Long enough to find a phone, short enough that an abandoned
// challenge is not left lying around.
const MFAChallengeTTL = 5 * time.Minute

// MaxMFAAttempts is how many codes may be tried against one challenge before
// it is destroyed and the user must re-enter their password. This is the cap
// that matters most: it bounds guesses against a single login to well under
// the 1-in-a-million a six-digit code is worth.
const MaxMFAAttempts = 5

// NewToken returns a cryptographically random, URL-safe session token.
func NewToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// HashToken derives the value stored in the database for a session token.
//
// It is keyed with the application's session secret (HMAC-SHA256) so that a
// leaked database alone cannot be used to forge or recognise live sessions.
// Tokens are high-entropy random values, so a single fast hash is appropriate
// here — unlike passwords, which need argon2id.
func HashToken(secret []byte, token string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(token))
	return hex.EncodeToString(mac.Sum(nil))
}

// CookieOptions describes how auth cookies should be written for the current
// environment. In production cookies are Secure; over plain-HTTP local
// development they cannot be, or the browser silently drops them.
type CookieOptions struct {
	Secure bool
}

// SetSessionCookie writes the session cookie.
//
// SameSite=Strict plus httpOnly means the token is never readable by scripts
// and is not attached to cross-site requests, which removes the bulk of both
// XSS token theft and CSRF risk.
func SetSessionCookie(w http.ResponseWriter, opts CookieOptions, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   opts.Secure,
		SameSite: http.SameSiteStrictMode,
	})
}

// SetCSRFCookie writes the CSRF token. This one is deliberately not httpOnly:
// the frontend must read it to echo it back in a header.
func SetCSRFCookie(w http.ResponseWriter, opts CookieOptions, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookieName,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		HttpOnly: false,
		Secure:   opts.Secure,
		SameSite: http.SameSiteStrictMode,
	})
}

// SetMFACookie writes the pending-challenge cookie.
func SetMFACookie(w http.ResponseWriter, opts CookieOptions, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     MFACookieName,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   opts.Secure,
		SameSite: http.SameSiteStrictMode,
	})
}

// ClearMFACookie expires the challenge cookie, on success or on abandonment.
func ClearMFACookie(w http.ResponseWriter, opts CookieOptions) {
	http.SetCookie(w, &http.Cookie{
		Name:     MFACookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   opts.Secure,
		SameSite: http.SameSiteStrictMode,
	})
}

// ClearAuthCookies expires every auth cookie on logout. The MFA cookie is
// included so an abandoned challenge cannot outlive the logout that followed it.
func ClearAuthCookies(w http.ResponseWriter, opts CookieOptions) {
	for _, name := range []string{SessionCookieName, CSRFCookieName, MFACookieName} {
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			HttpOnly: name != CSRFCookieName,
			Secure:   opts.Secure,
			SameSite: http.SameSiteStrictMode,
		})
	}
}
