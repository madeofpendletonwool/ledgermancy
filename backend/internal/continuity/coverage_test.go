package continuity

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db"
)

// The tests in this file are the point of the package.
//
// Everything else here backs data up. These decide whether the *next* feature's
// data gets backed up, by failing the build when a new table, or a new volume,
// arrives without anyone having said what happens to it in a restore. They take
// no database and no fixtures, so they run on every `go test ./...` rather than
// only in CI — which is the only way the author of the new table finds out in
// time to do something about it.
//
// If you are here because one of them failed: the fix is a one-line entry, not
// a workaround. See coverage.go.

const fixInstructions = `
Fix: add it to tableCoverage in backend/internal/continuity/coverage.go, picking
the category that is true:

  InExport  - user data. Things the household created, decided, or accumulated
              that no re-sync would bring back. Also enrols the table in the
              restore test's row-count check automatically.
  DumpOnly  - credentials, or app-internal bookkeeping that means nothing
              outside this application. In the pg_dump, not in the portable
              JSON export.
  Derived   - a job already rebuilds it. Name the job in the comment.
  Ephemeral - restoring it would be wrong: sessions, challenges, queue rows.

If the table is InExport you must also give it a serializer in export.go, or
TestEveryInExportTableHasAnExporter fails next.`

func TestEveryTableIsClassified(t *testing.T) {
	tables, err := TablesFromMigrations(db.Migrations())
	if err != nil {
		t.Fatalf("parse migrations: %v", err)
	}
	if len(tables) < 40 {
		t.Fatalf("parsed only %d tables from the migrations, which means the parser "+
			"is broken rather than the schema being small; every guard in this file "+
			"is worthless until that is fixed", len(tables))
	}

	for _, table := range tables {
		if _, ok := Classify(table); !ok {
			t.Errorf("table %q exists in the migrations but is not classified for continuity.\n"+
				"Nothing decides whether it is backed up, which means it probably is not.%s",
				table, fixInstructions)
		}
	}
}

func TestNoStaleClassifications(t *testing.T) {
	tables, err := TablesFromMigrations(db.Migrations())
	if err != nil {
		t.Fatalf("parse migrations: %v", err)
	}
	live := map[string]bool{}
	for _, table := range tables {
		live[table] = true
	}

	for _, table := range ClassifiedTables() {
		if !live[table] {
			t.Errorf("table %q is classified in coverage.go but no migration creates it.\n"+
				"Either it was dropped and the entry should go, or it was renamed and the "+
				"entry should follow — a stale entry makes the guard quieter than it looks.", table)
		}
	}
}

// TestMigrationParserHandlesDownSections pins the one thing the parser could get
// wrong without anybody noticing: every migration's Down half drops what its Up
// created, so a parser that read whole files would find an empty schema and then
// pass every other test in this file for the wrong reason.
func TestMigrationParserHandlesDownSections(t *testing.T) {
	const migration = `-- +goose Up
CREATE TABLE kept (id UUID PRIMARY KEY);
CREATE TABLE IF NOT EXISTS also_kept (id UUID PRIMARY KEY);

-- +goose Down
DROP TABLE IF EXISTS also_kept, kept CASCADE;
`
	live := map[string]bool{}
	applyUp(live, upSection(migration))

	if !live["kept"] || !live["also_kept"] {
		t.Fatalf("Up half was not applied: %v", live)
	}
	if len(live) != 2 {
		t.Fatalf("Down half leaked into the parse: %v", live)
	}

	// And a DROP in an *Up* half must still take effect.
	applyUp(live, upSection("-- +goose Up\nDROP TABLE kept;\n"))
	if live["kept"] {
		t.Error("DROP TABLE in an Up section was ignored")
	}
}

// TestComposeVolumesAreClassified guards the half of durability that has nothing
// to do with Postgres. A wave-5 feature that adds a volume — a cache, an upload
// staging area, a search index — fails here until somebody has decided whether
// losing it matters. "It does not matter" is a fine answer; not deciding is not.
func TestComposeVolumesAreClassified(t *testing.T) {
	volumes, err := composeVolumeNames(repoFile(t, "docker-compose.yml"))
	if err != nil {
		t.Fatalf("read compose volumes: %v", err)
	}
	if len(volumes) == 0 {
		t.Fatal("parsed no volumes from docker-compose.yml; the parser is broken")
	}

	for _, name := range volumes {
		if _, ok := VolumeCoverage(name); !ok {
			t.Errorf("compose volume %q is not classified for continuity.\n"+
				"Add it to composeVolumes in backend/internal/continuity/coverage.go with a "+
				"one-line statement of how it is captured — or that it is disposable and why.", name)
		}
	}
}

func TestNoStaleVolumeClassifications(t *testing.T) {
	volumes, err := composeVolumeNames(repoFile(t, "docker-compose.yml"))
	if err != nil {
		t.Fatalf("read compose volumes: %v", err)
	}
	declared := map[string]bool{}
	for _, name := range volumes {
		declared[name] = true
	}
	for name := range composeVolumes {
		if !declared[name] {
			t.Errorf("volume %q is classified in coverage.go but docker-compose.yml no longer declares it", name)
		}
	}
}

// TestSchemaMatchesLiveDatabase is the backstop: it catches anything the
// migration parser cannot see, including River's and goose's own tables, which
// no migration in this repo creates.
//
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' \
//	  go test ./internal/continuity/
func TestSchemaMatchesLiveDatabase(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	rows, err := pool.Query(ctx,
		`SELECT table_name FROM information_schema.tables
		  WHERE table_schema = 'public' AND table_type = 'BASE TABLE'
		  ORDER BY table_name`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()

	var live []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		live = append(live, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	for _, table := range live {
		if _, ok := Classify(table); !ok {
			t.Errorf("table %q exists in a migrated database but is not classified for continuity.%s",
				table, fixInstructions)
		}
	}

	// The parser and the database must agree about the application's own
	// tables. A divergence means the parser has started missing things and the
	// laptop-side guard has quietly stopped guarding.
	parsed, err := TablesFromMigrations(db.Migrations())
	if err != nil {
		t.Fatalf("parse migrations: %v", err)
	}
	inDB := map[string]bool{}
	for _, name := range live {
		inDB[name] = true
	}
	for _, name := range parsed {
		if !inDB[name] {
			t.Errorf("the migration parser found table %q, but a migrated database has no such table", name)
		}
	}
}

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

// composeVolumeNames reads the top-level `volumes:` block of a compose file.
//
// Hand-parsed rather than pulled through a YAML library on purpose: this repo
// keeps a deliberately short direct-dependency list, and promoting yaml.v3 from
// indirect to direct to read one fixed-shape block of one file we control is a
// poor trade. The parser is intentionally strict — a compose file it cannot
// understand yields zero volumes, and the caller fails on that rather than
// silently passing.
func composeVolumeNames(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []string
	inBlock := false
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		// A column-0 key ends the previous block and may start ours.
		if !strings.HasPrefix(line, " ") {
			inBlock = trimmed == "volumes:"
			continue
		}
		if !inBlock {
			continue
		}
		// Entries are `  name:` at exactly one level of indent. Anything
		// deeper is a volume's own configuration, not another name.
		if strings.HasPrefix(line, "   ") {
			continue
		}
		name, _, found := strings.Cut(trimmed, ":")
		if !found {
			continue
		}
		out = append(out, strings.TrimSpace(name))
	}
	return out, scanner.Err()
}

// repoFile resolves a path relative to the repository root.
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	// backend/internal/continuity -> repository root
	path, err := filepath.Abs(filepath.Join("..", "..", "..", rel))
	if err != nil {
		t.Fatalf("resolve %s: %v", rel, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected %s at %s: %v", rel, path, err)
	}
	return path
}
