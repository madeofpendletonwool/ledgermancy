// Package crypto encrypts secrets that must be stored but never exposed —
// principally Plaid access tokens, which grant read access to a user's bank
// accounts and are therefore never written to the database in the clear.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

// ErrInvalidCiphertext is returned when a value cannot be decrypted: either it
// is corrupt, was truncated, or was encrypted under a different key.
var ErrInvalidCiphertext = errors.New("ciphertext is invalid or was encrypted with a different key")

// Cipher seals and opens values with AES-256-GCM.
type Cipher struct {
	aead cipher.AEAD
}

// New builds a Cipher from a 32-byte key (the decoded ENCRYPTION_KEY).
func New(key []byte) (*Cipher, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("encryption key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create gcm: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// Seal encrypts plaintext, returning nonce||ciphertext.
//
// GCM is authenticated, so any later tampering with the stored bytes is
// detected at Open time rather than silently yielding garbage.
func (c *Cipher) Seal(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	// Prepending the nonce to the output keeps the stored value self-contained.
	return c.aead.Seal(nonce, nonce, plaintext, nil), nil
}

// Open reverses Seal.
func (c *Cipher) Open(sealed []byte) ([]byte, error) {
	nonceSize := c.aead.NonceSize()
	if len(sealed) < nonceSize {
		return nil, ErrInvalidCiphertext
	}
	nonce, ciphertext := sealed[:nonceSize], sealed[nonceSize:]

	plaintext, err := c.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		// Deliberately opaque: the caller cannot act on the distinction
		// between a wrong key and a corrupt value, and the detail is a useful
		// oracle to an attacker.
		return nil, ErrInvalidCiphertext
	}
	return plaintext, nil
}

// SealString and OpenString are string conveniences for token values.
func (c *Cipher) SealString(s string) ([]byte, error) { return c.Seal([]byte(s)) }

func (c *Cipher) OpenString(sealed []byte) (string, error) {
	plaintext, err := c.Open(sealed)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}
