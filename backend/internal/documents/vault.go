package documents

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/config"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/crypto"
)

// Errors the api layer maps onto status codes. Everything else is a 500.
var (
	// ErrTooLarge means the upload exceeded MaxFileBytes. It is raised before
	// the whole file is in memory, which is the point of the limit.
	ErrTooLarge = errors.New("file is larger than the configured limit")
	// ErrEmpty means the upload had no bytes. A zero-byte document is always a
	// mistake — a failed form submission or a dropped folder.
	ErrEmpty = errors.New("file is empty")
	// ErrCorrupt means the blob failed authentication or its plaintext hash did
	// not match what was recorded. Reads fail closed on this: no partial output
	// is ever written, because a half-decrypted tax return that renders is worse
	// than an error.
	ErrCorrupt = errors.New("document could not be decrypted or failed its integrity check")
)

// Vault seals documents into a Storage and opens them again.
//
// It holds the cipher, so nothing else in the app needs to know that documents
// are encrypted at all: callers hand it plaintext and get back a storage key,
// or hand it a storage key and get back plaintext or an error.
type Vault struct {
	store  Storage
	cipher *crypto.Cipher
	cfg    config.DocumentsConfig
}

// New builds a Vault. The cipher is the same one that protects Plaid access
// tokens — which is the whole design, and also the reason DEPLOYING.md now has
// to say that losing ENCRYPTION_KEY loses the vault too.
func New(cfg config.DocumentsConfig, cipher *crypto.Cipher, store Storage) *Vault {
	return &Vault{store: store, cipher: cipher, cfg: cfg}
}

// MaxFileBytes is the per-file cap, exposed so handlers can reject early and
// the UI can say the limit rather than making the user discover it.
func (v *Vault) MaxFileBytes() int64 { return v.cfg.MaxFileBytes }

// QuotaBytes is the per-household ceiling; zero means unlimited.
func (v *Vault) QuotaBytes() int64 { return v.cfg.QuotaBytes }

// OCREnabled reports whether receipt extraction may be offered.
func (v *Vault) OCREnabled() bool { return v.cfg.OCREnabled }

// Backend names the storage backend for the storage panel and startup logs.
func (v *Vault) Backend() string { return v.store.Describe() }

// Stored is the result of a successful Store: everything the metadata row needs
// to point back at the bytes.
type Stored struct {
	StorageKey string
	// ContentHash is SHA-256 of the PLAINTEXT, hex-encoded. Plaintext and not
	// ciphertext on purpose: GCM uses a fresh nonce per seal, so the same file
	// stored twice has two different ciphertexts and hashing those would make
	// dedupe and integrity checks both useless.
	ContentHash string
	SizeBytes   int64
}

// ReadLimited reads at most MaxFileBytes+1 bytes and refuses anything larger,
// so an oversized upload is rejected without ever being fully buffered.
//
// The +1 is what distinguishes "exactly at the limit" from "over it" — reading
// exactly the limit cannot tell the two apart.
func (v *Vault) ReadLimited(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, v.cfg.MaxFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read upload: %w", err)
	}
	if int64(len(data)) > v.cfg.MaxFileBytes {
		return nil, ErrTooLarge
	}
	if len(data) == 0 {
		return nil, ErrEmpty
	}
	return data, nil
}

// Store seals plaintext and writes it to the backend.
//
// It does not write a metadata row — the caller does that, and does it *after*
// this returns, so a failed upload never leaves a row pointing at nothing. The
// opposite ordering trades a broken download for a wasted byte, which is the
// wrong trade.
func (v *Vault) Store(ctx context.Context, plaintext []byte) (Stored, error) {
	if len(plaintext) == 0 {
		return Stored{}, ErrEmpty
	}
	if int64(len(plaintext)) > v.cfg.MaxFileBytes {
		return Stored{}, ErrTooLarge
	}

	sealed, err := v.cipher.Seal(plaintext)
	if err != nil {
		return Stored{}, fmt.Errorf("seal document: %w", err)
	}

	key := NewStorageKey()
	if err := v.store.Put(ctx, key, sealed); err != nil {
		return Stored{}, err
	}

	sum := sha256.Sum256(plaintext)
	return Stored{
		StorageKey:  key,
		ContentHash: hex.EncodeToString(sum[:]),
		SizeBytes:   int64(len(plaintext)),
	}, nil
}

// Fetch reads, decrypts and verifies a document.
//
// wantHash is checked against the decrypted bytes. GCM authentication already
// proves the blob was not tampered with, so this catches a different failure:
// a storage-layer mixup that serves an intact blob belonging to a *different*
// document. Under encryption that would otherwise be undetectable — the wrong
// file would decrypt perfectly and be served with the right filename.
func (v *Vault) Fetch(ctx context.Context, key, wantHash string) ([]byte, error) {
	sealed, err := v.store.Get(ctx, key)
	if err != nil {
		return nil, err
	}

	plaintext, err := v.cipher.Open(sealed)
	if err != nil {
		slog.Error("document failed to decrypt",
			"storage_key", key, "backend", v.store.Describe(), "error", err)
		return nil, ErrCorrupt
	}

	sum := sha256.Sum256(plaintext)
	if got := hex.EncodeToString(sum[:]); got != wantHash {
		slog.Error("document content hash mismatch; refusing to serve",
			"storage_key", key, "expected", wantHash, "got", got)
		return nil, ErrCorrupt
	}
	return plaintext, nil
}

// Remove deletes the blob. Failures are the caller's to log, not to surface: by
// the time this runs the metadata row is already gone, so the user's delete
// succeeded and what is left is an orphaned blob for an operator to sweep.
func (v *Vault) Remove(ctx context.Context, key string) error {
	return v.store.Delete(ctx, key)
}
