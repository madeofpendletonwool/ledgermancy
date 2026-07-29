package continuity

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Artefact kinds. These double as backup_runs.kind values and as the
// subdirectory each artefact lands in, so a `find` in the backup directory and
// a row in the panel use the same words.
const (
	KindDBDump           = "db_dump"
	KindDocumentsArchive = "documents_archive"
	KindExport           = "export"
	KindRestoreTest      = "restore_test"
	KindMirrorPush       = "mirror_push"
	KindKeyAck           = "key_ack"
)

// extensions names the file each producing kind writes. Kinds that produce no
// file are absent, and Artefact listing refuses to guess for them.
var extensions = map[string]string{
	KindDBDump:           ".dump",
	KindDocumentsArchive: ".tar.gz",
	KindExport:           ".json.gz",
}

// stampLayout is the timestamp embedded in every artefact filename.
//
// Filesystem-safe (no colons, which Windows and some SMB shares reject — and a
// NAS mount is the expected mirror destination), UTC, and lexically sortable so
// `ls` shows them in age order without a flag.
const stampLayout = "20060102T150405Z"

// Artefact is one file on disk.
//
// Taken comes from the filename rather than from mtime or from backup_runs, and
// that is deliberate: copying an artefact to a mirror rewrites its mtime, and a
// database that has itself been restored will not have the rows. The name is the
// only property that survives every path the file can take.
type Artefact struct {
	Kind  string
	Path  string
	Taken time.Time
	Size  int64
}

// Name builds the filename for a new artefact of this kind.
func Name(kind string, at time.Time) (string, error) {
	ext, ok := extensions[kind]
	if !ok {
		return "", fmt.Errorf("kind %q produces no artefact file", kind)
	}
	return fmt.Sprintf("ledgermancy-%s-%s%s", kind, at.UTC().Format(stampLayout), ext), nil
}

// Dir is the directory one kind's artefacts live in under a destination root.
func Dir(root, kind string) string { return filepath.Join(root, kind) }

// EnsureDir creates a destination directory.
//
// 0700 throughout: a backup directory holds the entire financial database in
// restorable form. It is the single most sensitive path on the host, and the
// fact that its contents are "just backups" is exactly the reasoning that leaves
// them world-readable.
func EnsureDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	return nil
}

// List returns one kind's artefacts in a destination, newest first.
//
// Files whose names do not parse are ignored rather than errored on: an
// operator's own `ledgermancy-2024-manual.dump` sitting in the directory should
// not stop the schedule, and — more importantly — must never be considered for
// deletion by retention. Retention only ever removes files it can prove it
// wrote.
func List(root, kind string) ([]Artefact, error) {
	dir := Dir(root, kind)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}

	var out []Artefact
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		taken, ok := parseStamp(kind, e.Name())
		if !ok {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Artefact{
			Kind:  kind,
			Path:  filepath.Join(dir, e.Name()),
			Taken: taken,
			Size:  info.Size(),
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Taken.After(out[j].Taken) })
	return out, nil
}

// Latest returns the newest artefact of a kind, or false when there is none.
func Latest(root, kind string) (Artefact, bool, error) {
	arts, err := List(root, kind)
	if err != nil || len(arts) == 0 {
		return Artefact{}, false, err
	}
	return arts[0], true, nil
}

// parseStamp recovers the timestamp from a filename this package wrote.
func parseStamp(kind, name string) (time.Time, bool) {
	ext, ok := extensions[kind]
	if !ok {
		return time.Time{}, false
	}
	prefix := "ledgermancy-" + kind + "-"
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ext) {
		return time.Time{}, false
	}
	stamp := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ext)
	t, err := time.Parse(stampLayout, stamp)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// CopyFile copies an artefact to a destination directory, via a temporary name
// so a mirror that is interrupted mid-copy never leaves a truncated file that
// looks like a complete backup.
//
// That failure is worth the extra rename: the mirror is usually a network
// share, network shares are the likeliest thing in the deployment to vanish
// mid-write, and a half-copied dump is indistinguishable from a whole one until
// someone tries to restore it.
func CopyFile(src, destDir string) (string, int64, error) {
	if err := EnsureDir(destDir); err != nil {
		return "", 0, err
	}
	final := filepath.Join(destDir, filepath.Base(src))

	in, err := os.Open(src)
	if err != nil {
		return "", 0, fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()

	tmp, err := os.CreateTemp(destDir, ".copy-*")
	if err != nil {
		return "", 0, fmt.Errorf("create temp in %s: %w", destDir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName) // no-op once the rename has succeeded
	}()

	if err := tmp.Chmod(0o600); err != nil {
		return "", 0, fmt.Errorf("set permissions on %s: %w", tmpName, err)
	}
	n, err := io.Copy(tmp, in)
	if err != nil {
		return "", 0, fmt.Errorf("copy %s: %w", src, err)
	}
	if err := tmp.Sync(); err != nil {
		return "", 0, fmt.Errorf("sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return "", 0, fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		return "", 0, fmt.Errorf("finalise %s: %w", final, err)
	}
	return final, n, nil
}

// Note: there is deliberately no directory-size helper here. The continuity
// panel is served by the api, which does not mount the backup volume — only the
// worker does, and only the worker should. Everything the panel shows comes
// from backup_runs, which records each artefact's size and path as it is
// written. That keeps the api out of the backup filesystem entirely.
