package documents

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/config"
)

// S3Storage puts ciphertext in any S3-compatible bucket — AWS, MinIO, Garage,
// Cloudflare R2 — for operators who want documents off the host that serves
// them.
//
// Requests are signed with SigV4 here rather than through an SDK. The vault
// needs exactly three verbs against one bucket, all with an in-memory body, and
// aws-sdk-go-v2 would add a large transitive tree to a self-hosted finance app
// that currently has thirteen direct dependencies. The signing algorithm below
// is the whole of what an SDK would do for these three calls.
//
// A signer that is subtly wrong fails as an opaque 403, so s3_test.go pins the
// parts that can be checked without a live endpoint: the canonical URI encoding
// rules, the header set, determinism, and that every input the signature is
// supposed to cover actually changes it.
type S3Storage struct {
	cfg    config.S3Config
	client *http.Client
}

// s3Timeout bounds one object request. Generous for a file capped at tens of
// megabytes, tight enough that a black-holed endpoint fails the upload rather
// than holding the request until the api's own 30s timeout kills it.
const s3Timeout = 60 * time.Second

func NewS3Storage(cfg config.S3Config) (*S3Storage, error) {
	if !cfg.Configured() {
		return nil, errors.New("s3 document backend is missing endpoint, bucket or credentials")
	}
	if _, err := url.Parse(cfg.Endpoint); err != nil {
		return nil, fmt.Errorf("invalid DOCUMENTS_S3_ENDPOINT: %w", err)
	}
	return &S3Storage{cfg: cfg, client: &http.Client{Timeout: s3Timeout}}, nil
}

func (s *S3Storage) Describe() string {
	return fmt.Sprintf("s3:%s/%s", s.cfg.Endpoint, s.cfg.Bucket)
}

// objectPath builds the path portion of the request, honouring the prefix and
// the path-style/virtual-host choice.
func (s *S3Storage) objectPath(key string) string {
	full := key
	if s.cfg.Prefix != "" {
		full = s.cfg.Prefix + "/" + key
	}
	if s.cfg.PathStyle {
		return "/" + s.cfg.Bucket + "/" + full
	}
	return "/" + full
}

func (s *S3Storage) objectURL(key string) (string, error) {
	base, err := url.Parse(s.cfg.Endpoint)
	if err != nil {
		return "", fmt.Errorf("invalid s3 endpoint: %w", err)
	}
	if !s.cfg.PathStyle {
		base.Host = s.cfg.Bucket + "." + base.Host
	}
	base.Path = s.objectPath(key)
	return base.String(), nil
}

func (s *S3Storage) Put(ctx context.Context, key string, sealed []byte) error {
	if err := validateKey(key); err != nil {
		return err
	}
	res, err := s.do(ctx, http.MethodPut, key, sealed)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return s3Error("put", res)
	}
	return nil
}

func (s *S3Storage) Get(ctx context.Context, key string) ([]byte, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	res, err := s.do(ctx, http.MethodGet, key, nil)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if res.StatusCode/100 != 2 {
		return nil, s3Error("get", res)
	}
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("read s3 object: %w", err)
	}
	return data, nil
}

func (s *S3Storage) Delete(ctx context.Context, key string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	res, err := s.do(ctx, http.MethodDelete, key, nil)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	// S3 reports a delete of an absent key as success, which is what we want:
	// Delete is called during cleanup and must be idempotent.
	if res.StatusCode/100 != 2 && res.StatusCode != http.StatusNotFound {
		return s3Error("delete", res)
	}
	return nil
}

func (s *S3Storage) do(ctx context.Context, method, key string, body []byte) (*http.Response, error) {
	endpoint, err := s.objectURL(key)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build s3 request: %w", err)
	}
	req.ContentLength = int64(len(body))
	if method == http.MethodPut {
		// Deliberately not the document's own type. What S3 holds is AES-GCM
		// output, and labelling it as the plaintext's type would be a lie that
		// leaks what the file is to anyone with bucket listing.
		req.Header.Set("Content-Type", "application/octet-stream")
	}

	if err := s.sign(req, body, time.Now().UTC()); err != nil {
		return nil, err
	}

	res, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("s3 %s: %w", strings.ToLower(method), err)
	}
	return res, nil
}

// s3Error turns a non-2xx into an error carrying enough of the body to be
// diagnosable (S3 explains itself in XML) without dumping an unbounded response
// into the logs.
func s3Error(op string, res *http.Response) error {
	snippet, _ := io.ReadAll(io.LimitReader(res.Body, 512))
	return fmt.Errorf("s3 %s failed: %s: %s", op, res.Status, strings.TrimSpace(string(snippet)))
}

// --------------------------------------------------------------------------
// SigV4
// --------------------------------------------------------------------------

const (
	sigV4Algorithm = "AWS4-HMAC-SHA256"
	amzDateFormat  = "20060102T150405Z"
	credDateFormat = "20060102"
)

// sign adds the SigV4 headers to req in place.
//
// S3 requires the payload hash in a header (x-amz-content-sha256) as well as in
// the signature, which is why the body is passed in rather than read back off
// the request.
func (s *S3Storage) sign(req *http.Request, body []byte, now time.Time) error {
	payloadHash := hex.EncodeToString(sha256sum(body))
	amzDate := now.Format(amzDateFormat)
	dateStamp := now.Format(credDateFormat)

	req.Header.Set("Host", req.URL.Host)
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	// Signed headers are the minimum that authenticates the request: the host it
	// is aimed at, when it was made, and what it carries. Signing more would
	// mean any proxy that adds a header breaks the signature.
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalHeaders := strings.Join([]string{
		"host:" + req.URL.Host,
		"x-amz-content-sha256:" + payloadHash,
		"x-amz-date:" + amzDate,
	}, "\n") + "\n"

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI(req.URL.Path),
		req.URL.RawQuery,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := strings.Join([]string{dateStamp, s.cfg.Region, "s3", "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		sigV4Algorithm,
		amzDate,
		scope,
		hex.EncodeToString(sha256sum([]byte(canonicalRequest))),
	}, "\n")

	key := hmacSHA256([]byte("AWS4"+s.cfg.SecretKey), dateStamp)
	key = hmacSHA256(key, s.cfg.Region)
	key = hmacSHA256(key, "s3")
	key = hmacSHA256(key, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, stringToSign))

	req.Header.Set("Authorization", fmt.Sprintf(
		"%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		sigV4Algorithm, s.cfg.AccessKey, scope, signedHeaders, signature,
	))
	return nil
}

// canonicalURI percent-encodes each path segment, leaving the separators alone.
// Our keys are hex and hyphens so nothing here ever needs escaping in practice,
// but a signer that only works for the inputs it expects is the kind that
// breaks the day a prefix contains a space.
func canonicalURI(path string) string {
	if path == "" {
		return "/"
	}
	segments := strings.Split(path, "/")
	for i, seg := range segments {
		segments[i] = awsURIEncode(seg)
	}
	return strings.Join(segments, "/")
}

// awsURIEncode implements the encoding SigV4 specifies, which is RFC 3986
// unreserved characters only — notably stricter than url.PathEscape, which
// leaves sub-delimiters such as '$' and '+' alone and would produce a canonical
// request AWS does not agree with.
func awsURIEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func sha256sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}
