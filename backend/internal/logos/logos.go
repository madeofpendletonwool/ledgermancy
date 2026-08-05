// Package logos is the opt-in merchant logo fetcher (MAD-38).
//
// The whole feature is three steps and one rule:
//
//	name → domain     the AI provider, which already sees merchant names
//	domain → bytes    Logo.dev, on its free tier
//	bytes → database  cached per household, served from this app's own origin
//
// The rule is that the browser never talks to Logo.dev. Everything here runs in
// the worker; the api serves cached bytes. That is what keeps "the page loads
// nothing from a third party" true even with the feature switched on, and it is
// why this is a fetch-and-cache package rather than a URL builder.
//
// Off unless the operator sets MERCHANT_LOGOS_ENABLED and the household leaves
// its `merchant.logos` preference on. With either off, no request is made.
package logos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// PreferenceKey is the household-scoped switch that decides whether logos are
// fetched and shown. It defaults ON — see PreferenceDefault.
const PreferenceKey = "merchant.logos"

// PreferenceDefault is what an unset preference means.
//
// True, unlike the other opt-in defaults in this app, and the asymmetry is
// deliberate: the *knowing* opt-in is the operator's MERCHANT_LOGOS_ENABLED,
// which is where the outbound destination is documented and where the decision
// to contact Logo.dev at all is made. Once that is set, requiring a second
// switch would mean an operator who deliberately enabled the feature sees no
// logos and no explanation. This preference exists so a household can turn the
// imagery back off — and, when it does, so the fetcher stops making requests on
// its behalf and the cache is dropped.
const PreferenceDefault = true

// HouseholdEnabled reports whether this household wants logos.
//
// Read by both the api (to answer /capabilities) and the fetch job, because a
// background job must never depend on a handler having run — the same rule the
// preference defaults in the api package carry. Any error degrades to the
// default rather than failing the caller: a missing preference row is the
// normal case, not a fault.
func HouseholdEnabled(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID) bool {
	hh := householdID
	raw, err := q.GetHouseholdPreference(ctx, dbgen.GetHouseholdPreferenceParams{
		HouseholdID: &hh, Key: PreferenceKey,
	})
	if err != nil {
		// pgx.ErrNoRows is the normal case — nobody has touched the setting.
		// Anything else is worth a line but is still not worth failing over.
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Error("read merchant logo preference", "error", err, "household_id", householdID)
		}
		return PreferenceDefault
	}
	var on bool
	if err := json.Unmarshal(raw, &on); err != nil {
		return PreferenceDefault
	}
	return on
}

// --------------------------------------------------------------------------
// Domains
// --------------------------------------------------------------------------

// domainPattern is what a bare registrable domain may look like. Deliberately
// strict ASCII: an internationalised domain reaches us as punycode ("xn--…"),
// which this accepts, and anything else is a model that misunderstood the
// question.
var domainPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

// maxDomainLength is the DNS limit, which doubles as a sanity bound on a string
// that is about to be interpolated into a URL.
const maxDomainLength = 253

// NormaliseDomain reduces a model's answer to a bare domain, or reports that it
// is not one.
//
// This is a validator before it is a tidier. The string it returns is
// interpolated into an outbound URL, so everything that is not unmistakably a
// hostname has to be refused here — a path, a scheme, a port, whitespace, an
// IP address, credentials. The model is being helpful, not hostile, but its
// input is ultimately a merchant name that arrived from a bank feed, and the
// cost of being strict is one merchant showing a monogram.
func NormaliseDomain(raw string) (string, bool) {
	d := strings.ToLower(strings.TrimSpace(raw))
	if d == "" {
		return "", false
	}

	// Tolerate the two shapes a model most often returns anyway, then insist on
	// a bare host from there.
	d = strings.TrimPrefix(strings.TrimPrefix(d, "https://"), "http://")
	d, _, _ = strings.Cut(d, "/")
	d = strings.TrimPrefix(d, "www.")
	d = strings.TrimSuffix(d, ".")

	if d == "" || len(d) > maxDomainLength {
		return "", false
	}
	if !domainPattern.MatchString(d) {
		return "", false
	}
	// A dotted quad matches the pattern above and is never a merchant's website.
	if last := d[strings.LastIndex(d, ".")+1:]; last == "" || last[0] >= '0' && last[0] <= '9' {
		return "", false
	}
	return d, true
}

// --------------------------------------------------------------------------
// Fetching
// --------------------------------------------------------------------------

// logoHost is the only host this package ever contacts. Named as a constant so
// grepping for outbound destinations finds it.
const logoHost = "https://img.logo.dev"

// allowedTypes are the image types worth storing. SVG is absent for the same
// reason it is absent from the document vault's allowlist: it is a
// script-bearing document format wearing an image's clothes, and these bytes
// are served back from this app's own origin.
var allowedTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/webp": true,
	"image/gif":  true,
}

// ErrNoLogo means the host has no logo for this domain. It is an ordinary
// outcome, not a failure: the caller records "none" and stops asking.
var ErrNoLogo = errors.New("logos: no logo for this domain")

// Fetcher pulls one logo from Logo.dev.
type Fetcher struct {
	HTTP  *http.Client
	Token string
	// Size is the square pixel size requested.
	Size int
	// MaxBytes caps what is read and stored. A response above it is treated as
	// no logo rather than as an error: nothing about it will be different next
	// time, so retrying is pointless.
	MaxBytes int64
	// BaseURL overrides logoHost in tests. Empty in production.
	BaseURL string
}

// NewFetcher builds a fetcher with a timeout suited to a single small image.
func NewFetcher(token string, size int, maxBytes int64) *Fetcher {
	return &Fetcher{
		HTTP:     &http.Client{Timeout: 20 * time.Second},
		Token:    token,
		Size:     size,
		MaxBytes: maxBytes,
	}
}

// Fetch returns the logo bytes and the content type sniffed from them.
//
// It returns ErrNoLogo — never a generated placeholder — when the host has
// nothing. That is what `fallback=404` buys: Logo.dev's default is to return a
// black-and-white monogram with a 200, which would mean caching *their* avatar
// forever in place of the app's own, which is prettier, deterministic and
// coloured. The app already has a monogram; it does not want a second one.
func (f *Fetcher) Fetch(ctx context.Context, domain string) ([]byte, string, error) {
	domain, ok := NormaliseDomain(domain)
	if !ok {
		return nil, "", fmt.Errorf("logos: refusing to fetch %q: not a bare domain", domain)
	}

	base := f.BaseURL
	if base == "" {
		base = logoHost
	}
	q := url.Values{
		"token": {f.Token},
		"size":  {fmt.Sprint(f.Size)},
		// PNG keeps transparency, which is what lets a logo sit on the app's own
		// tile rather than in a white box.
		"format": {"png"},
		// Prefer the variant drawn for dark backgrounds; this app has no other
		// kind of background.
		"theme": {"dark"},
		// See the doc comment: a real 404 instead of Logo.dev's own monogram.
		"fallback": {"404"},
	}
	reqURL := fmt.Sprintf("%s/%s?%s", strings.TrimRight(base, "/"), url.PathEscape(domain), q.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("logos: build request: %w", err)
	}
	req.Header.Set("accept", "image/png,image/*")

	resp, err := f.HTTP.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("logos: request failed: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, "", ErrNoLogo
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		// A 401/429/5xx is about us or about them, not about this domain, so it
		// is an error the caller retries rather than a cached "none".
		return nil, "", fmt.Errorf("logos: %s returned %d", base, resp.StatusCode)
	}

	// One byte over the cap is enough to know the body is too big without
	// reading the rest of it.
	body, err := io.ReadAll(io.LimitReader(resp.Body, f.MaxBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("logos: read response: %w", err)
	}
	if int64(len(body)) > f.MaxBytes {
		return nil, "", ErrNoLogo
	}
	if len(body) == 0 {
		return nil, "", ErrNoLogo
	}

	// Sniff the bytes rather than trusting the response header, the same rule
	// the document vault serves downloads by: what we store is what we will
	// later hand a browser on our own origin.
	sniffed, ok := ServedContentType(body)
	if !ok {
		return nil, "", ErrNoLogo
	}
	return body, sniffed, nil
}

// ServedContentType names the bytes for a response, and reports whether they
// are something this app is willing to serve at all.
//
// Stored types were validated on the way in, but every read re-derives from the
// bytes anyway: the cost is a 512-byte sniff, and the alternative is trusting a
// column to still be true after a restore, a migration, or a future writer.
func ServedContentType(image []byte) (string, bool) {
	base, _, _ := strings.Cut(http.DetectContentType(image), ";")
	base = strings.ToLower(strings.TrimSpace(base))
	return base, allowedTypes[base]
}
