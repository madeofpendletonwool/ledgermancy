package continuity

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/documents"
)

// The document vault is the reason this file exists, and the reason the
// blobStores registry in coverage.go exists.
//
// pg_dump captures every document's title, type, date and expiry, and not one
// byte of any document. A backup regime that only dumps the database therefore
// restores a vault whose every entry is present, listed, searchable, and
// impossible to open — and it does so while reporting complete success. That is
// the specific two-of-three failure this package is built to refuse.

// Archiver captures a blob store into a single compressed archive.
type Archiver struct {
	Queries *dbgen.Queries
	// Store reads sealed bytes. Blobs are written to the archive still sealed:
	// the archive is therefore no more sensitive than the volume it copies, and
	// — the point — is useless without ENCRYPTION_KEY, exactly like the source.
	Store documents.Storage
}

// ArchiveResult reports what a capture actually managed to include.
type ArchiveResult struct {
	Artefact Artefact
	Blobs    int
	// Missing is blobs the database references and the store does not have.
	// Recorded rather than fatal: the vault already treats this as a real and
	// survivable state (document_handlers.go answers it with a 404 naming the
	// disagreement), and refusing to archive the other 4,000 documents because
	// one is missing would be a worse outcome than a noted gap.
	Missing int
}

// Archive writes every blob the database references into a tar.gz.
//
// Driven from the database's key list rather than by walking the storage root,
// and that choice is what makes the archive verifiable. A directory walk copies
// whatever happens to be on disk, including orphans, and cannot notice an
// absence. Walking the references instead means the archive is complete with
// respect to the dump it will be restored beside — and any blob that is missing
// is counted and reported instead of silently not being there.
//
// It also makes the S3 backend work for free, since the reference list is the
// only thing both backends have in common.
func (a *Archiver) Archive(ctx context.Context, root string) (ArchiveResult, error) {
	blobs, err := a.Queries.ListAllDocumentBlobs(ctx)
	if err != nil {
		return ArchiveResult{}, fmt.Errorf("list document blobs: %w", err)
	}

	dir := Dir(root, KindDocumentsArchive)
	if err := EnsureDir(dir); err != nil {
		return ArchiveResult{}, err
	}

	at := time.Now().UTC()
	name, err := Name(KindDocumentsArchive, at)
	if err != nil {
		return ArchiveResult{}, err
	}
	final := filepath.Join(dir, name)
	tmp := final + ".partial"
	defer os.Remove(tmp)

	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return ArchiveResult{}, fmt.Errorf("create archive: %w", err)
	}
	defer f.Close()

	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	result := ArchiveResult{}
	dirsWritten := map[string]bool{}
	for _, blob := range blobs {
		sealed, err := a.Store.Get(ctx, blob.StorageKey)
		if errors.Is(err, documents.ErrNotFound) {
			result.Missing++
			continue
		}
		if err != nil {
			return result, fmt.Errorf("read blob %s: %w", blob.StorageKey, err)
		}

		// Shard directories are written explicitly, with the vault's own 0700.
		//
		// Without them tar creates the parents implicitly from the umask —
		// 0755 in practice — and a restored vault of tax returns and insurance
		// policies ends up world-readable. The restore would look completely
		// successful, which is what makes it worth fixing here rather than
		// leaving to a chmod line in a runbook somebody skims.
		if err := writeParentDirs(tw, blob.StorageKey, at, dirsWritten); err != nil {
			return result, err
		}

		// The archive member name is the storage key, so a restore can put
		// every blob back exactly where the restored database expects it
		// without consulting anything else.
		if err := tw.WriteHeader(&tar.Header{
			Name:    blob.StorageKey,
			Mode:    0o600,
			Size:    int64(len(sealed)),
			ModTime: at,
		}); err != nil {
			return result, fmt.Errorf("write archive header for %s: %w", blob.StorageKey, err)
		}
		if _, err := tw.Write(sealed); err != nil {
			return result, fmt.Errorf("write blob %s: %w", blob.StorageKey, err)
		}
		result.Blobs++
	}

	if err := tw.Close(); err != nil {
		return result, fmt.Errorf("close tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return result, fmt.Errorf("close gzip: %w", err)
	}
	if err := f.Sync(); err != nil {
		return result, fmt.Errorf("sync archive: %w", err)
	}
	if err := f.Close(); err != nil {
		return result, fmt.Errorf("close archive: %w", err)
	}

	info, err := os.Stat(tmp)
	if err != nil {
		return result, fmt.Errorf("stat archive: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return result, fmt.Errorf("finalise archive: %w", err)
	}

	result.Artefact = Artefact{Kind: KindDocumentsArchive, Path: final, Taken: at, Size: info.Size()}
	return result, nil
}

// writeParentDirs emits tar directory entries for a key's parents, once each,
// so extraction reproduces the vault's 0700 rather than inheriting the umask.
func writeParentDirs(tw *tar.Writer, key string, at time.Time, written map[string]bool) error {
	parts := strings.Split(key, "/")
	prefix := ""
	for _, part := range parts[:max(len(parts)-1, 0)] {
		prefix = path.Join(prefix, part)
		if written[prefix] {
			continue
		}
		if err := tw.WriteHeader(&tar.Header{
			Name:     prefix + "/",
			Typeflag: tar.TypeDir,
			Mode:     0o700,
			ModTime:  at,
		}); err != nil {
			return fmt.Errorf("write archive directory %s: %w", prefix, err)
		}
		written[prefix] = true
	}
	return nil
}

// ExtractBlob pulls one member out of an archive.
//
// Used by the restore test, which opens exactly one document to prove the
// three-way agreement between dump, archive and key. Streaming to the named
// member rather than extracting the whole archive keeps that check cheap enough
// to run on every restore test regardless of how large the vault has grown.
func ExtractBlob(archivePath, storageKey string) ([]byte, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, fmt.Errorf("open archive: %w", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("read archive: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("archive %s does not contain %s", filepath.Base(archivePath), storageKey)
		}
		if err != nil {
			return nil, fmt.Errorf("scan archive: %w", err)
		}
		if hdr.Name != storageKey {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("read %s from archive: %w", storageKey, err)
		}
		return data, nil
	}
}

// capturedBlobStores names the stores Archive actually captures. The registry in
// coverage.go lists what exists; this lists what is backed up, and
// TestEveryBlobStoreIsCaptured fails when the two disagree.
//
// Two lists rather than one because they answer different questions and drift
// apart in exactly one direction: somebody adds a store, remembers to register
// it so the panel looks right, and does not write the capture step.
func capturedBlobStores() []string {
	return []string{"documents"}
}
