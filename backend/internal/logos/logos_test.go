package logos

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestNormaliseDomainAccepts covers the shapes a model actually returns. The
// tidying is deliberate: refusing "https://www.amazon.com/" would throw away a
// correct answer over punctuation.
func TestNormaliseDomainAccepts(t *testing.T) {
	cases := map[string]string{
		"amazon.com":                 "amazon.com",
		"AMAZON.COM":                 "amazon.com",
		"  traderjoes.com  ":         "traderjoes.com",
		"www.amazon.com":             "amazon.com",
		"https://www.amazon.com/":    "amazon.com",
		"http://amazon.com/path?q=1": "amazon.com",
		// The path is discarded rather than being grounds for refusal: only the
		// host is ever used, so traversal in the tail is inert.
		"amazon.com/../../secret":     "amazon.com",
		"amazon.com.":                 "amazon.com",
		"marks-and-spencer.co.uk":     "marks-and-spencer.co.uk",
		"xn--80ak6aa92e.com":          "xn--80ak6aa92e.com",
		"shop.bluebottlecoffee.co.jp": "shop.bluebottlecoffee.co.jp",
	}
	for in, want := range cases {
		got, ok := NormaliseDomain(in)
		if !ok {
			t.Errorf("NormaliseDomain(%q) refused it", in)
			continue
		}
		if got != want {
			t.Errorf("NormaliseDomain(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestNormaliseDomainRefuses is the load-bearing half: the result is
// interpolated into an outbound URL, and the model's input ultimately came from
// a bank feed. Anything that is not unmistakably a hostname has to be refused.
func TestNormaliseDomainRefuses(t *testing.T) {
	for _, in := range []string{
		"",
		"   ",
		"unknown",             // the model answering in prose
		"localhost",           // no dot, and not a merchant
		"127.0.0.1",           // an address, never a website
		"192.168.1.1",         //
		"amazon.com:8080",     // a port
		"user@amazon.com",     // credentials
		"amazon com",          // a space
		"amazon.com amzn.com", // two answers in one string
		"amazon..com",
		"-amazon.com",
		"amazon.com-",
		"file:///etc/passwd",
		"http://[::1]/",
		strings.Repeat("a", 250) + ".com", // over the DNS length limit
	} {
		if got, ok := NormaliseDomain(in); ok {
			t.Errorf("NormaliseDomain(%q) accepted it as %q", in, got)
		}
	}
}

// pngBytes builds a tiny real PNG, so the content sniffing under test sees the
// bytes it would see in production rather than a string pretending to be one.
func pngBytes(t *testing.T, side int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, side, side))
	for x := range side {
		for y := range side {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func testFetcher(t *testing.T, h http.HandlerFunc, maxBytes int64) *Fetcher {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	f := NewFetcher("test-token", 128, maxBytes)
	f.BaseURL = srv.URL
	return f
}

// TestFetchReturnsImage also pins the request: fallback=404 is what stops
// Logo.dev answering a 200 with its own grey monogram, which would mean caching
// their placeholder in place of the app's coloured one.
func TestFetchReturnsImage(t *testing.T) {
	want := pngBytes(t, 32)

	var gotPath string
	var gotQuery url.Values
	f := testFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query()
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(want)
	}, 1<<20)

	got, contentType, err := f.Fetch(context.Background(), "amazon.com")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("got %d bytes, want %d", len(got), len(want))
	}
	if contentType != "image/png" {
		t.Errorf("content type = %q, want image/png", contentType)
	}
	if gotPath != "/amazon.com" {
		t.Errorf("path = %q, want /amazon.com", gotPath)
	}
	if gotQuery.Get("fallback") != "404" {
		t.Errorf("fallback = %q, want 404 (otherwise the host answers 200 with its own monogram)",
			gotQuery.Get("fallback"))
	}
	if gotQuery.Get("token") != "test-token" {
		t.Errorf("token = %q", gotQuery.Get("token"))
	}
	if gotQuery.Get("size") != "128" {
		t.Errorf("size = %q, want 128", gotQuery.Get("size"))
	}
}

// TestFetchNoLogo covers every way "there is nothing to show" arrives. All of
// them must be ErrNoLogo rather than an error, because the caller caches that
// answer and stops asking — which is the whole reason a merchant costs one
// request ever.
func TestFetchNoLogo(t *testing.T) {
	big := pngBytes(t, 64)

	cases := map[string]struct {
		handler  http.HandlerFunc
		maxBytes int64
	}{
		"404 from the host": {
			handler:  func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) },
			maxBytes: 1 << 20,
		},
		"empty body with a 200": {
			handler:  func(w http.ResponseWriter, _ *http.Request) {},
			maxBytes: 1 << 20,
		},
		"over the size ceiling": {
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(big)
			},
			// Well under the real body, so the limit is what trips.
			maxBytes: 64,
		},
		"an SVG, which is a script-bearing document": {
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "image/svg+xml")
				_, _ = w.Write([]byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`))
			},
			maxBytes: 1 << 20,
		},
		"HTML wearing an image content type": {
			handler: func(w http.ResponseWriter, _ *http.Request) {
				// The header lies; the bytes are what decide.
				w.Header().Set("Content-Type", "image/png")
				_, _ = w.Write([]byte("<!DOCTYPE html><html><body>nope</body></html>"))
			},
			maxBytes: 1 << 20,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := testFetcher(t, tc.handler, tc.maxBytes)
			if _, _, err := f.Fetch(context.Background(), "amazon.com"); !errors.Is(err, ErrNoLogo) {
				t.Fatalf("err = %v, want ErrNoLogo", err)
			}
		})
	}
}

// TestFetchTransientErrorIsNotNoLogo separates "this merchant has no logo" from
// "the host was unhappy". Only the first is cached; caching the second would
// blank a merchant permanently over a rate limit.
func TestFetchTransientErrorIsNotNoLogo(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusBadGateway} {
		f := testFetcher(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}, 1<<20)

		_, _, err := f.Fetch(context.Background(), "amazon.com")
		if err == nil {
			t.Errorf("status %d: got no error", status)
			continue
		}
		if errors.Is(err, ErrNoLogo) {
			t.Errorf("status %d: reported as ErrNoLogo, which would cache a transient failure forever", status)
		}
	}
}

// TestFetchRefusesUnusableDomain makes sure the validator runs before the
// request, not after it — a rejected domain must cost no outbound call at all.
func TestFetchRefusesUnusableDomain(t *testing.T) {
	called := false
	f := testFetcher(t, func(w http.ResponseWriter, _ *http.Request) {
		called = true
	}, 1<<20)

	if _, _, err := f.Fetch(context.Background(), "127.0.0.1"); err == nil {
		t.Fatal("expected a refusal")
	}
	if called {
		t.Error("the host was contacted for a domain that should have been refused")
	}
}

// TestServedContentType is what the api trusts on the way back out: bytes, not
// the stored column.
func TestServedContentType(t *testing.T) {
	if got, ok := ServedContentType(pngBytes(t, 8)); !ok || got != "image/png" {
		t.Errorf("png: got %q ok=%v", got, ok)
	}
	if got, ok := ServedContentType([]byte("<!DOCTYPE html><html></html>")); ok {
		t.Errorf("html was accepted as %q", got)
	}
	if _, ok := ServedContentType(nil); ok {
		t.Error("empty bytes were accepted")
	}
}
