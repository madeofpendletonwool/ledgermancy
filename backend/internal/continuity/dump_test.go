package continuity

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/testdb"
)

// The version check is the one place in this package where carrying on after a
// problem would be actively harmful.
//
// pg_dump from an older major cannot read a newer server's catalogs. Sometimes
// that fails loudly; sometimes it produces an archive that only fails at
// restore time, months later, at the exact moment nothing else is going right.
// A mismatch must therefore produce NO file — a stale backup is a problem an
// operator can see, and a fresh one that does not restore is not.

// stubPGDump writes a fake pg_dump reporting the given version, and returns a
// BinDir containing it.
func stubPGDump(t *testing.T, version string) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"--version\" ]; then echo 'pg_dump (PostgreSQL) " + version + "'; exit 0; fi\n" +
		// If the version guard is working, this line is never reached. It
		// exists so that a regression shows up as a created file rather than
		// as a silent pass.
		"echo 'STUB PG_DUMP RAN' >&2\n" +
		"exit 0\n"
	path := filepath.Join(dir, "pg_dump")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return dir
}

func TestVersionMismatchIsDetected(t *testing.T) {
	url := testdb.URL(t)
	ctx := context.Background()
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	// The test Postgres is 17; claim to be 15.
	d := &Dumper{Pool: pool, DatabaseURL: url, BinDir: stubPGDump(t, "15.6")}

	err = d.CheckVersions(ctx)
	if err == nil {
		t.Fatal("a cross-major pg_dump was accepted; it would produce archives that do not restore")
	}
	for _, want := range []string{"15", "17", "postgresql17-client"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message does not mention %q, so an operator cannot act on it: %v", want, err)
		}
	}
}

// TestVersionMismatchWritesNoDump is the half that matters. Detecting the
// mismatch is worth nothing if a file is left behind anyway — retention would
// keep it, the panel would call it a backup, and it would fail on the day it
// was needed.
func TestVersionMismatchWritesNoDump(t *testing.T) {
	url := testdb.URL(t)
	ctx := context.Background()
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	root := t.TempDir()
	d := &Dumper{Pool: pool, DatabaseURL: url, BinDir: stubPGDump(t, "15.6")}

	if _, err := d.Dump(ctx, root); err == nil {
		t.Fatal("Dump succeeded against a mismatched pg_dump")
	}

	arts, err := List(root, KindDBDump)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(arts) != 0 {
		t.Errorf("a mismatched pg_dump left %d artefact(s) behind; "+
			"they would be counted as backups and would not restore", len(arts))
	}
	// Not even a partial file, which retention ignores but an operator would
	// find and reasonably mistake for a backup.
	entries, _ := os.ReadDir(Dir(root, KindDBDump))
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("files left in the dump directory after a refused dump: %v", names)
	}
}

// TestMatchingVersionIsAccepted keeps the guard honest: a check that refuses
// everything would pass the two tests above and break every deployment.
func TestMatchingVersionIsAccepted(t *testing.T) {
	url := testdb.URL(t)
	ctx := context.Background()
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	var serverMajor int
	if err := pool.QueryRow(ctx,
		`SELECT current_setting('server_version_num')::int / 10000`).Scan(&serverMajor); err != nil {
		t.Fatalf("read server version: %v", err)
	}

	// Same major, different minor — the routine case after a server patch,
	// which must not fail.
	d := &Dumper{Pool: pool, DatabaseURL: url, BinDir: stubPGDump(t, strconv.Itoa(serverMajor)+".1")}
	if err := d.CheckVersions(ctx); err != nil {
		t.Errorf("a matching major was refused: %v", err)
	}
}
