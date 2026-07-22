// Package ratelimit provides per-client request throttling for the HTTP API.
//
// The store is in-process, which is the right shape for this application: the
// compose file runs exactly one api container, and a Postgres round trip on
// every request would cost more than the protection is worth. It is
// deliberately paired with durable per-account backoff in the login handler —
// this package bounds how fast an attacker can try, the database columns
// remember how often they already have.
package ratelimit

import (
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Limiter is a fixed-window counter keyed by client.
//
// A fixed window can let through up to 2x the limit across a window boundary.
// That is a real property, not an oversight: the alternative (sliding window or
// token bucket with per-key timestamps) costs more memory and complexity for a
// bound that is already an order of magnitude below what an attacker needs.
type Limiter struct {
	limit  int
	window time.Duration

	mu      sync.Mutex
	buckets map[string]*bucket

	// now is swappable so tests can drive the clock instead of sleeping.
	now func() time.Time
}

type bucket struct {
	count    int
	resetsAt time.Time
}

// New returns a Limiter allowing limit requests per key per window, and starts
// a janitor that evicts stale keys so an attacker cycling source addresses
// cannot grow the map without bound.
func New(limit int, window time.Duration) *Limiter {
	l := &Limiter{
		limit:   limit,
		window:  window,
		buckets: make(map[string]*bucket),
		now:     time.Now,
	}
	go l.janitor()
	return l
}

// Allow records an attempt for key and reports whether it is under the limit.
// The second return is how long until the window resets, for Retry-After.
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b, ok := l.buckets[key]
	if !ok || now.After(b.resetsAt) {
		l.buckets[key] = &bucket{count: 1, resetsAt: now.Add(l.window)}
		return true, 0
	}

	b.count++
	if b.count > l.limit {
		return false, b.resetsAt.Sub(now)
	}
	return true, 0
}

// Reset clears a key's counter. Called after a successful login so a user who
// fumbled their password twice is not still burning through the budget.
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.buckets, key)
}

func (l *Limiter) janitor() {
	// Sweeping at the window length means an expired bucket lives at most one
	// extra window, which is bounded and cheap.
	ticker := time.NewTicker(l.window)
	defer ticker.Stop()

	for range ticker.C {
		l.mu.Lock()
		now := l.now()
		for key, b := range l.buckets {
			if now.After(b.resetsAt) {
				delete(l.buckets, key)
			}
		}
		l.mu.Unlock()
	}
}

// Middleware throttles by client IP.
//
// IMPORTANT: this is only as trustworthy as ClientIP. See the comment there —
// if forwarded headers are honoured without a proxy in front that overwrites
// them, an attacker sets their own key and this middleware does nothing.
func (l *Limiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok, retryAfter := l.Allow(ClientIP(r))
		if !ok {
			w.Header().Set("Retry-After",
				strconv.Itoa(int(retryAfter.Seconds())+1))
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"too many requests; please wait and try again"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ClientIP returns the address to key limits and audit records on.
//
// It reads RemoteAddr and nothing else. Forwarded headers are handled upstream
// by chi's RealIP middleware, which the server mounts ONLY when
// TRUST_PROXY_HEADERS is set — because RealIP rewrites RemoteAddr from headers
// that any client can send. Reading X-Forwarded-For directly here would
// reintroduce exactly the spoofing this arrangement exists to prevent.
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// RemoteAddr is not always host:port (unix sockets, some test
		// harnesses). Keying on the raw value is still consistent per client.
		return r.RemoteAddr
	}
	return host
}
