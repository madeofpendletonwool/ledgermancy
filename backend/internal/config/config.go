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
	// tokens at rest. Decoded from the base64 ENCRYPTION_KEY value.
	EncryptionKey []byte
	SessionSecret []byte

	Plaid PlaidConfig
	AI    AIConfig
}

// PlaidConfig holds Plaid API credentials and the set of enabled products.
type PlaidConfig struct {
	Env        string
	ClientID   string
	Secret     string
	Products   []string
	WebhookURL string
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
			Env:        env("PLAID_ENV", "sandbox"),
			ClientID:   os.Getenv("PLAID_CLIENT_ID"),
			Secret:     os.Getenv("PLAID_SECRET"),
			Products:   splitList(env("PLAID_PRODUCTS", "transactions")),
			WebhookURL: os.Getenv("PLAID_WEBHOOK_URL"),
		},
		AI: AIConfig{
			BaseURL: env("AI_BASE_URL", "https://api.anthropic.com"),
			APIKey:  os.Getenv("AI_API_KEY"),
			Model:   env("AI_MODEL", "glm-4.6"),
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

	return cfg, nil
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
