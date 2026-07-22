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

// ClearAuthCookies expires both auth cookies on logout.
func ClearAuthCookies(w http.ResponseWriter, opts CookieOptions) {
	for _, name := range []string{SessionCookieName, CSRFCookieName} {
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			HttpOnly: name == SessionCookieName,
			Secure:   opts.Secure,
			SameSite: http.SameSiteStrictMode,
		})
	}
}
