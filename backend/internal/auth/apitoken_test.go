package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

// BearerToken's second return is what stops the CSRF exemption becoming a CSRF
// hole: any request carrying an Authorization header is a token request, so it
// can never fall through to the session cookie. The cases below are the ones
// where "there is no usable token here" and "this is not a token request" are
// easy to conflate.
func TestBearerTokenPresence(t *testing.T) {
	cases := []struct {
		name        string
		header      string
		wantToken   string
		wantPresent bool
	}{
		{"no header at all is a cookie request", "", "", false},
		{"an ordinary bearer token", "Bearer lgm_abc", "lgm_abc", true},
		{"the scheme is case-insensitive", "bearer lgm_abc", "lgm_abc", true},
		{"surrounding space is trimmed", "Bearer   lgm_abc  ", "lgm_abc", true},
		// The dangerous ones. Each is a request an attacker could shape, and each
		// must be answered with 401 rather than with the browser's cookie.
		{"an empty bearer value is still a token request", "Bearer ", "", true},
		{"a bare scheme is still a token request", "Bearer", "", true},
		{"another scheme is still not a cookie request", "Basic dXNlcjpwdw==", "", true},
		{"garbage is still not a cookie request", "nonsense", "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}

			token, present := BearerToken(r)
			if present != tc.wantPresent {
				t.Errorf("present: got %v, want %v", present, tc.wantPresent)
			}
			if token != tc.wantToken {
				t.Errorf("token: got %q, want %q", token, tc.wantToken)
			}
		})
	}
}

func TestNormalizeScopes(t *testing.T) {
	cases := []struct {
		name      string
		requested []string
		want      []string
		wantOK    bool
	}{
		{"nothing asked for is read-only", nil, []string{ScopeRead}, true},
		{"read stays read", []string{ScopeRead}, []string{ScopeRead}, true},
		// Write implies read: a token that could change a balance but not see
		// one would be a shape nobody asked for.
		{"write implies read", []string{ScopeWrite}, []string{ScopeRead, ScopeWrite}, true},
		{"order is canonical", []string{ScopeWrite, ScopeRead}, []string{ScopeRead, ScopeWrite}, true},
		{"case and space are tolerated", []string{" Write "}, []string{ScopeRead, ScopeWrite}, true},
		// Rejected rather than dropped: silently ignoring a scope hands back a
		// token that does less than the caller was told it does.
		{"an unknown scope is rejected", []string{ScopeRead, "admin"}, nil, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := NormalizeScopes(tc.requested)
			if ok != tc.wantOK {
				t.Fatalf("ok: got %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// AllowsWrite is the credential half of the write decision. A session is
// unrestricted; a token is only as good as its scopes.
func TestIdentityAllowsWrite(t *testing.T) {
	id := func(scopes ...string) Identity {
		tokenID := uuid.New()
		return Identity{APITokenID: &tokenID, Scopes: scopes}
	}

	if !(Identity{}).AllowsWrite() {
		t.Error("a session identity must be allowed to write")
	}
	if id(ScopeRead).AllowsWrite() {
		t.Error("a read-only token must not be allowed to write")
	}
	if !id(ScopeRead, ScopeWrite).AllowsWrite() {
		t.Error("a read-write token must be allowed to write")
	}
}

func TestIsStateChanging(t *testing.T) {
	safe := []string{http.MethodGet, http.MethodHead, http.MethodOptions}
	for _, method := range safe {
		if IsStateChanging(method) {
			t.Errorf("%s should be safe", method)
		}
	}
	// Allowlisted on purpose: a method nobody has thought about yet counts as a
	// write rather than sliding past both the scope check and the CSRF guard.
	unsafe := []string{
		http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, "PROPPATCH",
	}
	for _, method := range unsafe {
		if !IsStateChanging(method) {
			t.Errorf("%s should be state-changing", method)
		}
	}
}
