package config

import "testing"

// validateMerchantLogos is the boot-time guard on a logo fetcher that could
// never fetch anything. Both failures it prevents are silent at runtime: an
// operator sets MERCHANT_LOGOS_ENABLED=true, sees no error, and spends a week
// wondering why every merchant still shows a monogram.
func TestValidateMerchantLogos(t *testing.T) {
	withAI := AIConfig{APIKey: "sk-test"}
	noAI := AIConfig{}

	cases := []struct {
		name    string
		cfg     MerchantLogosConfig
		ai      AIConfig
		wantErr bool
	}{
		{
			name: "off is fine — this is every deployment by default",
			cfg:  MerchantLogosConfig{},
			ai:   noAI,
		},
		{
			// Inert rather than wrong: with the switch off nothing reads the
			// token, so refusing to boot over a leftover value would be officious.
			name: "a token with the switch off is tolerated",
			cfg:  MerchantLogosConfig{Token: "pk_test"},
			ai:   noAI,
		},
		{
			name:    "on with no token is refused",
			cfg:     MerchantLogosConfig{Enabled: true, Size: 128, MaxBytes: 1 << 17},
			ai:      withAI,
			wantErr: true,
		},
		{
			// Logo.dev is keyed by domain, and the AI provider is the only thing
			// that turns "BLUE BOTTLE COFFEE" into one. Without a key the feature
			// has no first step.
			name:    "on with no AI key is refused",
			cfg:     MerchantLogosConfig{Enabled: true, Token: "pk_test", Size: 128, MaxBytes: 1 << 17},
			ai:      noAI,
			wantErr: true,
		},
		{
			name:    "a size above the host's ceiling is refused",
			cfg:     MerchantLogosConfig{Enabled: true, Token: "pk_test", Size: 4096, MaxBytes: 1 << 17},
			ai:      withAI,
			wantErr: true,
		},
		{
			name:    "a non-positive byte cap is refused",
			cfg:     MerchantLogosConfig{Enabled: true, Token: "pk_test", Size: 128},
			ai:      withAI,
			wantErr: true,
		},
		{
			name: "fully configured",
			cfg:  MerchantLogosConfig{Enabled: true, Token: "pk_test", Size: 128, MaxBytes: 1 << 17},
			ai:   withAI,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateMerchantLogos(tc.cfg, tc.ai)
			if tc.wantErr && err == nil {
				t.Fatal("expected an error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// Ready is what every caller branches on, so it must fold in the AI dependency
// rather than leaving each site to remember it.
func TestMerchantLogosReady(t *testing.T) {
	full := MerchantLogosConfig{Enabled: true, Token: "pk_test", Size: 128, MaxBytes: 1 << 17}
	withAI := AIConfig{APIKey: "sk-test"}

	if !full.Ready(withAI) {
		t.Error("a fully configured fetcher is not ready")
	}
	if full.Ready(AIConfig{}) {
		t.Error("ready without an AI key")
	}
	if (MerchantLogosConfig{Token: "pk_test"}).Ready(withAI) {
		t.Error("ready with the switch off")
	}
	if (MerchantLogosConfig{Enabled: true}).Ready(withAI) {
		t.Error("ready without a token")
	}
}
