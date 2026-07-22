package auth

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"fmt"
	"image/png"
	"strings"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

// TOTP parameters. SHA1 / 6 digits / 30 seconds is not a compromise made for
// convenience — it is the only combination every authenticator app handles
// reliably, and Google Authenticator in particular silently ignores the
// algorithm and digit parameters in a provisioning URI. SHA1's collision
// weakness is irrelevant here: HMAC-SHA1 is unaffected by it.
const (
	totpPeriod    = 30
	totpDigits    = otp.DigitsSix
	totpAlgorithm = otp.AlgorithmSHA1

	// totpSkew is how many periods either side of now are accepted, covering
	// ordinary clock drift between the phone and the server. One step each way
	// gives a 90-second window; anything wider is a meaningful gift to someone
	// who has observed a code.
	totpSkew = 1
)

// GenerateTOTPSecret creates a new enrolment key for a user.
//
// issuer is what the authenticator app displays as the account's provider, and
// account identifies which login it belongs to — both are visible to the user
// in their app, so they are the only way to tell two entries apart.
func GenerateTOTPSecret(issuer, account string) (*otp.Key, error) {
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      issuer,
		AccountName: account,
		Period:      totpPeriod,
		Digits:      totpDigits,
		Algorithm:   totpAlgorithm,
		// 20 bytes is the RFC 4226 recommendation and the size every app
		// expects. Longer secrets are legal but break some scanners.
		SecretSize: 20,
	})
	if err != nil {
		return nil, fmt.Errorf("generate totp secret: %w", err)
	}
	return key, nil
}

// TOTPQRDataURI renders the key's provisioning URI as a PNG data URI.
//
// Rendering server-side rather than shipping a QR library to the browser keeps
// the Content-Security-Policy tight: the page needs `img-src 'self' data:` and
// nothing else. The secret is already in the response body either way.
func TOTPQRDataURI(key *otp.Key) (string, error) {
	img, err := key.Image(240, 240)
	if err != nil {
		return "", fmt.Errorf("render totp qr: %w", err)
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return "", fmt.Errorf("encode totp qr: %w", err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// ValidateTOTP checks code against secret and reports which time step matched.
//
// The step matters: TOTPSkew means a code stays valid for 90 seconds, so
// without recording the step that was used, a code observed once could be
// replayed for the rest of its window. Callers must persist the returned step
// and refuse anything at or below it.
//
// pquerna/otp's own totp.Validate cannot be used for this — it reports only a
// boolean and discards which step matched — so the candidate steps are walked
// here. Comparison is constant time, matching VerifyPassword.
func ValidateTOTP(secret, code string, now time.Time) (step int64, ok bool) {
	code = strings.TrimSpace(code)
	if len(code) != 6 {
		return 0, false
	}

	current := now.Unix() / totpPeriod

	// Walk oldest first so a code valid at more than one step (impossible in
	// practice, but not worth relying on) resolves to the earliest match.
	for offset := int64(-totpSkew); offset <= totpSkew; offset++ {
		candidate := current + offset

		want, err := totp.GenerateCodeCustom(secret, time.Unix(candidate*totpPeriod, 0), totp.ValidateOpts{
			Period:    totpPeriod,
			Digits:    totpDigits,
			Algorithm: totpAlgorithm,
		})
		if err != nil {
			// A malformed stored secret fails every step; there is nothing to
			// distinguish here for the caller.
			return 0, false
		}

		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return candidate, true
		}
	}
	return 0, false
}

// recoveryCodeAlphabet is Crockford base32: no I, L, O or U, so a handwritten
// code cannot be misread as a digit and there are no accidental words.
const recoveryCodeAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// RecoveryCodeCount is how many codes an enrolment issues.
const RecoveryCodeCount = 10

// NewRecoveryCodes returns n formatted single-use recovery codes.
//
// Each is 10 alphabet characters — 50 bits of entropy, which is far beyond
// guessing even without the rate limiting in front of it, and short enough to
// write on paper and stick in a drawer.
func NewRecoveryCodes(n int) ([]string, error) {
	codes := make([]string, 0, n)
	for range n {
		raw := make([]byte, 10)
		if _, err := rand.Read(raw); err != nil {
			return nil, fmt.Errorf("generate recovery code: %w", err)
		}

		out := make([]byte, len(raw))
		for i, b := range raw {
			// len(alphabet) is 32, an exact divisor of 256, so this modulo
			// introduces no bias.
			out[i] = recoveryCodeAlphabet[int(b)%len(recoveryCodeAlphabet)]
		}
		codes = append(codes, fmt.Sprintf("%s-%s", out[:5], out[5:]))
	}
	return codes, nil
}

// NormalizeRecoveryCode makes lookup forgiving of how the code was typed:
// case, whitespace, and the cosmetic hyphen are all discarded before hashing,
// so "abcde-fghjk" and "ABCDEFGHJK" are the same code.
func NormalizeRecoveryCode(code string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(code) {
		if strings.ContainsRune(recoveryCodeAlphabet, r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// DecodeTOTPSecret validates that a stored secret is usable base32. It exists
// so a corrupt secret is caught at read time with a clear error rather than
// silently failing every code the user enters.
func DecodeTOTPSecret(secret string) error {
	if _, err := base32.StdEncoding.WithPadding(base32.NoPadding).
		DecodeString(strings.ToUpper(strings.TrimSpace(secret))); err != nil {
		return fmt.Errorf("stored totp secret is not valid base32: %w", err)
	}
	return nil
}
