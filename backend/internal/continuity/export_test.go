package continuity

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db"
)

// TestEveryBlobStoreIsCaptured is the non-Postgres half of the coverage guard.
//
// The two lists it compares drift apart in exactly one direction: somebody adds
// a blob store, registers it so the continuity panel looks right, and does not
// write the capture step — leaving a panel that reports on a backup nobody
// takes. That is strictly worse than having no panel line at all.
func TestEveryBlobStoreIsCaptured(t *testing.T) {
	captured := map[string]bool{}
	for _, name := range capturedBlobStores() {
		captured[name] = true
	}

	for _, store := range BlobStores() {
		if !captured[store.Name] {
			t.Errorf("blob store %q is registered but archive.go does not capture it.\n"+
				"Its contents are not in any backup. Add a capture step in archive.go and "+
				"name it in capturedBlobStores().\nWhat is lost without it: %s", store.Name, store.Why)
		}
	}
	for name := range captured {
		found := false
		for _, store := range BlobStores() {
			if store.Name == name {
				found = true
			}
		}
		if !found {
			t.Errorf("archive.go captures %q but it is not in the blobStores registry, "+
				"so the continuity panel will never mention it", name)
		}
	}
}

// TestEveryBlobStoreHasAVolumeThatIsClassified ties the two registries together:
// a store backed by a compose volume must have that volume classified, or
// TestComposeVolumesAreClassified would be satisfied by a volume nobody backs up.
func TestEveryBlobStoreHasAVolumeThatIsClassified(t *testing.T) {
	for _, store := range BlobStores() {
		if store.Volume == "" {
			continue
		}
		if _, ok := VolumeCoverage(store.Volume); !ok {
			t.Errorf("blob store %q is backed by volume %q, which is not classified in composeVolumes",
				store.Name, store.Volume)
		}
	}
}

// TestExportNoteDescribesTheMoneyInvariant pins the one promise the export
// envelope makes to a reader who no longer has this application: that a value
// which looks like money is a string and has not been through a float.
func TestExportNoteDescribesTheMoneyInvariant(t *testing.T) {
	if !strings.Contains(exportNote, "decimal string") {
		t.Error("the export note no longer tells the reader money is a decimal string; " +
			"that sentence is the export's entire contract with a future reader")
	}
}

// TestSelectExprCastsMoneyToText is the unit-level guard on the single most
// important line in the package. If numeric columns stop being cast to text in
// SQL, every monetary value in the export starts its life as a float64 in the
// driver, and no amount of care downstream gets the digits back.
func TestSelectExprCastsMoneyToText(t *testing.T) {
	got := selectExpr("amount", "numeric")
	if !strings.Contains(got, "::text") {
		t.Fatalf("numeric column is not cast to text in SQL: %q\n"+
			"JSON has one number type and it is a double. Casting in the query is what "+
			"keeps 1234.56 from becoming 1234.5599999999999 somewhere downstream.", got)
	}
	if plain := selectExpr("name", "text"); strings.Contains(plain, "::text") {
		t.Errorf("text column should not be cast: %q", plain)
	}
}

// TestBinaryColumnsAreNeverExported checks the type rule rather than the name
// list. Every bytea column in this schema is a ciphertext bearer credential, and
// the next one will be too.
func TestBinaryColumnsAreNeverExported(t *testing.T) {
	if !IsSensitive("some_future_table", "some_future_secret", "bytea") {
		t.Error("a bytea column in a table nobody has written yet is not being withheld from the export")
	}
	if !IsSensitive("users", "password_hash", "text") {
		t.Error("users.password_hash is not being withheld from the export")
	}
	if IsSensitive("transactions", "amount", "numeric") {
		t.Error("ordinary columns must not be withheld")
	}
}

// TestNormaliseKeepsStoredJSONAsJSON guards a quiet corruption: Go marshals
// []byte as base64, so a jsonb column read as bytes would land in the export as
// an opaque base64 blob that still parses as valid JSON — a corrupt export that
// looks fine.
//
// The string case is the one that actually runs, since selectExpr casts JSON
// columns to text. It is also the case that broke once: pgx decodes a JSON
// column into Go values, so a stored JSON string `"ntfy"` arrived here as the
// bare Go string `ntfy`, and wrapping that as a RawMessage produced a document
// that would not marshal at all.
func TestNormaliseKeepsStoredJSONAsJSON(t *testing.T) {
	for _, input := range []any{
		`{"channel":"ntfy"}`,
		[]byte(`{"channel":"ntfy"}`),
	} {
		out := normalise(input, "jsonb")
		raw, ok := out.(json.RawMessage)
		if !ok {
			t.Fatalf("jsonb column normalised to %T, not json.RawMessage", out)
		}
		encoded, err := json.Marshal(map[string]any{"value": raw})
		if err != nil {
			t.Fatalf("marshal %T: %v", input, err)
		}
		if !bytes.Contains(encoded, []byte(`"channel":"ntfy"`)) {
			t.Errorf("stored JSON did not survive the export encoder: %s", encoded)
		}
	}

	// A NULL jsonb must become null, not an empty RawMessage — an empty
	// RawMessage is not valid JSON and fails the whole export at marshal time.
	for _, empty := range []any{nil, "", []byte(nil)} {
		if out := normalise(empty, "jsonb"); out != nil {
			t.Errorf("empty jsonb normalised to %#v, want nil", out)
		}
	}
	if _, err := json.Marshal(map[string]any{"v": normalise(nil, "jsonb")}); err != nil {
		t.Errorf("a NULL jsonb column breaks the export: %v", err)
	}
}

// TestSelectExprPassesStoredJSONThroughAsText pins the fix for the float
// coercion described in selectExpr. transactions.raw is Plaid's payload and is
// full of JSON numbers; decoding and re-encoding those through float64 changes
// literals, in the app's most safety-critical column type.
func TestSelectExprPassesStoredJSONThroughAsText(t *testing.T) {
	got := selectExpr("raw", "jsonb")
	if !strings.Contains(got, "::text") {
		t.Errorf("jsonb column is not taken as text: %q — numbers inside stored "+
			"documents will round-trip through float64", got)
	}
}

// TestEveryInExportTableHasAnExporter is the DB-gated completeness check: it
// runs the real exporter against a migrated database and asserts every table the
// registry classifies InExport actually produced a section.
//
// Classifying a table InExport and then failing to serialise it is the same bug
// as not classifying it at all, one step later — and it is the more likely of
// the two, because the classification is the part a reviewer notices.
//
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' \
//	  go test ./internal/continuity/
func TestEveryInExportTableHasAnExporter(t *testing.T) {
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

	export, err := (&Exporter{Pool: pool}).Build(ctx)
	if err != nil {
		t.Fatalf("build export: %v", err)
	}

	for _, table := range TablesWithCoverage(InExport) {
		if _, ok := export.Tables[table]; !ok {
			t.Errorf("table %q is classified InExport but the export produced no section for it.\n"+
				"It is in the pg_dump and absent from the portable export, which is the artefact "+
				"meant to outlive this application.", table)
		}
	}

	// No value anywhere may encode as an array of numbers.
	//
	// That is the signature of a Go byte array reaching the encoder, which is
	// what pgx's generic row scan hands back for a uuid — and it would have
	// turned every primary and foreign key in the export into an unreadable
	// list of integers. Checked structurally rather than per-column so a column
	// type nobody has used yet is covered by the same rule.
	assertNoByteArrays(t, export)

	if export.Meta.SchemaVersion != ExportSchemaVersion {
		t.Errorf("export schema version = %d, want %d", export.Meta.SchemaVersion, ExportSchemaVersion)
	}
	if export.Meta.MigrationVersion == 0 {
		t.Error("export did not record a migration version, so a reader cannot tell which schema it describes")
	}

	// Nothing classified otherwise may appear. An export that quietly carried
	// plaid_items would be shipping access tokens in a plain file.
	for table := range export.Tables {
		if c, _ := Classify(table); c != InExport {
			t.Errorf("table %q is classified %s but appears in the portable export", table, c)
		}
	}
}

// TestExportedMoneyIsAlwaysAString round-trips a real export and asserts every
// numeric column arrived as a JSON string, decoding with UseNumber so a bare
// number cannot be silently re-rendered as one on the way out.
func TestExportedMoneyIsAlwaysAString(t *testing.T) {
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

	// A value chosen to be exactly representable as a decimal and not as a
	// float64: if anything in the path coerces, the digits change visibly.
	const amount = "12345678.91"
	var householdID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO households (name) VALUES ('export-decimal-test') RETURNING id::text`,
	).Scan(&householdID); err != nil {
		t.Fatalf("seed household: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id = $1::uuid`, householdID)
	})
	if _, err := pool.Exec(ctx,
		`INSERT INTO manual_assets (household_id, name, kind, value, as_of)
		 VALUES ($1::uuid, 'export decimal probe', 'other', $2::numeric, CURRENT_DATE)`,
		householdID, amount); err != nil {
		t.Fatalf("seed manual asset: %v", err)
	}

	// A stored JSON document containing numbers must survive byte-exact. This
	// is the transactions.raw case: Plaid payloads are full of JSON numbers,
	// and a decode/re-encode through float64 rewrites the literals — an integer
	// past 2^53 and a repeating binary fraction both change visibly.
	const storedJSON = `{"big": 12345678901234567890, "frac": 0.1}`
	if _, err := pool.Exec(ctx,
		`INSERT INTO net_worth_snapshots
		   (household_id, as_of, assets_total, liabilities_total, net_worth, breakdown)
		 VALUES ($1::uuid, CURRENT_DATE, 1, 0, 1, $2::jsonb)`,
		householdID, storedJSON); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}

	export, err := (&Exporter{Pool: pool}).Build(ctx)
	if err != nil {
		t.Fatalf("build export: %v", err)
	}
	encoded, err := json.Marshal(export)
	if err != nil {
		t.Fatalf("marshal export: %v", err)
	}

	// Decode with UseNumber so numbers stay textual and a float coercion shows
	// up as a changed literal rather than being hidden by float64 round-tripping.
	dec := json.NewDecoder(bytes.NewReader(encoded))
	dec.UseNumber()
	var decoded struct {
		Tables map[string][]map[string]any `json:"tables"`
	}
	if err := dec.Decode(&decoded); err != nil {
		t.Fatalf("decode export: %v", err)
	}

	found := false
	for _, row := range decoded.Tables["manual_assets"] {
		// Scoped to this test's own household: the export is instance-wide, so
		// matching on the name alone would pick up a row left behind by an
		// earlier run and make the result depend on test history.
		if row["household_id"] != householdID {
			continue
		}
		found = true

		got, ok := row["value"].(string)
		if !ok {
			t.Fatalf("manual_assets.value came back as %T (%v), not a string — "+
				"a monetary value has been through a JSON number", row["value"], row["value"])
		}
		// Compared as decimals, not as text. The column is numeric(20,4), so
		// Postgres renders "12345678.9100" — trailing zeros are the column's
		// declared scale, not a change to the value. What must not happen is a
		// change in the digits themselves, which is what a float round-trip does.
		want := decimal.RequireFromString(amount)
		gotDec, err := decimal.NewFromString(got)
		if err != nil {
			t.Fatalf("manual_assets.value = %q, which is not a decimal at all", got)
		}
		if !gotDec.Equal(want) {
			t.Errorf("manual_assets.value = %q, want %q — the digits changed in transit", got, amount)
		}
	}
	if !found {
		t.Fatal("seeded row did not appear in the export")
	}

	// And nothing anywhere in the export may be a bare JSON number where the
	// column is numeric.
	if bytes.Contains(encoded, []byte(`"value":12345678.91`)) {
		t.Error("a numeric column was encoded as a bare JSON number")
	}

	// The stored JSON document's numbers must be byte-identical. float64 turns
	// 12345678901234567890 into 12345678901234567000 and 0.1 into
	// 0.1000000000000000055511151231257827, so either literal surviving proves
	// the bytes were never parsed.
	if !bytes.Contains(encoded, []byte("12345678901234567890")) {
		t.Errorf("a large integer inside a stored JSON column was rewritten — " +
			"stored documents are being decoded and re-encoded through float64")
	}
	if bytes.Contains(encoded, []byte("12345678901234567000")) {
		t.Error("a stored JSON integer was rounded by a float64 round-trip")
	}
}

// assertNoByteArrays walks an encoded export and fails on any value that came
// out as an array of bare numbers.
//
// This is a type-shaped guard rather than a column-shaped one, deliberately.
// The uuid case that prompted it was invisible in review — the code read
// correctly and the export was valid JSON; it was only wrong in the sense that
// no human could use it. Any future column type whose Go representation is a
// byte array fails the same way, so the check is written against the shape
// rather than against the list of types that have caused it so far.
func assertNoByteArrays(t *testing.T, export *Export) {
	t.Helper()

	encoded, err := json.Marshal(export)
	if err != nil {
		t.Fatalf("marshal export: %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(encoded, &generic); err != nil {
		t.Fatalf("unmarshal export: %v", err)
	}

	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch t2 := v.(type) {
		case map[string]any:
			for k, child := range t2 {
				walk(path+"."+k, child)
			}
		case []any:
			// A row list is fine. An array whose every element is a number is
			// not — no column in this schema is a list of bare numbers, so this
			// can only be a Go byte array that reached the encoder.
			if len(t2) > 0 {
				allNumbers := true
				for _, e := range t2 {
					if _, ok := e.(float64); !ok {
						allNumbers = false
						break
					}
				}
				if allNumbers {
					t.Errorf("%s encoded as an array of %d numbers — a Go byte array "+
						"reached the encoder. Cast this column to text in selectExpr.", path, len(t2))
					return
				}
			}
			for i, child := range t2 {
				walk(fmt.Sprintf("%s[%d]", path, i), child)
			}
		}
	}
	walk("export", generic)
}
