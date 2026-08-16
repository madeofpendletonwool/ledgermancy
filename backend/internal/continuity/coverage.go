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
	// Savings jars and their append-only deposit/withdraw log. The jar is an
	// annotation the household invented over part of an account balance — no
	// bank knows it exists, so nothing outside this table could name it or say
	// how much of the account it claims. piggy_bank_events is the only record of
	// how a jar reached its balance; without it a restored jar is a number with
	// no history behind it.
	"piggy_banks":       InExport,
	"piggy_bank_events": InExport,
	// Free-form labels and what they are stuck to. Every row is a household
	// judgement about what a charge was FOR, which no bank holds and no re-sync
	// can re-derive: the trip that "Summer Vacation" names exists nowhere
	// outside this table. transaction_tags is the half that carries the meaning
	// — without it the tags restore as a list of names attached to nothing, and
	// every envelope total reads as zero.
	"tags":             InExport,
	"transaction_tags": InExport,
	// Typed connections between two transactions, and the vocabulary of
	// relationships they are drawn from. Every link is a household judgement
	// ("this credit refunds that charge") that no bank records and no re-sync
	// could reconstruct — the two rows come back from Plaid as unrelated
	// forever. link_types is InExport rather than DumpOnly for the same reason
	// `categories` is: it holds the seeded rows AND whatever the household
	// invented on top, and exporting only half of it would restore links whose
	// relationship has no name.
	"link_types":        InExport,
	"transaction_links": InExport,
	// User-written IF-THEN automation, and the two child tables that ARE the
	// rule. A rule is pure household judgement — "charges from this merchant
	// over ten dollars are Coffee, and tag them" — that nothing outside this
	// database has ever seen, so no re-sync and no recomputation brings one
	// back. The children are not detail: a rules row on its own restores as a
	// name with no conditions and no actions, and the engine reads a
	// condition-less rule as matching NOTHING. Losing them turns every
	// automation the household built into an inert list, silently.
	"rules":         InExport,
	"rule_triggers": InExport,
	"rule_actions":  InExport,
	// The per-object change log: who edited a transaction/budget/goal and how.
	// Every row is a hand-edit the household made — nothing a Plaid re-sync or a
	// recomputation can ever reconstruct — so losing it loses the audit trail
	// behind every correction in the ledger.
	"object_changes": InExport,
	"preferences":    InExport,

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
	// The class-specific facts about an asset — a bond's series and issue date,
	// a car's model year and odometer. Typed in by hand, and without them a
	// restored bond cannot be valued at all: it falls back to a frozen number,
	// which is the exact condition doc 26 exists to remove.
	"asset_details": InExport,
	// Every dated valuation. manual_assets.value carries only the CURRENT
	// figure, so losing this loses the trend behind it permanently — the same
	// reasoning as net_worth_snapshots directly above. A house's five-year
	// appreciation cannot be reconstructed from its present value.
	"asset_valuations": InExport,
	// The same argument as asset_valuations, one table over. accounts.current_-
	// balance holds only what a manual account is worth TODAY; this is every
	// balance the household ever recorded for it, and for an account Plaid
	// cannot reach there is no sync that could rebuild a single row of it. It is
	// also the audit trail for the scheduled-contribution worker — a
	// reason='scheduled' row is the only record of why a balance moved.
	"account_balance_history": InExport,
	// account_terms is the sharper half of the pair above it. liabilities is a
	// mirror of Plaid and a resync rebuilds it; account_terms is the APR and the
	// monthly payment a person typed in, for the majority of institutions Plaid
	// declines to report terms for at all. Nothing anywhere can re-supply it.
	"account_terms":          InExport,
	"projection_assumptions": InExport,

	// --- Recurring and obligations ----------------------------------------
	"recurring_obligations": InExport,
	"recurring_overrides":   InExport,
	// Per-occurrence "this bill was paid" records (MAD-85). Every row is a
	// member's manual mark — the matcher reads transactions on the fly and
	// writes nothing — so nothing a re-sync does can re-derive a row. Losing it
	// re-arms every reminder the household had laid to rest.
	"obligation_satisfaction": InExport,

	// --- Anomaly detection ------------------------------------------------
	// The same kind of thing as recurring_overrides: the record of a household
	// saying "this merchant is fine, stop telling me". Nothing can re-derive it,
	// and losing it means making every dismissal again, one merchant at a time,
	// as the detectors re-raise all of them on the next sweep.
	"anomaly_overrides": InExport,

	// --- Allowances -------------------------------------------------------
	"allowances":        InExport,
	"allowance_entries": InExport,

	// --- Documents --------------------------------------------------------
	// Metadata only. The bytes live in a blob store that pg_dump cannot see —
	// see blobStores below, which is why that registry exists at all.
	"documents":      InExport,
	"document_links": InExport,

	// --- Payroll ----------------------------------------------------------
	// Sharper than it first looks. A paystub is not re-syncable from anywhere:
	// Plaid reports the deposit, never the withholding behind it, and an
	// employer's self-service portal keeps two years at best and vanishes with
	// the job. Every line was typed in or read off a PDF the household still has
	// to find again, and the YTD figures a tax summary is built from cannot be
	// reconstructed from the ledger at any price.
	//
	// The EIN column is bytea, so IsSensitive withholds it from the portable
	// export by type while the dump still carries it — the same treatment as
	// every other sealed value, and the reason that rule is a type rule.
	"employers":     InExport,
	"paystubs":      InExport,
	"paystub_lines": InExport,

	// --- Alerts -----------------------------------------------------------
	// The rules are the user's; the events are the engine's output.
	"alerts": InExport,

	// --- Advisor surface --------------------------------------------------
	// A saved conversation is user-authored and re-derivable from nothing: the
	// household's own questions, in their own words, and the reasoning they were
	// answered with.
	//
	// advisor_messages is the sharpest case in this block and the asymmetry is
	// deliberate. content and tool_trace are BYTEA, so IsSensitive withholds them
	// from the PORTABLE export by type — the rows travel, the sealed text does
	// not. That is the right default: the export is a plain JSON file a user may
	// email themselves, and a full advisor transcript, which is a household
	// narrating its salary and its debts in natural language, is the last thing
	// that should ride in one. The encrypted bytes are still in the pg_dump, so a
	// restore under the same ENCRYPTION_KEY recovers them intact.
	//
	// Keep that asymmetry in the restore runbook: a portable export restores a
	// household's threads with empty bodies, a dump brings them back whole.
	"advisor_threads":  InExport,
	"advisor_messages": InExport,
	// The record of what the household decided to DO. Nothing re-derives a
	// decision; re-running the ranker produces today's options, not the one
	// somebody accepted in March and is still working through.
	"advisor_action_items": InExport,

	// --- Allocation planner -----------------------------------------------
	// A saved plan is a USER-AUTHORED DECISION about where money should go, and
	// nothing re-derives it: re-running the allocator produces the split the
	// user is looking at today, not the one they settled on in March and are
	// still working through. Results are deliberately not stored (they are
	// recomputed against the live baseline on open), so what travels here is
	// exactly the inputs and the assumptions snapshot — which is the part that
	// cannot be reconstructed.
	//
	// Money inside `inputs` and `assumptions` is a decimal STRING rather than a
	// JSON number, because this is the one place the export's numeric-to-text
	// rule cannot reach: normalise passes jsonb through as json.RawMessage. See
	// allocation/store.go, which is where that rule is enforced.
	"allocation_plans": InExport,

	// The household's tracked decisions (doc 33): one snapshot per plan per
	// date, recording what the plan EXPECTED to have gone in by then.
	//
	// InExport rather than Derived, and the distinction is the whole reason this
	// table stores only the expected side. Actuals are read live and are
	// genuinely recomputable; the expected side is not, because replaying it
	// means running the plan's inputs against assumptions the household can
	// edit. A tracking history regenerated after an assumption change would
	// silently rewrite what the plan used to say — which is the one thing a
	// plan-vs-actual comparison must never do.
	//
	// expected_lump and expected_total are ordinary numeric COLUMNS, so
	// export.go's numeric-to-text cast covers them. Money inside
	// snapshot_inputs is a decimal STRING for the same reason as
	// allocation_plans above.
	"plan_trackings": InExport,

	// --- Financial plan ----------------------------------------------------
	// The household's authored INTENT (MAD-258): the strategy prose, the
	// per-person notes, and the append-only decisions log. Nothing outside this
	// database has ever seen any of it — no bank holds "why we hold the
	// emergency fund at three months", and no re-sync or recomputation can
	// re-derive a sentence the household wrote about its own life. Losing these
	// tables loses the plan outright.
	//
	// body columns are BYTEA, so the portable export withholds them by type for
	// the same reason it withholds advisor transcripts — plan prose is the most
	// sensitive text in the house, and the pg_dump still recovers it whole
	// under the same ENCRYPTION_KEY.
	"plan_sections":  InExport,
	"plan_decisions": InExport,
	// The review stamp lives on households (classified above); it is named
	// here only in passing because the plan_stale producer reads it as the
	// "keep this updated" signal.

	// --- Digests ----------------------------------------------------------
	// InExport, and NOT Derived, which is the tempting wrong answer: a digest
	// looks like something a job produces. But the job cannot produce it again.
	// Each entry is a snapshot of figures as they stood in a past period, and
	// the transactions behind it have since been recategorised, corrected and
	// added to. Re-running the sweep against today's data would write a
	// different digest, not the same one — so a lost entry is lost history.
	"digest_entries": InExport,

	// --- Credentials: dump only, never a plain-JSON file ------------------
	"plaid_items":         DumpOnly, // access tokens, sealed with ENCRYPTION_KEY
	"user_recovery_codes": DumpOnly,
	// Personal API tokens. DumpOnly rather than InExport because every row is a
	// credential digest, and the export is a plain-JSON file meant to outlive
	// this app — a hash belongs in neither. DumpOnly rather than Ephemeral,
	// which is where `sessions` sits, because a token is not a browser's: the
	// user minted it deliberately, it survives every sign-out on purpose, and a
	// restore that silently broke every integration they had wired up is not
	// what anyone means by restoring their data.
	"api_tokens": DumpOnly,
	// Outgoing webhook subscriptions. Every row holds a sealed signing secret, so
	// it cannot go into a plain-JSON file meant to outlive this app — and like
	// api_tokens directly above, it is emphatically not Ephemeral: the household
	// configured these deliberately, and a restore that silently stopped feeding
	// every automation they had wired up is not what anybody means by restoring
	// their data.
	"webhooks": DumpOnly,

	// --- Operational bookkeeping ------------------------------------------
	"auth_events":       DumpOnly, // audit log, swept at 180 days
	"digest_deliveries": DumpOnly, // per-period dedupe keys
	"backup_runs":       DumpOnly, // this package's own state
	"asset_prices":      DumpOnly, // cache of third-party closes, refetchable
	// Published Treasury rates, seeded by migration 00051 — a fresh schema has
	// them. DumpOnly rather than InExport because none of it is the
	// household's: it is public reference data, identical in every install, and
	// the portable export exists to carry what is theirs. A household that
	// edited a row keeps that edit through the dump, which is the restore path
	// that matters.
	"savings_bond_rates": DumpOnly,
	// CPI-U, seeded by migration 00052 — a fresh schema already has the whole
	// series. Same reasoning as savings_bond_rates: public reference data,
	// identical in every install, so it rides along in the dump but stays out of
	// the portable export, which exists to carry what is the household's.
	"cpi_series":       DumpOnly,
	"pfc_category_map": DumpOnly, // seeded by migration 00002; a fresh schema has it

	// --- Derived: a job rebuilds these ------------------------------------
	"alert_events":      Derived, // alerts.Evaluate, on the next sweep
	"insights":          Derived, // insights.Generate, on the next sweep
	"monthly_summaries": Derived, // jobs.SummaryRefreshWorker, per completed month
	// jobs.FetchMerchantLogosWorker, on the next daily sweep. Derived rather
	// than DumpOnly on purpose: it IS in the dump, so a restore does not spend
	// a fresh round of third-party requests re-fetching what it already had —
	// but nothing in it is the household's, so it stays out of the portable
	// export, which exists to outlive this app. Public brand imagery is the
	// last thing worth carrying into a spreadsheet.
	"merchant_logos": Derived,

	// The structural transfer pairer (categorize.PairAllHouseholds, and the
	// post-sync pass) rebuilds every row from transactions, which are InExport
	// above. The match is a pure function of amount/date/account and is
	// deterministic, so a restore converges to the same pairs — and a manual
	// re-categorisation on either leg survives the rebuild (manual legs are not
	// candidates), which is the one way a household overrules a pair.
	"transaction_pairs": Derived,

	// --- Ephemeral --------------------------------------------------------
	"sessions":       Ephemeral, // restoring these would resurrect logins
	"mfa_challenges": Ephemeral, // seconds-lived by design
	// Outgoing webhook delivery history. Ephemeral rather than DumpOnly, and the
	// distinction from `webhooks` above is worth being explicit about: the
	// SUBSCRIPTION is configuration and must come back, but these are a record of
	// requests that already happened, collected on a thirty-day retention.
	// Restoring a week-old `pending` row would hand the sweep a backlog of events
	// the household has long since seen in the app and re-deliver them to a live
	// automation — a restore that sets off somebody's lights is a bug, not a
	// recovery.
	"webhook_messages": Ephemeral,
	"webhook_attempts": Ephemeral,
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
