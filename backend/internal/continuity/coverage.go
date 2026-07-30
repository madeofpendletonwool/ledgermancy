// Package continuity is the app's answer to "what happens when the disk dies".
//
// It owns the scheduled database dump, the document-vault archive, the portable
// JSON export, and the restore test that proves the first three are worth
// something. It also owns the registry below, which is the part that matters
// most in six months.
//
// The failure this package exists to prevent is not "the backup did not run".
// That one is loud. It is "the backup ran, and the table added three months ago
// was never in it" — which is silent until a restore, at which point it is
// permanent. The registry turns that class of mistake into a failing test on
// the pull request that would have introduced it.
package continuity

import (
	"fmt"
	"sort"
	"strings"
)

// Coverage answers one question, asked of every table in the schema: if this
// database were lost, what brings this table's contents back?
//
// Every table must have an answer. There is no default and no "unclassified"
// value, because a default would silently absorb every table a future feature
// adds — and "fails open" is not an acceptable posture for the one subsystem
// whose failure mode is permanent data loss.
type Coverage int

const (
	// InExport is user data: things the household created, decided, or
	// accumulated, which no amount of re-syncing would bring back. Captured by
	// both the pg_dump and the portable JSON export.
	//
	// This is also the set the restore test row-counts, so classifying a table
	// here is what enrols it in verification. Nothing else needs editing.
	InExport Coverage = iota

	// DumpOnly is captured by the pg_dump and deliberately left out of the
	// portable export. Two kinds of thing land here: credentials, which must
	// never be written to a plain-JSON file (Plaid tokens, password hashes,
	// recovery codes), and app-internal bookkeeping that means nothing outside
	// this application.
	//
	// The distinction is the whole point of having two artefacts. The dump
	// restores *this app*; the export outlives it.
	DumpOnly

	// Derived is recomputable from InExport tables by a job that already
	// exists. Restoring it is a convenience — it saves a re-run, not the data.
	// Each entry below names the job, so the claim can be checked rather than
	// trusted.
	Derived

	// Ephemeral is state where restoring it would be actively wrong, not merely
	// unnecessary: live sessions, half-finished MFA challenges, queue rows
	// describing work that already happened.
	Ephemeral
)

func (c Coverage) String() string {
	switch c {
	case InExport:
		return "in-export"
	case DumpOnly:
		return "dump-only"
	case Derived:
		return "derived"
	case Ephemeral:
		return "ephemeral"
	}
	return fmt.Sprintf("Coverage(%d)", int(c))
}

// tableCoverage classifies every table in the schema.
//
// This is an allowlist, not a blocklist, and the difference is the entire
// design. A blocklist would mean any table added by a future migration is
// treated as covered until somebody remembers to say otherwise — which is
// exactly the mistake this package exists to make impossible.
//
// Adding a table to a migration and not adding it here fails
// TestEveryTableIsClassified. That is not an inconvenience to work around; it
// is the guard doing its job. Pick the category that is true and move on.
var tableCoverage = map[string]Coverage{
	// --- Identity and household structure ---------------------------------
	"households":        InExport,
	"users":             InExport, // minus the credential columns; see sensitiveColumns
	"household_people":  InExport,
	"household_invites": DumpOnly, // pending invites; meaningless once expired

	// --- Ledger core ------------------------------------------------------
	// Everything a household typed, corrected, or categorised by hand. Plaid
	// can re-supply at most the trailing link window, so treating any of this
	// as re-syncable is how a year of history quietly disappears.
	"accounts":              InExport,
	"transactions":          InExport,
	"transaction_splits":    InExport,
	"categories":            InExport,
	"category_rules":        InExport,
	"merchant_category_map": InExport,
	"budgets":               InExport,
	"goals":                 InExport,
	"goal_contributions":    InExport,
	"preferences":           InExport,

	// --- Merchants --------------------------------------------------------
	// merchant_merge_rejections is user work, not bookkeeping: it is the record
	// of every "no, these are different merchants" decision. Lose it and the
	// suggestion queue re-proposes all of them forever.
	"merchant_entities":         InExport,
	"merchant_aliases":          InExport,
	"merchant_merge_rejections": InExport,

	// --- Net worth and investments ----------------------------------------
	// The snapshot tables are the sharpest case in the schema. Neither Plaid
	// nor any brokerage keeps a history of what a portfolio was worth on a
	// given day, so a day that is not recorded here can never be recovered by
	// anything, at any price.
	"net_worth_snapshots":     InExport,
	"investment_snapshots":    InExport,
	"holdings":                InExport,
	"securities":              InExport,
	"investment_transactions": InExport,
	"liabilities":             InExport,
	"manual_assets":           InExport,
	"account_contributions":   InExport,
	// account_terms is the sharper half of the pair above it. liabilities is a
	// mirror of Plaid and a resync rebuilds it; account_terms is the APR and the
	// monthly payment a person typed in, for the majority of institutions Plaid
	// declines to report terms for at all. Nothing anywhere can re-supply it.
	"account_terms": InExport,
	"projection_assumptions":  InExport,

	// --- Recurring and obligations ----------------------------------------
	"recurring_obligations": InExport,
	"recurring_overrides":   InExport,

	// --- Allowances -------------------------------------------------------
	"allowances":        InExport,
	"allowance_entries": InExport,

	// --- Documents --------------------------------------------------------
	// Metadata only. The bytes live in a blob store that pg_dump cannot see —
	// see blobStores below, which is why that registry exists at all.
	"documents":      InExport,
	"document_links": InExport,

	// --- Alerts -----------------------------------------------------------
	// The rules are the user's; the events are the engine's output.
	"alerts": InExport,

	// --- Credentials: dump only, never a plain-JSON file ------------------
	"plaid_items":         DumpOnly, // access tokens, sealed with ENCRYPTION_KEY
	"user_recovery_codes": DumpOnly,

	// --- Operational bookkeeping ------------------------------------------
	"auth_events":       DumpOnly, // audit log, swept at 180 days
	"digest_deliveries": DumpOnly, // per-period dedupe keys
	"backup_runs":       DumpOnly, // this package's own state
	"asset_prices":      DumpOnly, // cache of third-party closes, refetchable
	"pfc_category_map":  DumpOnly, // seeded by migration 00002; a fresh schema has it

	// --- Derived: a job rebuilds these ------------------------------------
	"alert_events":      Derived, // alerts.Evaluate, on the next sweep
	"insights":          Derived, // insights.Generate, on the next sweep
	"monthly_summaries": Derived, // jobs.SummaryRefreshWorker, per completed month

	// --- Ephemeral --------------------------------------------------------
	"sessions":       Ephemeral, // restoring these would resurrect logins
	"mfa_challenges": Ephemeral, // seconds-lived by design
}

// runtimeTablePrefixes covers tables no migration in this repo creates: River
// owns and versions its own schema (jobs.Migrate), and goose owns its version
// table. They only show up in the live-database cross-check, never in the
// migration parse, but they still need an answer — and the answer is that
// restoring a queue row describing work that already ran is a bug.
var runtimeTablePrefixes = map[string]Coverage{
	"river_":           Ephemeral,
	"goose_db_version": Ephemeral,
}

// Classify returns a table's coverage. The second result is false for a table
// nobody has classified, which is the condition the guard test fails on.
func Classify(table string) (Coverage, bool) {
	if c, ok := tableCoverage[table]; ok {
		return c, true
	}
	for prefix, c := range runtimeTablePrefixes {
		if strings.HasPrefix(table, prefix) {
			return c, true
		}
	}
	return 0, false
}

// TablesWithCoverage returns the tables in one class, sorted.
//
// The export walks InExport through this, and so does the restore test's
// row-count check. That is deliberate and is what makes the registry pay for
// itself: a table added in a later wave and classified InExport is dumped,
// exported, and restore-verified without anyone editing three more files.
func TablesWithCoverage(c Coverage) []string {
	var out []string
	for table, got := range tableCoverage {
		if got == c {
			out = append(out, table)
		}
	}
	sort.Strings(out)
	return out
}

// ClassifiedTables returns every table in the registry, sorted.
func ClassifiedTables() []string {
	out := make([]string, 0, len(tableCoverage))
	for table := range tableCoverage {
		out = append(out, table)
	}
	sort.Strings(out)
	return out
}

// sensitiveColumns are dropped from the portable export even though their table
// is InExport.
//
// A column-level exclusion rather than demoting the whole table: a household's
// user list — who exists, their names, their roles — is real data worth
// carrying forward, and losing it to protect a password hash would be a poor
// trade. The hash itself is worth nothing outside this app and everything to
// somebody who finds the file.
//
// This list only has to name secrets held as *text*. Every binary column is
// excluded by type — see IsSensitive — which is the rule that will still be
// correct after a wave nobody has planned yet.
var sensitiveColumns = map[string]map[string]bool{
	"users": {
		"password_hash": true,
	},
}

// IsSensitive reports whether a column must be withheld from the export.
//
// The `bytea` rule is the load-bearing half and is a type rule rather than a
// name rule on purpose. Every binary column in this schema is a ciphertext
// bearer credential — Plaid access tokens, TOTP secrets — and the next one will
// be too, because that is what the column type is used for here. A named
// allowlist would protect the two that exist and quietly export the third.
//
// It is also correct on its own terms: a sealed blob is meaningless in a
// portable JSON file, so excluding it costs the export nothing.
func IsSensitive(table, column, dataType string) bool {
	if dataType == "bytea" {
		return true
	}
	return sensitiveColumns[table][column]
}

// --------------------------------------------------------------------------
// Durable state that is not in Postgres
// --------------------------------------------------------------------------

// BlobStore is durable state that pg_dump cannot reach.
//
// There is exactly one today, and a registry of one looks like overkill. It is
// not: the document vault was itself the second durable thing this app grew,
// and it was nearly shipped without a backup story because the backup story was
// written when the database was the only thing that existed. The registry is
// here so the *third* one cannot repeat that, and so the continuity panel lists
// it whether or not anyone remembered to add a line to the UI.
type BlobStore struct {
	// Name matches the run kind that captures it, so a panel row and a
	// backup_runs row line up without a translation table.
	Name string
	// Volume is the compose volume backing the default (local) deployment.
	// Empty when the store has no on-host form.
	Volume string
	// Why explains, in one sentence, what is lost if this store is missing
	// from a restore. It is shown to the operator verbatim.
	Why string
}

// blobStores is the registry. Adding a blob store to the app means adding a
// line here and a capture step in archive.go; TestEveryBlobStoreIsCaptured
// fails until both exist.
var blobStores = []BlobStore{
	{
		Name:   "documents",
		Volume: "documents-data",
		Why: "Document contents. The database keeps every title, type and expiry " +
			"and none of the bytes, so a database-only restore produces a vault " +
			"full of entries that cannot be opened.",
	},
}

// BlobStores returns the registry.
func BlobStores() []BlobStore {
	out := make([]BlobStore, len(blobStores))
	copy(out, blobStores)
	return out
}

// composeVolumes says how each named volume in docker-compose.yml is covered.
//
// Guarded by TestComposeVolumesAreClassified, which parses the compose file. A
// wave-5 feature that adds a volume — a cache, an upload staging area, a search
// index — fails the build until somebody has decided whether losing it matters.
// Deciding "it does not matter" is a perfectly good answer; not deciding is not.
var composeVolumes = map[string]string{
	"postgres-data":  "Captured by the scheduled pg_dump (kind db_dump).",
	"documents-data": "Captured by the scheduled vault archive (kind documents_archive).",
	"backup-data":    "Holds the backups themselves; mirrored to BACKUP_MIRROR_DIR when set.",
}

// VolumeCoverage explains how a compose volume is covered, or reports false for
// one nobody has classified.
func VolumeCoverage(volume string) (string, bool) {
	how, ok := composeVolumes[volume]
	return how, ok
}
