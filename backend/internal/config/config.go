// Package config loads and validates runtime configuration from the environment.
package config

import (
	"encoding/base64"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the fully-resolved application configuration.
type Config struct {
	AppEnv         string
	HTTPAddr       string
	FrontendOrigin string
	DatabaseURL    string

	// TrustProxyHeaders declares that a reverse proxy sits in front of this
	// process and overwrites X-Forwarded-For / X-Forwarded-Proto on the way in.
	//
	// It defaults to false, and that default is load-bearing. When it is set,
	// the api derives the client's IP and scheme from request headers — so if
	// nothing in front is actually sanitising them, any client can choose its
	// own apparent IP address and defeat every IP-based rate limit, and can
	// claim a plain-HTTP request arrived over TLS. Only enable it when
	// something trustworthy really is in front.
	TrustProxyHeaders bool

	// EncryptionKey is a 32-byte AES-GCM key used to encrypt Plaid access
	// tokens and every document in the vault at rest. Decoded from the base64
	// ENCRYPTION_KEY value.
	//
	// The vault raised the stakes on this key considerably: a lost key costs a
	// relink for the tokens, and costs the documents outright.
	EncryptionKey []byte
	SessionSecret []byte

	Plaid      PlaidConfig
	AI         AIConfig
	NTFY       NTFYConfig
	Benchmarks BenchmarkConfig
	Retirement RetirementConfig
	Documents  DocumentsConfig
}

// DocumentsConfig configures the encrypted document vault: where ciphertext
// lands, how big one file may be, and how much a household may store.
//
// The two size limits are not tuning knobs, they are load-bearing. crypto.Seal
// and crypto.Open are whole-buffer operations, so a file becomes its own size in
// Go heap for the duration of a request — twice over, briefly, since plaintext
// and ciphertext coexist. MaxFileBytes is what stops that becoming an OOM under
// concurrent uploads, and it is enforced before a byte is read into memory.
// Raising it far past the default is not a configuration change so much as a
// request for the chunked-streaming redesign this deliberately does not do.
type DocumentsConfig struct {
	// Enabled turns the vault off entirely. It defaults on: local storage needs
	// nothing but a writable directory, and a vault nobody can reach is not a
	// safer default, just a broken one.
	Enabled bool

	// Backend is "local" (a mounted volume, the Compose deploy model) or "s3"
	// (any S3-compatible endpoint, for off-host durability).
	Backend string

	// LocalRoot is the directory ciphertext is written under, for Backend
	// "local". It must be a volume: it is NOT in pg_dump, so a backup plan that
	// only dumps the database silently omits every document.
	LocalRoot string

	S3 S3Config

	MaxFileBytes int64
	// QuotaBytes caps one household's total stored size. Counted across the
	// whole household including private uploads, because the thing being
	// rationed is the operator's disk.
	QuotaBytes int64

	// OCREnabled allows receipt field extraction via the configured AI provider.
	// Off by default and separate from AI_API_KEY on purpose: this is the one
	// place the app would send an image of a user's paperwork off the host, and
	// that deserves its own deliberate switch rather than riding along with
	// having configured a model at all.
	OCREnabled bool
}

// S3Config points at any S3-compatible endpoint (AWS, MinIO, Garage, R2).
// Requests are signed with SigV4 by hand rather than via an SDK — the vault
// needs three verbs, and this keeps a large dependency tree out of a
// self-hosted finance app.
type S3Config struct {
	Endpoint  string // e.g. https://s3.us-east-1.amazonaws.com, or a MinIO host
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
	// Prefix scopes every key, so one bucket can hold more than this app.
	Prefix string
	// PathStyle addresses the bucket as {endpoint}/{bucket}/{key} rather than
	// as a subdomain. Defaults true because that is what self-hosted
	// S3-compatible servers universally support.
	PathStyle bool
}

// Configured reports whether the S3 backend has everything it needs. An
// half-configured S3 backend must fail loudly at startup rather than accept an
// upload and lose it.
func (s S3Config) Configured() bool {
	return s.Endpoint != "" && s.Bucket != "" && s.AccessKey != "" && s.SecretKey != ""
}

// RetirementConfig gates the withdrawal-phase simulation on the Retirement page.
//
// Off by default, and for a different reason than Benchmarks: this one makes no
// outbound call at all. The reason is that a survival percentage is the most
// quotable number a retirement tool can print, and Ledgermancy bundles no
// historical return series to compute one from — the sequences are drawn around
// the user's own stated assumptions, which is a real and useful thing to show
// but is not a backtest. Every projection figure works without it; an operator
// turns it on knowing what it is and is not.
type RetirementConfig struct {
	MonteCarloEnabled bool
}

// BenchmarkConfig controls the daily end-of-day price fetch that backs the
// Investments page's benchmark comparison.
//
// Off by default, and that default is the point. With every optional switch
// left alone, the app talks to nothing but Plaid and the configured AI
// provider, and the README promises exactly that. Turning this on adds a third
// host (Stooq); the other two switches that can add one are DocumentsConfig's
// OCREnabled and its S3 backend. Each is a decision the operator makes
// knowingly, which is why none of them defaults on.
type BenchmarkConfig struct {
	Enabled bool
	Tickers []string
}

// PlaidConfig holds Plaid API credentials and the set of enabled products.
type PlaidConfig struct {
	Env      string
	ClientID string
	Secret   string
	// Products an institution MUST support to appear in Link. Every entry here
	// narrows the institution list, so it stays minimal — transactions only.
	Products []string
	// OptionalProducts are pulled where the institution and accounts support
	// them, and ignored where they don't. Institutions are never filtered on
	// these, and Plaid only bills for one when it actually applies. Investments
	// and Liabilities belong here.
	//
	// This is also the switch that turns the optional sync modules on, including
	// for Items that already exist — no relink required.
	OptionalProducts []string
	WebhookURL       string
}

// AIConfig points at any Anthropic Messages API-compatible endpoint (GLM,
// Claude, or a self-hosted proxy). When APIKey is empty, AI features are
// disabled and the app falls back to deterministic behaviour everywhere.
type AIConfig struct {
	BaseURL string
	APIKey  string
	Model   string
}

// Enabled reports whether AI-backed features should be offered.
func (a AIConfig) Enabled() bool { return a.APIKey != "" }

// NTFYConfig points at an ntfy server (the public https://ntfy.sh by default,
// or a self-hosted instance) used for external push. The base URL is always
// defaulted, so Enabled() is effectively "a server is reachable"; the real
// per-user switch is the notify.channel/notify.ntfy_topic preference, checked
// inside the notifier's Send.
type NTFYConfig struct {
	BaseURL string
	Token   string // optional Bearer for protected/self-hosted topics
}

// Enabled reports whether push delivery will be attempted at all.
func (n NTFYConfig) Enabled() bool { return n.BaseURL != "" }

// IsProduction reports whether the app is running in a production environment,
// which tightens cookie and TLS behaviour.
func (c Config) IsProduction() bool { return c.AppEnv == "production" }

// SessionTTL is how long a login session remains valid.
const SessionTTL = 30 * 24 * time.Hour

// Load reads configuration from the environment, applying defaults and
// validating anything the app cannot safely start without.
func Load() (Config, error) {
	cfg := Config{
		AppEnv:            env("APP_ENV", "development"),
		HTTPAddr:          env("HTTP_ADDR", ":8080"),
		FrontendOrigin:    env("FRONTEND_ORIGIN", "http://localhost:5173"),
		DatabaseURL:       os.Getenv("DATABASE_URL"),
		TrustProxyHeaders: envBool("TRUST_PROXY_HEADERS", false),
		Plaid: PlaidConfig{
			Env:      env("PLAID_ENV", "sandbox"),
			ClientID: os.Getenv("PLAID_CLIENT_ID"),
			Secret:   os.Getenv("PLAID_SECRET"),
			Products: splitList(env("PLAID_PRODUCTS", "transactions")),
			// Defaulted on: the Investments page and debt-payoff goals are
			// built into the app, and leaving them dark by default is how they
			// came to be silently empty in the first place. Nothing is billed
			// for an institution or account that doesn't carry them. Set the
			// variable empty to opt out.
			OptionalProducts: splitList(env("PLAID_OPTIONAL_PRODUCTS", "investments,liabilities")),
			WebhookURL:       os.Getenv("PLAID_WEBHOOK_URL"),
		},
		AI: AIConfig{
			BaseURL: env("AI_BASE_URL", "https://api.anthropic.com"),
			APIKey:  os.Getenv("AI_API_KEY"),
			Model:   env("AI_MODEL", "glm-4.6"),
		},
		NTFY: NTFYConfig{
			BaseURL: env("NTFY_BASE_URL", "https://ntfy.sh"),
			Token:   os.Getenv("NTFY_TOKEN"),
		},
		Benchmarks: BenchmarkConfig{
			Enabled: envBool("BENCHMARK_PRICES_ENABLED", false),
			Tickers: splitList(env("BENCHMARK_TICKERS", "SPY,VTI,BND,QQQ")),
		},
		Retirement: RetirementConfig{
			MonteCarloEnabled: envBool("RETIREMENT_MONTE_CARLO_ENABLED", false),
		},
		Documents: DocumentsConfig{
			Enabled:      envBool("DOCUMENTS_ENABLED", true),
			Backend:      strings.ToLower(env("DOCUMENTS_BACKEND", "local")),
			LocalRoot:    env("DOCUMENTS_LOCAL_ROOT", "/var/lib/ledgermancy/documents"),
			MaxFileBytes: envBytes("DOCUMENTS_MAX_FILE_BYTES", 25<<20), // 25 MiB
			QuotaBytes:   envBytes("DOCUMENTS_QUOTA_BYTES", 2<<30),     // 2 GiB
			OCREnabled:   envBool("DOCUMENTS_OCR_ENABLED", false),
			S3: S3Config{
				Endpoint:  strings.TrimRight(os.Getenv("DOCUMENTS_S3_ENDPOINT"), "/"),
				Region:    env("DOCUMENTS_S3_REGION", "us-east-1"),
				Bucket:    os.Getenv("DOCUMENTS_S3_BUCKET"),
				AccessKey: os.Getenv("DOCUMENTS_S3_ACCESS_KEY"),
				SecretKey: os.Getenv("DOCUMENTS_S3_SECRET_KEY"),
				Prefix:    strings.Trim(os.Getenv("DOCUMENTS_S3_PREFIX"), "/"),
				PathStyle: envBool("DOCUMENTS_S3_PATH_STYLE", true),
			},
		},
	}

	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}

	var err error
	if cfg.EncryptionKey, err = decodeKey("ENCRYPTION_KEY"); err != nil {
		return Config{}, err
	}
	if cfg.SessionSecret, err = decodeKey("SESSION_SECRET"); err != nil {
		return Config{}, err
	}

	if err := validateDocuments(cfg.Documents); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// maxDocumentFileCeiling is the highest DOCUMENTS_MAX_FILE_BYTES will accept.
//
// It exists because the failure it prevents does not look like a configuration
// mistake when it happens. Sealing is whole-buffer, so the real cost of a limit
// is roughly twice the limit per in-flight upload; a few concurrent uploads at
// a "generous" 500 MB would take the api process down with an OOM that reads
// like a crash, not like a setting. Anything above this wants streaming
// encryption, which is a redesign rather than a bigger number.
const maxDocumentFileCeiling = 100 << 20 // 100 MiB

func validateDocuments(d DocumentsConfig) error {
	if !d.Enabled {
		return nil
	}
	switch d.Backend {
	case "local":
		if d.LocalRoot == "" {
			return fmt.Errorf("DOCUMENTS_LOCAL_ROOT is required for the local document backend")
		}
	case "s3":
		if !d.S3.Configured() {
			return fmt.Errorf("DOCUMENTS_BACKEND=s3 needs DOCUMENTS_S3_ENDPOINT, _BUCKET, _ACCESS_KEY and _SECRET_KEY")
		}
	default:
		return fmt.Errorf("DOCUMENTS_BACKEND must be \"local\" or \"s3\", got %q", d.Backend)
	}
	if d.MaxFileBytes <= 0 {
		return fmt.Errorf("DOCUMENTS_MAX_FILE_BYTES must be positive")
	}
	if d.MaxFileBytes > maxDocumentFileCeiling {
		return fmt.Errorf(
			"DOCUMENTS_MAX_FILE_BYTES is %d, above the %d ceiling: encryption is whole-buffer, so a larger cap trades a clean rejection for an out-of-memory crash",
			d.MaxFileBytes, int64(maxDocumentFileCeiling))
	}
	if d.QuotaBytes > 0 && d.QuotaBytes < d.MaxFileBytes {
		return fmt.Errorf("DOCUMENTS_QUOTA_BYTES (%d) is below DOCUMENTS_MAX_FILE_BYTES (%d), so no upload could ever succeed",
			d.QuotaBytes, d.MaxFileBytes)
	}
	return nil
}

// decodeKey reads a base64-encoded 32-byte secret from the named variable.
// Both secrets are required: without them we would be storing Plaid access
// tokens in the clear, so we fail fast rather than start up insecurely.
func decodeKey(name string) ([]byte, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return nil, fmt.Errorf("%s is required (generate one with: openssl rand -base64 32)", name)
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("%s must be valid base64: %w", name, err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("%s must decode to 32 bytes, got %d", name, len(key))
	}
	return key, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// envBool parses a boolean setting. Anything unparseable falls back rather
// than failing startup — but note every caller's fallback is the *safe*
// value, so a typo degrades to the restrictive behaviour, never the permissive
// one.
func envBool(key string, fallback bool) bool {
	v, err := strconv.ParseBool(os.Getenv(key))
	if err != nil {
		return fallback
	}
	return v
}

// envBytes parses a byte size, accepting a plain integer or a KB/MB/GB suffix
// (binary multiples — "25MB" is 25 MiB). Like envBool, an unparseable value
// falls back rather than failing startup, and every caller's fallback is the
// conservative one.
func envBytes(key string, fallback int64) int64 {
	raw := strings.TrimSpace(strings.ToUpper(os.Getenv(key)))
	if raw == "" {
		return fallback
	}

	multiplier := int64(1)
	for suffix, m := range map[string]int64{"KB": 1 << 10, "MB": 1 << 20, "GB": 1 << 30} {
		if strings.HasSuffix(raw, suffix) {
			raw, multiplier = strings.TrimSpace(strings.TrimSuffix(raw, suffix)), m
			break
		}
	}

	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return fallback
	}
	return n * multiplier
}

func splitList(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
