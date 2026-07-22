package auth

import (
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
)

// testSecret is an arbitrary valid base32 secret; the tests care about the
// algorithm's behaviour, not this particular value.
const testSecret = "JBSWY3DPEHPK3PXP"

// codeAt returns the valid code for the period containing t.
func codeAt(t *testing.T, at time.Time) string {
	t.Helper()
	code, err := totp.GenerateCodeCustom(testSecret, at, totp.ValidateOpts{
		Period:    totpPeriod,
		Digits:    totpDigits,
		Algorithm: totpAlgorithm,
	})
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}
	return code
}

func TestValidateTOTPAcceptsCurrentCode(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	step, ok := ValidateTOTP(testSecret, codeAt(t, now), now)
	if !ok {
		t.Fatal("current code was rejected")
	}
	if want := now.Unix() / totpPeriod; step != want {
		t.Errorf("step = %d, want %d", step, want)
	}
}

// The skew window is the whole reason the replay guard is needed, so its
// boundaries are worth pinning down exactly.
func TestValidateTOTPSkewWindow(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	tests := []struct {
		name       string
		offsetSecs int64
		wantOK     bool
	}{
		{"one period behind", -totpPeriod, true},
		{"one period ahead", totpPeriod, true},
		{"two periods behind", -2 * totpPeriod, false},
		{"two periods ahead", 2 * totpPeriod, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code := codeAt(t, now.Add(time.Duration(tc.offsetSecs)*time.Second))

			step, ok := ValidateTOTP(testSecret, code, now)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok {
				want := (now.Unix() + tc.offsetSecs) / totpPeriod
				if step != want {
					t.Errorf("step = %d, want %d", step, want)
				}
			}
		})
	}
}

// A code that matched at step N must never be accepted again. ValidateTOTP
// itself is stateless — it reports the step so the caller can enforce this —
// so what this pins down is that the step it returns is stable and comparable,
// which is what the caller's `step <= lastStep` check depends on.
func TestValidateTOTPStepIsStableForReplayDetection(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	code := codeAt(t, now)

	first, ok := ValidateTOTP(testSecret, code, now)
	if !ok {
		t.Fatal("first use rejected")
	}

	// Same code, a few seconds later but still inside its period.
	second, ok := ValidateTOTP(testSecret, code, now.Add(5*time.Second))
	if !ok {
		t.Fatal("code should still verify inside its own period")
	}

	if first != second {
		t.Fatalf("step changed within one period: %d then %d", first, second)
	}

	// This is the comparison the caller makes; it must catch the reuse.
	if second > first {
		t.Error("replayed code produced a higher step, which would defeat the guard")
	}
}

func TestValidateTOTPRejectsMalformedInput(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	for _, code := range []string{"", "12345", "1234567", "abcdef", "  "} {
		if _, ok := ValidateTOTP(testSecret, code, now); ok {
			t.Errorf("accepted malformed code %q", code)
		}
	}
}

func TestValidateTOTPTolerastesSurroundingWhitespace(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	if _, ok := ValidateTOTP(testSecret, " "+codeAt(t, now)+" ", now); !ok {
		t.Error("a pasted code with whitespace should still verify")
	}
}

func TestGenerateTOTPSecretProducesUsableKey(t *testing.T) {
	key, err := GenerateTOTPSecret("Ledgermancy", "user@example.com")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	if err := DecodeTOTPSecret(key.Secret()); err != nil {
		t.Errorf("generated secret is not valid base32: %v", err)
	}

	// The provisioning URI is what the QR encodes; the app reads the issuer
	// and account from it, so both must survive.
	uri := key.URL()
	if !strings.Contains(uri, "Ledgermancy") || !strings.Contains(uri, "user@example.com") {
		t.Errorf("provisioning URI is missing issuer or account: %s", uri)
	}

	now := time.Now()
	code, err := totp.GenerateCodeCustom(key.Secret(), now, totp.ValidateOpts{
		Period: totpPeriod, Digits: totpDigits, Algorithm: totpAlgorithm,
	})
	if err != nil {
		t.Fatalf("generate code from new secret: %v", err)
	}
	if _, ok := ValidateTOTP(key.Secret(), code, now); !ok {
		t.Error("a freshly generated secret did not validate its own code")
	}
}

func TestTOTPQRDataURI(t *testing.T) {
	key, err := GenerateTOTPSecret("Ledgermancy", "user@example.com")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	uri, err := TOTPQRDataURI(key)
	if err != nil {
		t.Fatalf("render qr: %v", err)
	}

	// The CSP allows `img-src 'self' data:` and nothing else, so anything but
	// an inline PNG data URI would fail to render in the browser.
	if !strings.HasPrefix(uri, "data:image/png;base64,") {
		t.Errorf("QR is not an inline PNG data URI: %.40s", uri)
	}
	if len(uri) < 200 {
		t.Errorf("QR payload is implausibly short (%d bytes)", len(uri))
	}
}

func TestNewRecoveryCodesAreDistinctAndWellFormed(t *testing.T) {
	codes, err := NewRecoveryCodes(RecoveryCodeCount)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(codes) != RecoveryCodeCount {
		t.Fatalf("got %d codes, want %d", len(codes), RecoveryCodeCount)
	}

	seen := make(map[string]bool, len(codes))
	for _, code := range codes {
		if seen[code] {
			t.Fatalf("duplicate recovery code %q", code)
		}
		seen[code] = true

		if len(code) != 11 || code[5] != '-' {
			t.Errorf("code %q is not in xxxxx-xxxxx form", code)
		}
		// Crockford base32 excludes I, L, O and U so a handwritten code
		// cannot be misread. A generator that emitted them would quietly
		// undermine the whole point of the alphabet.
		for _, r := range NormalizeRecoveryCode(code) {
			if !strings.ContainsRune(recoveryCodeAlphabet, r) {
				t.Errorf("code %q contains out-of-alphabet character %q", code, r)
			}
		}
	}
}

// Recovery codes are matched by hash, so anything that changes the normalised
// form changes the hash and would reject a correctly-typed code.
func TestNormalizeRecoveryCode(t *testing.T) {
	codes, err := NewRecoveryCodes(1)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	canonical := NormalizeRecoveryCode(codes[0])

	variants := []string{
		codes[0],
		strings.ToLower(codes[0]),
		strings.ReplaceAll(codes[0], "-", ""),
		"  " + codes[0] + "  ",
		strings.ToLower(strings.ReplaceAll(codes[0], "-", " ")),
	}

	for _, v := range variants {
		if got := NormalizeRecoveryCode(v); got != canonical {
			t.Errorf("NormalizeRecoveryCode(%q) = %q, want %q", v, got, canonical)
		}
	}
}
