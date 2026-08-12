// Package testdb hands every test package its own Postgres database.
//
// `go test ./...` runs packages in parallel (-p defaults to GOMAXPROCS). Before
// this package existed, every DB-backed test in the tree opened the one database
// named by TEST_DATABASE_URL, so internal/api, internal/db, internal/insights and
// friends seeded, mutated and deleted the same rows at the same time. The suite
// failed in a different test on roughly every other run, which is worse than a
// slow suite: it teaches everyone to re-run, and a real regression arrives
// disguised as "probably the flake again".
//
// URL fixes that at the only level that actually isolates: a separate database
// per package. TEST_DATABASE_URL keeps its meaning — it points at a throwaway
// Postgres, and its database is used as the admin connection from which the
// per-package databases are created.
//
// # The contract
//
// URL does not migrate. The caller runs migrations, exactly as every fixture in
// this repo already did:
//
//	url := testdb.URL(t)
//	pool, err := db.Connect(ctx, url)
//	...
//	if err := db.Migrate(ctx, pool); err != nil { ... }
//
// Every entry point into a package's tests must migrate, not just the first one
// to run — file order is not a guarantee worth resting on. Migration is
// idempotent, so the second and later fixtures in a package pay nothing.
//
// (This package deliberately does not migrate for you. Doing so would mean
// importing internal/db, and internal/db's own tests live in package db — that
// is an import cycle. Provisioning stays dependency-free instead.)
//
// # Lifetime
//
// A package's database is created once and then REUSED by later runs, which is
// how the single shared database behaved before this package existed. It is
// worth being deliberate about, because the alternative is not free: dropping
// and recreating every run costs a full migration per package — about 25
// seconds for internal/db alone, and roughly a doubling of the whole suite —
// whereas against an existing database goose is a no-op.
//
// The cost of reuse is that a migration edited IN PLACE has already been
// applied and will not run again. Set TESTDB_FRESH=1 to drop and rebuild:
//
//	TESTDB_FRESH=1 go test ./...
//
// Nothing is dropped at the end of a run. A test binary has no reliable
// "everything has finished" hook short of a TestMain in every package, and
// leaving the rows in place means a failure can still be opened in psql
// afterwards.
package testdb

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// selfPkg is this package's import path, used to skip our own stack frames when
// working out which package called us.
const selfPkg = "github.com/madeofpendletonwool/ledgermancy/backend/internal/testdb"

var (
	mu    sync.Mutex
	ready = map[string]string{} // package name -> connection URL
)

// URL returns a connection string for a database private to the calling test
// package, creating it on first use. The test is skipped when TEST_DATABASE_URL
// is unset, which is what lets the suite run on a machine with no Postgres.
//
// The database is not migrated; the caller does that. See the package comment.
func URL(t *testing.T) string {
	t.Helper()

	admin := os.Getenv("TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	pkg := callerPackage()

	mu.Lock()
	defer mu.Unlock()
	if u, ok := ready[pkg]; ok {
		return u
	}

	u, err := provision(admin, pkg)
	if err != nil {
		t.Fatalf("provision test database for package %s: %v", pkg, err)
	}
	ready[pkg] = u
	return u
}

// provision makes sure the package's database exists, rebuilding it from
// scratch when TESTDB_FRESH is set.
func provision(admin, pkg string) (string, error) {
	name, err := databaseName(admin, pkg)
	if err != nil {
		return "", err
	}
	target, err := withDatabase(admin, name)
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		return "", fmt.Errorf("connect to %s: %w", redact(admin), err)
	}
	defer conn.Close(context.Background())

	quoted := pgx.Identifier{name}.Sanitize()

	if os.Getenv("TESTDB_FRESH") != "" {
		// FORCE terminates anything still attached — a killed `go test` leaves
		// connections behind and DROP would otherwise block on them.
		if _, err := conn.Exec(ctx, `DROP DATABASE IF EXISTS `+quoted+` WITH (FORCE)`); err != nil {
			return "", fmt.Errorf("drop %s: %w", name, err)
		}
	}

	var exists bool
	if err := conn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, name).Scan(&exists); err != nil {
		return "", fmt.Errorf("look for %s: %w", name, err)
	}
	if exists {
		return target, nil
	}

	if _, err := conn.Exec(ctx, `CREATE DATABASE `+quoted); err != nil {
		return "", fmt.Errorf("create %s (does the test role have CREATEDB?): %w", name, err)
	}
	return target, nil
}

// databaseName derives the per-package database name from the admin URL's own
// database, so a run against `lmtest` produces `lmtest_api`, `lmtest_db`, and so
// on — recognisably one family, and never colliding with a real database.
func databaseName(admin, pkg string) (string, error) {
	u, err := url.Parse(admin)
	if err != nil {
		return "", fmt.Errorf("parse TEST_DATABASE_URL: %w", err)
	}
	base := strings.TrimPrefix(u.Path, "/")
	if base == "" {
		return "", fmt.Errorf("TEST_DATABASE_URL names no database: %s", redact(admin))
	}

	name := base + "_" + pkg
	// Postgres truncates identifiers at 63 bytes; do it ourselves so the name we
	// quote is the name that exists.
	if len(name) > 63 {
		name = name[:63]
	}
	return name, nil
}

// withDatabase rewrites the admin URL to point at a different database, leaving
// credentials, host and query parameters (sslmode and friends) alone.
func withDatabase(admin, name string) (string, error) {
	u, err := url.Parse(admin)
	if err != nil {
		return "", fmt.Errorf("parse TEST_DATABASE_URL: %w", err)
	}
	u.Path = "/" + name
	return u.String(), nil
}

// callerPackage walks out of this package and reports the short name of the
// package that called in — "api" for .../internal/api. Walking rather than a
// fixed frame depth keeps it correct through the fixture helpers that wrap URL.
func callerPackage() string {
	pcs := make([]uintptr, 32)
	n := runtime.Callers(2, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	for {
		frame, more := frames.Next()
		// frame.Function looks like
		// "github.com/…/internal/api.newAllocationFixture" or, for a method,
		// "github.com/…/internal/api.(*fixture).seed".
		if fn := frame.Function; fn != "" && !strings.HasPrefix(fn, selfPkg+".") {
			last := fn
			if i := strings.LastIndex(fn, "/"); i >= 0 {
				last = fn[i+1:]
			}
			if i := strings.Index(last, "."); i >= 0 {
				return last[:i]
			}
			return last
		}
		if !more {
			break
		}
	}
	return "unknown"
}

// redact strips the password before a connection string reaches an error
// message, since test output ends up in CI logs.
func redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparseable connection string>"
	}
	if u.User != nil {
		if _, set := u.User.Password(); set {
			u.User = url.UserPassword(u.User.Username(), "xxxxx")
		}
	}
	return u.String()
}
