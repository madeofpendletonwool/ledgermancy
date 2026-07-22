package api

import "net/http"

// securityHeaders sets the response headers that constrain what a browser will
// do with an API response.
//
// nginx sets an overlapping set for the SPA it serves (see frontend/nginx.conf),
// but these are applied here as well on purpose: the api is a separately
// runnable process, and a deployment that puts something other than our nginx
// in front of it must not silently lose them.
//
// There is no Content-Security-Policy here. CSP governs how a *document*
// loads subresources, and this handler only ever emits JSON and CSV — the
// policy that matters belongs on the HTML, which nginx serves.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()

		// Without this, a JSON error body can be sniffed as HTML and executed.
		h.Set("X-Content-Type-Options", "nosniff")
		// Nothing here is meant to be framed. X-Frame-Options is the legacy
		// spelling; CSP frame-ancestors on the HTML covers modern browsers.
		h.Set("X-Frame-Options", "DENY")
		// Account balances have no business appearing in a Referer header.
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Permissions-Policy",
			"geolocation=(), camera=(), microphone=(), payment=(), usb=()")

		// Every response from this API is either someone's financial data or an
		// auth exchange. no-store keeps all of it out of intermediary caches
		// and off disk — which matters most for the CSV exports, which browsers
		// would otherwise be free to cache as ordinary downloads.
		h.Set("Cache-Control", "no-store, no-cache, must-revalidate, private")
		h.Set("Pragma", "no-cache")

		// HSTS is only meaningful, and only safe, over a connection that
		// already arrived via TLS. Sending it on plain HTTP would either be
		// ignored or would lock a developer out of their local http:// setup.
		if s.isHTTPS(r) {
			h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		}

		next.ServeHTTP(w, r)
	})
}

// isHTTPS reports whether the original client request used TLS.
//
// X-Forwarded-Proto is only consulted when the deployment has declared it sits
// behind a proxy. Trusting it unconditionally would let any client claim its
// plain-HTTP request was secure, which in turn would make the api mark cookies
// Secure on a connection that cannot carry them.
func (s *Server) isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return s.Config.TrustProxyHeaders && r.Header.Get("X-Forwarded-Proto") == "https"
}
