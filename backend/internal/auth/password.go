// Package auth handles password hashing, session lifecycle, and the HTTP
// middleware that turns a session cookie into an authenticated user.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// ErrMismatchedPassword is returned when a password does not match its hash.
var ErrMismatchedPassword = errors.New("password does not match")

// argon2Params are the current hashing cost parameters. They follow the OWASP
// argon2id guidance with headroom. Each stored hash records the parameters it
// was created with, so these can be raised later without invalidating existing
// passwords — old hashes still verify, and can be re-hashed on next login.
type argon2Params struct {
	memoryKiB   uint32
	iterations  uint32
	parallelism uint8
	saltLength  uint32
	keyLength   uint32
}

var defaultParams = argon2Params{
	memoryKiB:   64 * 1024, // 64 MiB
	iterations:  3,
	parallelism: 2,
	saltLength:  16,
	keyLength:   32,
}

// HashPassword returns a PHC-formatted argon2id hash:
//
//	$argon2id$v=19$m=65536,t=3,p=2$<salt>$<hash>
func HashPassword(password string) (string, error) {
	p := defaultParams

	salt := make([]byte, p.saltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}

	key := argon2.IDKey([]byte(password), salt, p.iterations, p.memoryKiB, p.parallelism, p.keyLength)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.memoryKiB, p.iterations, p.parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword reports whether password matches encodedHash. It returns
// ErrMismatchedPassword on a wrong password, and a different error if the
// stored hash is malformed.
func VerifyPassword(password, encodedHash string) error {
	p, salt, want, err := decodeHash(encodedHash)
	if err != nil {
		return err
	}

	got := argon2.IDKey([]byte(password), salt, p.iterations, p.memoryKiB, p.parallelism, p.keyLength)

	// Constant time: never leak how much of the hash matched via timing.
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrMismatchedPassword
	}
	return nil
}

// NeedsRehash reports whether encodedHash was produced with weaker parameters
// than defaultParams, and should therefore be replaced.
//
// This is the other half of the promise made above: raising the cost constants
// only protects existing accounts if something actually re-hashes them. Call it
// after a successful VerifyPassword, when the plaintext is in hand — that is
// the only moment a new hash can be computed.
//
// A hash that cannot be decoded returns false: it is corrupt rather than
// merely outdated, and re-hashing is not the fix.
func NeedsRehash(encodedHash string) bool {
	p, _, _, err := decodeHash(encodedHash)
	if err != nil {
		return false
	}
	return p.memoryKiB < defaultParams.memoryKiB ||
		p.iterations < defaultParams.iterations ||
		p.parallelism < defaultParams.parallelism ||
		p.keyLength < defaultParams.keyLength
}

func decodeHash(encoded string) (argon2Params, []byte, []byte, error) {
	var p argon2Params

	parts := strings.Split(encoded, "$")
	if len(parts) != 6 {
		return p, nil, nil, errors.New("malformed password hash")
	}
	if parts[1] != "argon2id" {
		return p, nil, nil, fmt.Errorf("unsupported hash algorithm %q", parts[1])
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return p, nil, nil, fmt.Errorf("parse hash version: %w", err)
	}
	if version != argon2.Version {
		return p, nil, nil, fmt.Errorf("incompatible argon2 version %d", version)
	}

	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d",
		&p.memoryKiB, &p.iterations, &p.parallelism); err != nil {
		return p, nil, nil, fmt.Errorf("parse hash params: %w", err)
	}

	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil {
		return p, nil, nil, fmt.Errorf("decode salt: %w", err)
	}
	key, err := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil {
		return p, nil, nil, fmt.Errorf("decode hash: %w", err)
	}

	p.saltLength = uint32(len(salt))
	p.keyLength = uint32(len(key))
	return p, salt, key, nil
}
