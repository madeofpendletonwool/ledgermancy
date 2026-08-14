package auth

import (
	"net/http"
	"strings"
)

// Scopes a personal API token can carry.
//
// There are two, and the split is between reading and changing rather than
// between features, because that is the distinction a user can actually reason
// about when handing a credential to somebody else's program. A dashboard tile
// gets ScopeRead and cannot move money no matter what it is asked to do.
//
// Deliberately not a boolean column. The additions worth making later are
// narrower reads ("transactions:read"), and a boolean would have to be migrated
// away the first time one arrives.
const (
	// ScopeRead is on every token. A token without it is meaningless, and the
	// database rejects one.
	ScopeRead = "read"
	// ScopeWrite additionally permits state-changing methods. Its absence is
	// what "read-only" means, and it is enforced in Authenticate rather than in
	// any handler — see the comment there.
	ScopeWrite = "write"
)

// APITokenPrefix marks a value as one of ours.
//
// It buys two things for four bytes. A user who finds the string in a config
// file can tell what it is, and secret scanners can be taught one pattern that
// matches every Ledgermancy token and nothing else. It is part of the token, so
// it is covered by the HMAC like everything else — there is no separate
// unauthenticated "id" half to keep in step.
const APITokenPrefix = "lgm_"

// NewAPIToken returns a fresh personal API token in plaintext.
//
// The entropy is NewToken's — 256 bits, the same as a session token — because a
// bearer credential that outlives every session is the last thing that should be
// weaker than one.
func NewAPIToken() (string, error) {
	token, err := NewToken()
	if err != nil {
		return "", err
	}
	return APITokenPrefix + token, nil
}

// NormalizeScopes validates a requested scope set and returns it in a canonical
// order, so two tokens asking for the same permissions are stored identically.
//
// ScopeRead is added when it is missing rather than rejected: every token can
// read, and refusing {"write"} would only teach callers to send a set they do
// not care about. An unknown scope IS rejected — silently dropping it would
// hand back a token that does less than the caller was told it does.
func NormalizeScopes(requested []string) ([]string, bool) {
	write := false
	for _, scope := range requested {
		switch strings.TrimSpace(strings.ToLower(scope)) {
		case ScopeRead:
		case ScopeWrite:
			write = true
		default:
			return nil, false
		}
	}
	if write {
		return []string{ScopeRead, ScopeWrite}, true
	}
	return []string{ScopeRead}, true
}

// ScopesAllowWrite reports whether a scope set permits state-changing requests.
func ScopesAllowWrite(scopes []string) bool {
	for _, scope := range scopes {
		if scope == ScopeWrite {
			return true
		}
	}
	return false
}

// BearerToken returns the credential from an Authorization header, and false
// when the request carries no bearer header at all.
//
// The bool is what the rest of the auth path keys off, and it is separate from
// "the token was any good" on purpose: a request that PRESENTS a bearer header
// is a token request even if the value is empty or garbage, and must never fall
// back to the session cookie. See Middleware.identify.
func BearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return "", false
	}
	// The scheme is case-insensitive per RFC 7235, and clients do vary.
	scheme, value, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		// Some other scheme (Basic, Negotiate). Not ours — and not a cookie
		// request either, so it still returns true: a request carrying an
		// Authorization header must never fall back to whoever's cookie
		// happens to ride along with it.
		return "", true
	}
	return strings.TrimSpace(value), true
}

// IsStateChanging reports whether a method may alter server state.
//
// Written as an allowlist of safe methods, so a method nobody has thought about
// yet is treated as a write. Shared by the read-only scope check and the CSRF
// guard, which have to agree on the answer or one of them is protecting a
// different set of requests than the other.
func IsStateChanging(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}
