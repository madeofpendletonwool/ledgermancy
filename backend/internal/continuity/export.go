package continuity

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The portable export is a different artefact from the dump, and both are
// needed. The dump restores *this app*; the export outlives it.
//
// Self-hosting is a claim about ownership, and a pg_dump only makes that claim
// true for as long as somebody is still running this Go binary against a
// compatible Postgres. The export is the version of the claim that survives
// this project being abandoned: plain JSON, documented, decodable by anything.

// ExportSchemaVersion is the version of the export envelope.
//
// Bump it when the envelope changes shape — not when a table gains a column,
// which readers are expected to tolerate. Rows are emitted with the database's
// own column names, so schema drift within a table is self-describing.
const ExportSchemaVersion = 1

// Export is the envelope written to the JSON file.
type Export struct {
	Meta   ExportMeta                  `json:"meta"`
	Tables map[string][]map[string]any `json:"tables"`
}

// ExportMeta is everything a reader needs to interpret the tables, including
// enough to tell whether it is looking at a complete export.
type ExportMeta struct {
	SchemaVersion    int       `json:"schema_version"`
	GeneratedAt      time.Time `json:"generated_at"`
	MigrationVersion int64     `json:"migration_version"`
	Application      string    `json:"application"`
	// TableCount and RowCounts let a reader detect a truncated file without
	// parsing all of it, and let a human sanity-check an export at a glance.
	RowCounts map[string]int `json:"row_counts"`
	// Note is addressed to whoever opens this file years from now, possibly
	// without this application to hand.
	Note string `json:"note"`
}

const exportNote = "Portable export from Ledgermancy. Every monetary value is a decimal " +
	"string, never a JSON number, so no reader can round it. Tables are keyed by their " +
	"database name and rows by their column name. Credentials (password hashes, encrypted " +
	"tokens and secrets) are deliberately absent: this file is your data, not your logins."

// Exporter produces the portable export.
type Exporter struct {
	Pool *pgxpool.Pool
}

// Build assembles the export in memory.
//
// The table list comes from the coverage registry rather than from a list kept
// here, which is the entire reason the registry exists: a table added in a
// later wave and classified InExport appears in the export without anyone
// editing this file. TestEveryInExportTableHasAnExporter enforces the other
// direction — that nothing classified InExport is silently skipped.
func (e *Exporter) Build(ctx context.Context) (*Export, error) {
	out := &Export{
		Meta: ExportMeta{
			SchemaVersion: ExportSchemaVersion,
			GeneratedAt:   time.Now().UTC(),
			Application:   "ledgermancy",
			RowCounts:     map[string]int{},
			Note:          exportNote,
		},
		Tables: map[string][]map[string]any{},
	}

	if err := e.Pool.QueryRow(ctx,
		`SELECT COALESCE(max(version_id), 0) FROM goose_db_version`,
	).Scan(&out.Meta.MigrationVersion); err != nil {
		return nil, fmt.Errorf("read migration version: %w", err)
	}

	for _, table := range TablesWithCoverage(InExport) {
		rows, err := e.exportTable(ctx, table)
		if err != nil {
			return nil, fmt.Errorf("export %s: %w", table, err)
		}
		out.Tables[table] = rows
		out.Meta.RowCounts[table] = len(rows)
	}
	return out, nil
}

// column describes one column as the export will read it.
type column struct {
	Name string
	Type string
	// Expr is the SQL that reads it. Numerics and dates are cast to text in
	// the query itself — see selectExpr.
	Expr string
}

// exportTable reads one table.
func (e *Exporter) exportTable(ctx context.Context, table string) ([]map[string]any, error) {
	cols, err := e.columns(ctx, table)
	if err != nil {
		return nil, err
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("table has no exportable columns")
	}

	sql := "SELECT "
	for i, c := range cols {
		if i > 0 {
			sql += ", "
		}
		sql += c.Expr
	}
	// Table names come from the coverage registry — a Go map literal in this
	// repository, never from a request — so interpolating one here cannot carry
	// user input. Quoted regardless, because the day that stops being true
	// should not also be the day this becomes an injection.
	sql += fmt.Sprintf(` FROM %q`, table)

	rows, err := e.Pool.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []map[string]any{}
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			return nil, err
		}
		record := make(map[string]any, len(cols))
		for i, c := range cols {
			if i >= len(values) {
				break
			}
			record[c.Name] = normalise(values[i], c.Type)
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

// columns reads a table's shape from the catalog, dropping anything sensitive.
func (e *Exporter) columns(ctx context.Context, table string) ([]column, error) {
	rows, err := e.Pool.Query(ctx,
		`SELECT column_name, data_type
		   FROM information_schema.columns
		  WHERE table_schema = 'public' AND table_name = $1
		  ORDER BY ordinal_position`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []column
	for rows.Next() {
		var name, dataType string
		if err := rows.Scan(&name, &dataType); err != nil {
			return nil, err
		}
		if IsSensitive(table, name, dataType) {
			continue
		}
		out = append(out, column{Name: name, Type: dataType, Expr: selectExpr(name, dataType)})
	}
	return out, rows.Err()
}

// selectExpr builds the SQL that reads one column.
//
// The `numeric` case is the most important line in this package.
//
// Money in this application is exact — NUMERIC in Postgres, shopspring/decimal
// in Go, `StringFixed(2)` at every boundary — and an export is the easiest place
// in the whole codebase to undo that, because JSON's only number type is a
// double. Casting to text in the *query* means the value is already a string
// before it reaches the driver, so no type map, struct tag, or marshaller
// setting anywhere downstream is in a position to turn 1234.56 into
// 1234.5599999999999. It is not that the alternatives are wrong today; it is
// that this way there is nothing left to get wrong later.
//
// `jsonb` is cast for the same reason, one level down and easier to miss.
// pgx decodes a JSON column into Go values before this code sees it, so any
// number inside a stored document — and transactions.raw is Plaid's payload,
// full of them — would become a float64 and be re-encoded from that float.
// Taking the column as text means the stored bytes are passed through
// unexamined, which is both exact and what a reader of the export wants.
//
// `uuid` is cast because pgx's generic row scan hands back a [16]byte, which
// Go marshals as a sixteen-element array of numbers. Every primary and foreign
// key in the export would have been an unreadable list of integers — parseable,
// technically lossless, and completely useless to the person this file is for.
// The whole value of the export is that somebody can open it.
//
// Dates are cast for a smaller reason: a `date` rendered through time.Time
// becomes "2026-07-29T00:00:00Z", which invites a reader to believe in a
// midnight that was never in the data.
//
// The pattern across all four: anything whose Go representation is a decision
// made by a driver gets rendered by Postgres instead, where the result is
// specified. Only types whose Go form is obviously right — text, bool, the
// integers, timestamps — are passed through.
func selectExpr(name, dataType string) string {
	switch dataType {
	case "numeric", "jsonb", "json", "uuid":
		return fmt.Sprintf("%q::text AS %q", name, name)
	case "date":
		return fmt.Sprintf("to_char(%q, 'YYYY-MM-DD') AS %q", name, name)
	default:
		return fmt.Sprintf("%q", name)
	}
}

// normalise converts a scanned value into something json.Marshal renders the
// way a reader expects.
//
// JSON columns arrive here as text, because selectExpr cast them. Re-wrapping
// that text as a RawMessage emits stored JSON as JSON rather than as a string
// containing JSON — while still never having parsed it.
func normalise(v any, dataType string) any {
	switch dataType {
	case "jsonb", "json":
		switch t := v.(type) {
		case string:
			if t == "" {
				return nil
			}
			return json.RawMessage(t)
		case []byte:
			if len(t) == 0 {
				return nil
			}
			return json.RawMessage(t)
		case nil:
			return nil
		}
	}
	if b, ok := v.([]byte); ok {
		// Any remaining binary would base64-encode silently. Nothing should
		// reach here — bytea is excluded by IsSensitive — so this is a guard,
		// not a conversion.
		return fmt.Sprintf("<%d bytes withheld>", len(b))
	}
	return v
}

// Write serialises an export to a gzipped JSON file in root.
func (e *Exporter) Write(ctx context.Context, root string) (Artefact, error) {
	export, err := e.Build(ctx)
	if err != nil {
		return Artefact{}, err
	}

	dir := Dir(root, KindExport)
	if err := EnsureDir(dir); err != nil {
		return Artefact{}, err
	}
	at := export.Meta.GeneratedAt
	name, err := Name(KindExport, at)
	if err != nil {
		return Artefact{}, err
	}
	final := filepath.Join(dir, name)
	tmp := final + ".partial"
	defer os.Remove(tmp)

	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return Artefact{}, fmt.Errorf("create export: %w", err)
	}
	defer f.Close()

	gz := gzip.NewWriter(f)
	enc := json.NewEncoder(gz)
	enc.SetIndent("", "  ")
	if err := enc.Encode(export); err != nil {
		return Artefact{}, fmt.Errorf("encode export: %w", err)
	}
	if err := gz.Close(); err != nil {
		return Artefact{}, fmt.Errorf("close gzip: %w", err)
	}
	if err := f.Sync(); err != nil {
		return Artefact{}, fmt.Errorf("sync export: %w", err)
	}
	if err := f.Close(); err != nil {
		return Artefact{}, fmt.Errorf("close export: %w", err)
	}

	info, err := os.Stat(tmp)
	if err != nil {
		return Artefact{}, fmt.Errorf("stat export: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return Artefact{}, fmt.Errorf("finalise export: %w", err)
	}
	return Artefact{Kind: KindExport, Path: final, Taken: at, Size: info.Size()}, nil
}
