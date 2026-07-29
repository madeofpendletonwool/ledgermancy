package continuity

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// pg_dump writes a custom-format archive rather than plain SQL.
//
// Plain SQL piped through gzip is what DEPLOYING.md used to document by hand,
// and it reads more approachably, but custom format is what a restore actually
// wants: pg_restore can go parallel, can restore selectively when only one table
// was lost, and refuses outright to load an archive it does not understand
// instead of half-applying it the way psql would. The approachable, greppable
// artefact is the portable JSON export, which exists precisely so the dump does
// not have to be both.
const dumpFormat = "custom"

// commandTimeout bounds one pg_dump / pg_restore invocation. Generous — a
// household database restores in seconds and this is sized for one that has not
// been — but bounded, so a hung child process cannot wedge the queue slot
// forever.
const commandTimeout = 30 * time.Minute

// Dumper produces database dumps.
type Dumper struct {
	Pool        *pgxpool.Pool
	DatabaseURL string
	// BinDir optionally overrides where pg_dump is looked up. Empty means PATH.
	BinDir string
}

// Dump writes a compressed custom-format archive into root and returns it.
//
// The version check happens first and is fatal to the run. This is the one
// place in the subsystem where "carry on and log a warning" would be actively
// harmful: pg_dump from an older major cannot read a newer server's catalogs,
// and the archive it produces — when it produces one at all — fails at restore
// time, months later, at the exact moment nothing else is going right. A
// mismatch must produce no file, so that the panel shows a stale backup (a
// problem an operator can see) rather than a fresh one that does not work (a
// problem they cannot).
func (d *Dumper) Dump(ctx context.Context, root string) (Artefact, error) {
	if err := d.CheckVersions(ctx); err != nil {
		return Artefact{}, err
	}

	dir := Dir(root, KindDBDump)
	if err := EnsureDir(dir); err != nil {
		return Artefact{}, err
	}

	at := time.Now().UTC()
	name, err := Name(KindDBDump, at)
	if err != nil {
		return Artefact{}, err
	}
	final := filepath.Join(dir, name)

	// Dump to a temporary name and rename into place. Without this, a crash or
	// a full disk mid-dump leaves a partial archive whose filename says it is a
	// complete backup — and the panel, and retention, would both believe it.
	tmp := final + ".partial"
	defer os.Remove(tmp)

	runCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, d.bin("pg_dump"),
		"--dbname="+d.DatabaseURL,
		"--format="+dumpFormat,
		"--compress=6",
		// The restore target is a database this app creates and owns, so
		// ownership and ACL statements are noise that makes a restore into a
		// differently-named role fail for no reason.
		"--no-owner",
		"--no-privileges",
		"--file="+tmp,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return Artefact{}, fmt.Errorf("pg_dump failed: %w: %s", err, tidyStderr(stderr.String()))
	}

	info, err := os.Stat(tmp)
	if err != nil {
		return Artefact{}, fmt.Errorf("pg_dump reported success but wrote no archive: %w", err)
	}
	if info.Size() == 0 {
		return Artefact{}, fmt.Errorf("pg_dump wrote a zero-byte archive")
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return Artefact{}, fmt.Errorf("set permissions on dump: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return Artefact{}, fmt.Errorf("finalise dump: %w", err)
	}

	return Artefact{Kind: KindDBDump, Path: final, Taken: at, Size: info.Size()}, nil
}

// CheckVersions compares the pg_dump binary's major version against the
// server's, and refuses to proceed when they differ.
//
// Only the major is compared. Postgres guarantees dump/restore compatibility
// within a major and explicitly does not guarantee an older pg_dump can read a
// newer server, so major is the line that matters; requiring an exact minor
// match would fail every routine server patch for no benefit.
func (d *Dumper) CheckVersions(ctx context.Context) error {
	toolMajor, toolVersion, err := d.toolVersion(ctx, "pg_dump")
	if err != nil {
		return err
	}
	serverMajor, serverVersion, err := d.serverVersion(ctx)
	if err != nil {
		return err
	}
	if toolMajor != serverMajor {
		return fmt.Errorf(
			"pg_dump is version %s (major %d) but the server is %s (major %d): "+
				"a cross-major dump either fails outright or produces an archive that will not restore, "+
				"so no dump was written. Install postgresql%d-client in the worker image to match the server",
			toolVersion, toolMajor, serverVersion, serverMajor, serverMajor)
	}
	return nil
}

var versionRe = regexp.MustCompile(`(\d+)(?:\.(\d+))?`)

// toolVersion runs `<tool> --version` and returns its major.
func (d *Dumper) toolVersion(ctx context.Context, tool string) (int, string, error) {
	runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	out, err := exec.CommandContext(runCtx, d.bin(tool), "--version").Output()
	if err != nil {
		return 0, "", fmt.Errorf(
			"cannot run %s: %w — the worker image must include the postgresql client tools for backups to work",
			tool, err)
	}
	// e.g. "pg_dump (PostgreSQL) 17.2"
	text := strings.TrimSpace(string(out))
	m := versionRe.FindStringSubmatch(text)
	if m == nil {
		return 0, text, fmt.Errorf("cannot parse %s version from %q", tool, text)
	}
	major, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, text, fmt.Errorf("cannot parse %s major version from %q", tool, text)
	}
	return major, text, nil
}

// serverVersion reads the server's major from server_version_num, which is an
// integer precisely so it does not have to be parsed out of prose.
func (d *Dumper) serverVersion(ctx context.Context) (int, string, error) {
	var num int
	if err := d.Pool.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&num); err != nil {
		return 0, "", fmt.Errorf("read server version: %w", err)
	}
	var text string
	if err := d.Pool.QueryRow(ctx, `SELECT current_setting('server_version')`).Scan(&text); err != nil {
		text = strconv.Itoa(num)
	}
	return num / 10000, text, nil
}

func (d *Dumper) bin(tool string) string {
	if d.BinDir == "" {
		return tool
	}
	return filepath.Join(d.BinDir, tool)
}

// tidyStderr trims a child process's stderr to something worth putting in
// backup_runs.detail, which an operator reads in a web page rather than a log.
func tidyStderr(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(no output)"
	}
	const max = 2000
	if len(s) > max {
		return s[:max] + "… (truncated)"
	}
	return s
}
