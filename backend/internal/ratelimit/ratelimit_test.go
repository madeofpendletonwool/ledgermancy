package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newTestLimiter builds a Limiter with a controllable clock and no janitor
// goroutine, so tests are deterministic and do not sleep.
func newTestLimiter(limit int, window time.Duration, now *time.Time) *Limiter {
	return &Limiter{
		limit:   limit,
		window:  window,
		buckets: make(map[string]*bucket),
		now:     func() time.Time { return *now },
	}
}

func TestAllowUpToLimitThenRejects(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newTestLimiter(3, time.Minute, &now)

	for i := 1; i <= 3; i++ {
		if ok, _ := l.Allow("client"); !ok {
			t.Fatalf("attempt %d was rejected but is within the limit", i)
		}
	}

	ok, retryAfter := l.Allow("client")
	if ok {
		t.Fatal("attempt 4 was allowed past a limit of 3")
	}
	if retryAfter <= 0 || retryAfter > time.Minute {
		t.Errorf("retryAfter = %v, want a positive value within the window", retryAfter)
	}
}

func TestWindowResets(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newTestLimiter(2, time.Minute, &now)

	l.Allow("client")
	l.Allow("client")
	if ok, _ := l.Allow("client"); ok {
		t.Fatal("limit was not enforced before the window elapsed")
	}

	now = now.Add(time.Minute + time.Second)

	if ok, _ := l.Allow("client"); !ok {
		t.Fatal("limit was still enforced after the window elapsed")
	}
}

// One attacker must not be able to lock everyone else out.
func TestKeysAreIndependent(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newTestLimiter(1, time.Minute, &now)

	if ok, _ := l.Allow("attacker"); !ok {
		t.Fatal("first attempt rejected")
	}
	if ok, _ := l.Allow("attacker"); ok {
		t.Fatal("attacker exceeded their own limit")
	}

	if ok, _ := l.Allow("innocent"); !ok {
		t.Fatal("a different key was throttled by someone else's attempts")
	}
}

// A successful login resets the caller's budget, so someone who mistyped their
// password a few times is not left throttled afterwards.
func TestResetClearsCounter(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newTestLimiter(2, time.Minute, &now)

	l.Allow("client")
	l.Allow("client")
	if ok, _ := l.Allow("client"); ok {
		t.Fatal("limit was not reached")
	}

	l.Reset("client")

	if ok, _ := l.Allow("client"); !ok {
		t.Fatal("Reset did not clear the counter")
	}
}

func TestMiddlewareReturns429WithRetryAfter(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newTestLimiter(1, time.Minute, &now)

	var served int
	handler := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served++
		w.WriteHeader(http.StatusOK)
	}))

	call := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
		req.RemoteAddr = "192.0.2.10:54321"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	if got := call().Code; got != http.StatusOK {
		t.Fatalf("first request: status %d, want 200", got)
	}

	rec := call()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: status %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 response is missing Retry-After")
	}
	if served != 1 {
		t.Errorf("handler ran %d times, want 1 — the throttled request reached it", served)
	}
}

// The whole point of ClientIP: forwarded headers are attacker-controlled, so
// reading them here would let anyone rotate their apparent address and make
// every limit above meaningless. Only RemoteAddr is trusted, and chi's RealIP
// (mounted only behind a declared proxy) is what rewrites that when it should.
func TestClientIPIgnoresForwardedHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
	req.RemoteAddr = "192.0.2.10:54321"
	req.Header.Set("True-Client-IP", "1.2.3.4")
	req.Header.Set("X-Real-IP", "5.6.7.8")
	req.Header.Set("X-Forwarded-For", "9.10.11.12")

	if got := ClientIP(req); got != "192.0.2.10" {
		t.Errorf("ClientIP = %q, want 192.0.2.10 — a forwarded header was trusted", got)
	}
}

// Spoofing headers must not buy an attacker extra attempts.
func TestMiddlewareCannotBeEvadedWithSpoofedHeaders(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newTestLimiter(2, time.Minute, &now)

	handler := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	statuses := make([]int, 0, 5)
	for i := range 5 {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
		req.RemoteAddr = "192.0.2.10:54321"
		// A new claimed identity on every attempt.
		req.Header.Set("True-Client-IP", string(rune('a'+i)))
		req.Header.Set("X-Forwarded-For", string(rune('a'+i)))

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		statuses = append(statuses, rec.Code)
	}

	for i, code := range statuses[2:] {
		if code != http.StatusTooManyRequests {
			t.Errorf("attempt %d: status %d, want 429 — header spoofing evaded the limit",
				i+3, code)
		}
	}
}

func TestClientIPHandlesAddressWithoutPort(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "unix"

	if got := ClientIP(req); got != "unix" {
		t.Errorf("ClientIP = %q, want the raw RemoteAddr back", got)
	}
}
