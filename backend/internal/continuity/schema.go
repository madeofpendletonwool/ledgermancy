package continuity

import (
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
)

// This file discovers what tables exist by reading the migrations, rather than
// by asking a database.
//
// That choice is the difference between a guard that fires and one that does
// not. A live-database check can only run where a database is configured, which
// in this repo means CI — so the author of a new table would learn about the
// omission after pushing, if at all. Parsing the embedded migrations needs
// nothing, so `go test ./...` on a laptop catches it before the commit lands.
//
// The live-database check still exists (TestSchemaMatchesLiveDatabase); it is
// the backstop that catches anything this parser cannot see, including River's
// and goose's own tables.

var (
	createTableRe = regexp.MustCompile(`(?im)^\s*CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-z_][a-z0-9_]*)`)
	dropTableRe   = regexp.MustCompile(`(?is)\bDROP\s+TABLE\s+(?:IF\s+EXISTS\s+)?(.*?);`)
	renameTableRe = regexp.MustCompile(`(?is)\bALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?([a-z_][a-z0-9_]*)\s+RENAME\s+TO\s+([a-z_][a-z0-9_]*)`)
)

// TablesFromMigrations replays the `-- +goose Up` half of every migration, in
// filename order, and returns the set of tables that exist at the end.
//
// Only the Up half is read, and that is load-bearing rather than tidy: every
// migration's Down half drops what its Up created, so a parser that read the
// whole file would conclude the schema is empty. Filename order matters for the
// same reason goose runs in strict order — a table created in 00019 and dropped
// in 00024 must not survive the replay.
func TablesFromMigrations(fsys fs.FS) ([]string, error) {
	entries, err := fs.Glob(fsys, "migrations/*.sql")
	if err != nil {
		return nil, fmt.Errorf("glob migrations: %w", err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("no migrations found; the embedded filesystem is empty")
	}
	sort.Strings(entries)

	live := map[string]bool{}
	for _, name := range entries {
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path.Base(name), err)
		}
		applyUp(live, upSection(string(body)))
	}

	out := make([]string, 0, len(live))
	for table := range live {
		out = append(out, table)
	}
	sort.Strings(out)
	return out, nil
}

// upSection returns the text between `-- +goose Up` and `-- +goose Down`. A
// migration with no Down is legal, so the Up simply runs to the end of file.
func upSection(body string) string {
	if i := strings.Index(body, "-- +goose Up"); i >= 0 {
		body = body[i+len("-- +goose Up"):]
	}
	if i := strings.Index(body, "-- +goose Down"); i >= 0 {
		body = body[:i]
	}
	return body
}

// applyUp folds one migration's Up half into the running set.
func applyUp(live map[string]bool, sql string) {
	for _, m := range createTableRe.FindAllStringSubmatch(sql, -1) {
		live[m[1]] = true
	}
	for _, m := range dropTableRe.FindAllStringSubmatch(sql, -1) {
		for _, table := range splitDropList(m[1]) {
			delete(live, table)
		}
	}
	for _, m := range renameTableRe.FindAllStringSubmatch(sql, -1) {
		if live[m[1]] {
			delete(live, m[1])
			live[m[2]] = true
		}
	}
}

// splitDropList unpacks the name list of a DROP TABLE, which may span lines and
// carry a trailing CASCADE or RESTRICT.
func splitDropList(list string) []string {
	list = strings.ReplaceAll(list, "\n", " ")
	var out []string
	for _, part := range strings.Split(list, ",") {
		for _, field := range strings.Fields(part) {
			upper := strings.ToUpper(field)
			if upper == "CASCADE" || upper == "RESTRICT" {
				continue
			}
			out = append(out, strings.Trim(field, `" `))
			break
		}
	}
	return out
}
