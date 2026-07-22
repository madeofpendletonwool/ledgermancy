package crypto

import (
	"bytes"
	"crypto/rand"
	"errors"
	"testing"
)

func newTestCipher(t *testing.T) (*Cipher, []byte) {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("generate key: %v", err)
	}
	c, err := New(key)
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	return c, key
}

func TestSealOpenRoundTrip(t *testing.T) {
	c, _ := newTestCipher(t)

	// Shaped like a real Plaid access token.
	const token = "access-sandbox-8ab97615-1f2e-4d3c-9f0a-2b7c5e1d4a6f"

	sealed, err := c.SealString(token)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if bytes.Contains(sealed, []byte(token)) {
		t.Fatal("plaintext token is visible in the ciphertext")
	}

	got, err := c.OpenString(sealed)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got != token {
		t.Errorf("round trip changed the value: got %q, want %q", got, token)
	}
}

// Encrypting the same value twice must produce different ciphertexts, or the
// nonce is being reused — which is catastrophic for GCM.
func TestSealUsesFreshNonce(t *testing.T) {
	c, _ := newTestCipher(t)

	a, err := c.SealString("same-token")
	if err != nil {
		t.Fatalf("seal a: %v", err)
	}
	b, err := c.SealString("same-token")
	if err != nil {
		t.Fatalf("seal b: %v", err)
	}
	if bytes.Equal(a, b) {
		t.Error("identical plaintexts produced identical ciphertexts; nonce is being reused")
	}
}

func TestOpenRejectsTamperedCiphertext(t *testing.T) {
	c, _ := newTestCipher(t)

	sealed, err := c.SealString("access-sandbox-token")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	tampered := bytes.Clone(sealed)
	tampered[len(tampered)-1] ^= 0xFF // flip bits in the auth tag

	if _, err := c.Open(tampered); !errors.Is(err, ErrInvalidCiphertext) {
		t.Errorf("tampered ciphertext: got %v, want ErrInvalidCiphertext", err)
	}
}

func TestOpenRejectsWrongKey(t *testing.T) {
	c, _ := newTestCipher(t)
	other, _ := newTestCipher(t)

	sealed, err := c.SealString("access-sandbox-token")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := other.Open(sealed); !errors.Is(err, ErrInvalidCiphertext) {
		t.Errorf("wrong key: got %v, want ErrInvalidCiphertext", err)
	}
}

func TestOpenRejectsTruncatedInput(t *testing.T) {
	c, _ := newTestCipher(t)

	for _, input := range [][]byte{nil, {}, {0x01, 0x02, 0x03}} {
		if _, err := c.Open(input); !errors.Is(err, ErrInvalidCiphertext) {
			t.Errorf("input %v: got %v, want ErrInvalidCiphertext", input, err)
		}
	}
}

func TestNewRejectsWrongKeySize(t *testing.T) {
	for _, size := range []int{0, 16, 31, 33, 64} {
		if _, err := New(make([]byte, size)); err == nil {
			t.Errorf("key size %d: expected an error, got nil", size)
		}
	}
}
