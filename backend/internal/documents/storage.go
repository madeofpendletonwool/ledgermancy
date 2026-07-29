// Package documents is the encrypted vault: it owns where a document's bytes
// live, how they are sealed, and the rules about size, retention and safe
// naming. It knows nothing about HTTP or about the metadata rows in Postgres —
// the api layer joins the two.
//
// The security story is short enough to state in one place. Bytes are sealed
// with the same AES-GCM key that protects Plaid access tokens, so the storage
// backend never sees plaintext. The path they are stored at is a generated
// UUID and is never derived from the user's filename, which removes path
// traversal as a class rather than filtering for it. And every read verifies a
// SHA-256 of the plaintext, so a storage-layer mixup that serves the wrong
// intact blob is caught rather than trusted.
package documents

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/config"
)

// ErrNotFound is returned by a Storage when a key has no object. It is
// deliberately distinct from a decryption failure: one means the blob is gone,
// the other means it is present but wrong, and an operator needs to tell them
// apart.
var ErrNotFound = errors.New("document blob not found")

// Storage is the blob layer. Implementations move opaque, already-encrypted
// bytes; none of them has, or should have, access to the cipher.
type Storage interface {
	Put(ctx context.Context, key string, sealed []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
	Delete(ctx context.Context, key string) error
	// Describe names the backend for logs and the storage panel, without
	// leaking credentials.
	Describe() string
}

// storageKeyPattern is the exact shape NewStorageKey produces: two hex shard
// segments and a UUID. Every backend validates against it before touching a
// path, so even a key that somehow arrived from the database malformed cannot
// escape the root.
var storageKeyPattern = regexp.MustCompile(`^[0-9a-f]{2}/[0-9a-f]{2}/[0-9a-f-]{36}\.bin$`)

// NewStorageKey mints the path a document's ciphertext is stored at.
//
// The two-byte shard prefixes exist because a household vault is a directory
// that only grows, and tens of thousands of entries in one directory is a
// filesystem's worst case. They carry no meaning and are not an index.
func NewStorageKey() string {
	id := uuid.New()
	hex := id.String()
	return fmt.Sprintf("%s/%s/%s.bin", hex[0:2], hex[2:4], hex)
}

func validateKey(key string) error {
	if !storageKeyPattern.MatchString(key) {
		return fmt.Errorf("refusing malformed storage key %q", key)
	}
	return nil
}

// NewStorage builds the configured backend. It is called once at startup and
// returns an error rather than degrading: a vault that accepts uploads and
// cannot store them is worse than one that is plainly switched off.
func NewStorage(cfg config.DocumentsConfig) (Storage, error) {
	switch cfg.Backend {
	case "local":
		return NewLocalStorage(cfg.LocalRoot)
	case "s3":
		return NewS3Storage(cfg.S3)
	default:
		return nil, fmt.Errorf("unknown document backend %q", cfg.Backend)
	}
}

// --------------------------------------------------------------------------
// Local filesystem
// --------------------------------------------------------------------------

// LocalStorage writes ciphertext under a directory, which in the Compose deploy
// is a mounted volume.
//
// Worth saying plainly because it is the thing operators get wrong: this
// directory is NOT in pg_dump. A backup that captures only the database
// captures every document's title, type and expiry and none of its contents.
type LocalStorage struct {
	root string
}

func NewLocalStorage(root string) (*LocalStorage, error) {
	if root == "" {
		return nil, errors.New("local document root is empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve document root: %w", err)
	}
	// 0700: the vault holds tax returns and insurance policies. Nothing but this
	// process has business reading it, even on a single-user host.
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("create document root %s: %w", abs, err)
	}

	// Probe for writability at startup rather than discovering it on a user's
	// first upload.
	//
	// The overwhelmingly common cause of a failure here is ownership, not a
	// missing directory: Docker creates a named volume owned by root while this
	// process runs as an unprivileged user, so the message names that explicitly.
	// An operator reading "permission denied" at 1am should not have to work out
	// which uid to chown to.
	probe := filepath.Join(abs, ".writable")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		return nil, fmt.Errorf(
			"document root %s is not writable by uid %d: %w — if this is a Docker volume it is probably owned by root; fix it with `chown -R %d %s` inside the container, or discard the volume and let the image recreate it (docker compose down && docker volume rm <project>_documents-data)",
			abs, os.Getuid(), err, os.Getuid(), abs)
	}
	_ = os.Remove(probe)

	return &LocalStorage{root: abs}, nil
}

func (l *LocalStorage) path(key string) (string, error) {
	if err := validateKey(key); err != nil {
		return "", err
	}
	return filepath.Join(l.root, filepath.FromSlash(key)), nil
}

func (l *LocalStorage) Put(_ context.Context, key string, sealed []byte) error {
	path, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create document directory: %w", err)
	}

	// Write to a temporary name and rename into place, so a crash mid-write
	// leaves no half-file that would later fail to decrypt with a confusing
	// "corrupt ciphertext" instead of an obvious absence.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".upload-*")
	if err != nil {
		return fmt.Errorf("create document temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName) // no-op once the rename has succeeded
	}()

	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("set document permissions: %w", err)
	}
	if _, err := tmp.Write(sealed); err != nil {
		return fmt.Errorf("write document: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync document: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close document: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("finalise document: %w", err)
	}
	return nil
}

func (l *LocalStorage) Get(_ context.Context, key string) ([]byte, error) {
	path, err := l.path(key)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read document: %w", err)
	}
	return data, nil
}

func (l *LocalStorage) Delete(_ context.Context, key string) error {
	path, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete document: %w", err)
	}
	return nil
}

func (l *LocalStorage) Describe() string { return "local:" + l.root }
