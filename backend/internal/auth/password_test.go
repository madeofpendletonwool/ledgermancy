package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestHashPasswordRoundTrip(t *testing.T) {
	const password = "correct horse battery staple"

	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Errorf("expected an argon2id PHC string, got %q", hash)
	}
	if strings.Contains(hash, password) {
		t.Fatal("hash contains the plaintext password")
	}

	if err := VerifyPassword(password, hash); err != nil {
		t.Errorf("correct password rejected: %v", err)
	}
	if err := VerifyPassword("wrong password", hash); !errors.Is(err, ErrMismatchedPassword) {
		t.Errorf("wrong password: got %v, want ErrMismatchedPassword", err)
	}
}

// Equal passwords must still produce different hashes, or the salt is not
// doing its job and identical passwords become linkable across accounts.
func TestHashPasswordIsSalted(t *testing.T) {
	a, err := HashPassword("same-password")
	if err != nil {
		t.Fatalf("hash a: %v", err)
	}
	b, err := HashPassword("same-password")
	if err != nil {
		t.Fatalf("hash b: %v", err)
	}
	if a == b {
		t.Error("identical passwords produced identical hashes; salt is not random")
	}
}

func TestVerifyPasswordRejectsMalformedHashes(t *testing.T) {
	cases := map[string]string{
		"empty":             "",
		"not a phc string":  "hunter2",
		"wrong algorithm":   "$bcrypt$v=19$m=65536,t=3,p=2$c2FsdA$aGFzaA",
		"missing sections":  "$argon2id$v=19$m=65536,t=3,p=2",
		"bad base64 salt":   "$argon2id$v=19$m=65536,t=3,p=2$!!!!$aGFzaA",
		"unparseable param": "$argon2id$v=19$m=abc,t=3,p=2$c2FsdA$aGFzaA",
	}

	for name, hash := range cases {
		t.Run(name, func(t *testing.T) {
			err := VerifyPassword("anything", hash)
			if err == nil {
				t.Fatal("expected an error for a malformed hash, got nil")
			}
			// A malformed stored hash is a data problem, not a wrong password;
			// conflating them would hide corruption behind a login failure.
			if errors.Is(err, ErrMismatchedPassword) {
				t.Error("malformed hash reported as a password mismatch")
			}
		})
	}
}

// Parameters are embedded in each hash, so raising the cost later must not
// lock out users whose passwords were hashed with the old settings.
func TestVerifyPasswordAcceptsOtherCostParameters(t *testing.T) {
	original := defaultParams
	t.Cleanup(func() { defaultParams = original })

	defaultParams = argon2Params{
		memoryKiB: 16 * 1024, iterations: 1, parallelism: 1,
		saltLength: 16, keyLength: 32,
	}
	weak, err := HashPassword("legacy-password")
	if err != nil {
		t.Fatalf("hash with old params: %v", err)
	}

	defaultParams = original
	if err := VerifyPassword("legacy-password", weak); err != nil {
		t.Errorf("hash made with older parameters no longer verifies: %v", err)
	}
}
