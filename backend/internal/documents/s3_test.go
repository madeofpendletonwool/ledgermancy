package documents

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/config"
)

// A hand-rolled signer that is subtly wrong fails as an opaque 403 from the
// endpoint, days after deploy. These tests pin what can be checked without a
// live bucket: the encoding rules, the header set, determinism, and that the
// signature actually covers what it claims to.

func testS3(pathStyle bool) *S3Storage {
	return &S3Storage{cfg: config.S3Config{
		Endpoint:  "https://s3.example.com",
		Region:    "us-east-1",
		Bucket:    "vault",
		AccessKey: "AKIAIOSFODNN7EXAMPLE",
		SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		PathStyle: pathStyle,
	}}
}

// awsURIEncode is the one piece with an externally-defined answer: SigV4
// specifies RFC 3986 unreserved characters only, which is stricter than
// url.PathEscape and is the usual source of "works until a prefix has a space".
func TestAWSURIEncode(t *testing.T) {
	cases := map[string]string{
		"abcXYZ019": "abcXYZ019",
		"-_.~":      "-_.~",
		" ":         "%20",
		"+":         "%2B", // url.PathEscape leaves this alone; SigV4 must not
		"$":         "%24",
		"&":         "%26",
		",":         "%2C",
		"=":         "%3D",
		"documents": "documents",
		"a b+c$d":   "a%20b%2Bc%24d",
	}
	for in, want := range cases {
		if got := awsURIEncode(in); got != want {
			t.Errorf("awsURIEncode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCanonicalURIKeepsSeparators(t *testing.T) {
	got := canonicalURI("/vault/ab/cd/0d1b2f8e-0000-4000-8000-000000000000.bin")
	want := "/vault/ab/cd/0d1b2f8e-0000-4000-8000-000000000000.bin"
	if got != want {
		t.Errorf("canonicalURI = %q, want %q", got, want)
	}
	if canonicalURI("") != "/" {
		t.Error("an empty path must canonicalise to /")
	}
	// The separators stay, everything inside a segment is encoded.
	if got := canonicalURI("/my bucket/a key"); got != "/my%20bucket/a%20key" {
		t.Errorf("canonicalURI = %q, want /my%%20bucket/a%%20key", got)
	}
}

func TestObjectPathHonoursPrefixAndStyle(t *testing.T) {
	key := "ab/cd/0d1b2f8e-0000-4000-8000-000000000000.bin"

	pathStyle := testS3(true)
	if got := pathStyle.objectPath(key); got != "/vault/"+key {
		t.Errorf("path style: %q, want /vault/%s", got, key)
	}

	virtual := testS3(false)
	if got := virtual.objectPath(key); got != "/"+key {
		t.Errorf("virtual host style: %q, want /%s", got, key)
	}
	url, err := virtual.objectURL(key)
	if err != nil {
		t.Fatalf("objectURL: %v", err)
	}
	if !strings.HasPrefix(url, "https://vault.s3.example.com/") {
		t.Errorf("virtual host URL = %q, want the bucket as a subdomain", url)
	}

	prefixed := testS3(true)
	prefixed.cfg.Prefix = "ledgermancy"
	if got := prefixed.objectPath(key); got != "/vault/ledgermancy/"+key {
		t.Errorf("prefixed: %q", got)
	}
}

// The properties a signature must have: deterministic for fixed inputs, and
// sensitive to every input it is supposed to authenticate.
func TestSignCoversMethodPathAndBody(t *testing.T) {
	s := testS3(true)
	at := time.Date(2026, time.July, 28, 12, 0, 0, 0, time.UTC)

	sign := func(method, key string, body []byte) *http.Request {
		t.Helper()
		url, err := s.objectURL(key)
		if err != nil {
			t.Fatalf("objectURL: %v", err)
		}
		req, err := http.NewRequest(method, url, bytes.NewReader(body))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		if err := s.sign(req, body, at); err != nil {
			t.Fatalf("sign: %v", err)
		}
		return req
	}

	key := "ab/cd/0d1b2f8e-0000-4000-8000-000000000000.bin"
	base := sign(http.MethodPut, key, []byte("ciphertext"))

	auth := base.Header.Get("Authorization")
	for _, want := range []string{
		"AWS4-HMAC-SHA256",
		"Credential=AKIAIOSFODNN7EXAMPLE/20260728/us-east-1/s3/aws4_request",
		"SignedHeaders=host;x-amz-content-sha256;x-amz-date",
		"Signature=",
	} {
		if !strings.Contains(auth, want) {
			t.Errorf("Authorization %q is missing %q", auth, want)
		}
	}
	if got := base.Header.Get("X-Amz-Date"); got != "20260728T120000Z" {
		t.Errorf("X-Amz-Date = %q", got)
	}
	// S3 requires the payload hash as a header as well as in the signature.
	if base.Header.Get("X-Amz-Content-Sha256") == "" {
		t.Error("X-Amz-Content-Sha256 is not set")
	}

	signatureOf := func(r *http.Request) string {
		_, sig, _ := strings.Cut(r.Header.Get("Authorization"), "Signature=")
		return sig
	}

	same := sign(http.MethodPut, key, []byte("ciphertext"))
	if signatureOf(base) != signatureOf(same) {
		t.Error("signing the same request twice produced different signatures")
	}

	for _, tc := range []struct {
		name string
		req  *http.Request
	}{
		{"a different method", sign(http.MethodGet, key, nil)},
		{"a different key", sign(http.MethodPut, "ff/ee/0d1b2f8e-0000-4000-8000-000000000000.bin", []byte("ciphertext"))},
		{"different bytes", sign(http.MethodPut, key, []byte("other ciphertext"))},
	} {
		if signatureOf(tc.req) == signatureOf(base) {
			t.Errorf("%s did not change the signature; it is not covered", tc.name)
		}
	}

	// A different moment must produce a different signature, or a captured
	// request would be replayable indefinitely.
	later := sign(http.MethodPut, key, []byte("ciphertext"))
	if err := s.sign(later, []byte("ciphertext"), at.Add(time.Hour)); err != nil {
		t.Fatalf("sign: %v", err)
	}
	if signatureOf(later) == signatureOf(base) {
		t.Error("the timestamp is not covered by the signature")
	}
}

// A round trip against a stub endpoint: the three verbs address the right
// paths, carry the right bytes, and map responses onto the Storage contract.
func TestS3StorageRoundTrip(t *testing.T) {
	key := "ab/cd/0d1b2f8e-0000-4000-8000-000000000000.bin"
	objects := map[string][]byte{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			t.Error("request arrived unsigned")
		}
		switch r.Method {
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			objects[r.URL.Path] = body
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			body, ok := objects[r.URL.Path]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Write(body)
		case http.MethodDelete:
			delete(objects, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	store, err := NewS3Storage(config.S3Config{
		Endpoint: srv.URL, Region: "us-east-1", Bucket: "vault",
		AccessKey: "key", SecretKey: "secret", PathStyle: true,
	})
	if err != nil {
		t.Fatalf("new s3 storage: %v", err)
	}

	ctx := context.Background()
	sealed := []byte("sealed bytes")

	if err := store.Put(ctx, key, sealed); err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, ok := objects["/vault/"+key]; !ok {
		t.Fatalf("object landed at the wrong path; have %v", objects)
	}

	got, err := store.Get(ctx, key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !bytes.Equal(got, sealed) {
		t.Errorf("got %q, want %q", got, sealed)
	}

	if err := store.Delete(ctx, key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.Get(ctx, key); err != ErrNotFound {
		t.Errorf("after delete: err = %v, want ErrNotFound", err)
	}
	// Delete is called during cleanup, so a second one must not be an error.
	if err := store.Delete(ctx, key); err != nil {
		t.Errorf("deleting an absent key: %v", err)
	}
}

func TestS3RefusesIncompleteConfig(t *testing.T) {
	if _, err := NewS3Storage(config.S3Config{Endpoint: "https://s3.example.com"}); err == nil {
		t.Error("NewS3Storage accepted a config with no bucket or credentials")
	}
}
