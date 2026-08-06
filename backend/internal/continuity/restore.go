package continuity

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/config"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/crypto"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// The restore test is the highest-value thing in this package, and the reason
// is uncomfortable: an untested backup is not a backup, it is a belief about a
// backup. Everything else here produces artefacts. This is the only part that
// produces evidence.
//
// It is also the only part that can fail in a way the operator wants to know
// about *today* rather than during an outage, which is why its result gets its
// own line in the panel and its own colour.

// RestoreTester restores the latest dump into a scratch database and checks it.
type RestoreTester struct {
	Pool        *pgxpool.Pool
	Queries     *dbgen.Queries
	DatabaseURL string
	Cipher      *crypto.Cipher
	BinDir      string
}

// RestoreReport is the human-readable outcome, written to backup_runs.detail.
type RestoreReport struct {
	Dump          string
	ScratchDB     string
	TablesChecked int
	RowsRestored  int64
	// Problems is empty on success. Each entry is written for an operator
	// reading a web page at an unpleasant hour, not for a log parser.
	Problems []string
	// Drift is tables that gained rows since the dump was taken, as
	// "table live->restored". Expected, never a failure — it is how old the
	// dump is, expressed in the only units that matter.
	Drift []string
	// DocumentChecked names the document opened end-to-end, when there was one.
	DocumentChecked string
	// AdvisorMessageChecked names the advisor transcript turn opened out of the
	// dump, when there was one. Its counterpart to DocumentChecked: the same
	// dump + cipher + key agreement, one table over.
	AdvisorMessageChecked string
}

func (r RestoreReport) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "restored %s into %s: %d tables, %d rows",
		r.Dump, r.ScratchDB, r.TablesChecked, r.RowsRestored)
	if r.DocumentChecked != "" {
		fmt.Fprintf(&b, "; opened document %s end-to-end (dump + archive + key all agree)", r.DocumentChecked)
	}
	if r.AdvisorMessageChecked != "" {
		fmt.Fprintf(&b, "; opened advisor message %s from the dump (dump + key agree)", r.AdvisorMessageChecked)
	}
	if len(r.Drift) > 0 {
		fmt.Fprintf(&b, "\nrows added since the dump was taken: %s", strings.Join(r.Drift, ", "))
	}
	for _, p := range r.Problems {
		fmt.Fprintf(&b, "\n- %s", p)
	}
	return b.String()
}

// Run restores the newest dump and verifies it. The returned report is worth
// recording whether or not the error is nil — a failure's detail is the part an
// operator needs.
func (rt *RestoreTester) Run(ctx context.Context, root, archiveRoot string) (RestoreReport, error) {
	report := RestoreReport{}

	dump, ok, err := Latest(root, KindDBDump)
	if err != nil {
		return report, fmt.Errorf("find latest dump: %w", err)
	}
	if !ok {
		return report, fmt.Errorf("no dump to restore: nothing has been backed up yet")
	}
	report.Dump = dump.Path

	scratch := config.ScratchDBPrefix() + fmt.Sprint(time.Now().UnixNano())
	report.ScratchDB = scratch

	if err := rt.createScratch(ctx, scratch); err != nil {
		return report, err
	}
	// Dropped on every path, including a panic in the checks below. A scratch
	// database left behind is a full disk in a month.
	defer func() {
		if err := rt.dropScratch(context.WithoutCancel(ctx), scratch); err != nil {
			// Deliberately not folded into the returned error: the restore
			// result is what the operator asked about, and losing it behind a
			// cleanup failure would be the wrong trade.
			report.Problems = append(report.Problems,
				fmt.Sprintf("could not drop scratch database %s: %v (drop it by hand)", scratch, err))
		}
	}()

	if err := rt.restore(ctx, scratch, dump.Path); err != nil {
		return report, err
	}

	scratchPool, err := pgxpool.New(ctx, rt.urlFor(scratch))
	if err != nil {
		return report, fmt.Errorf("connect to scratch database: %w", err)
	}
	defer scratchPool.Close()

	if err := rt.checkMigrationVersion(ctx, scratchPool, &report); err != nil {
		return report, err
	}
	if err := rt.checkRowCounts(ctx, scratchPool, &report); err != nil {
		return report, err
	}
	if archiveRoot != "" {
		rt.checkDocument(ctx, scratchPool, archiveRoot, &report)
	}
	rt.checkAdvisorMessage(ctx, scratchPool, &report)

	if len(report.Problems) > 0 {
		return report, fmt.Errorf("restore verification found %d problem(s)", len(report.Problems))
	}
	return report, nil
}

// checkMigrationVersion asserts the restored schema is the one this code
// expects. A dump taken before a migration restores into a database the current
// binary cannot run against, and finding that out here is much cheaper than
// finding it out during a real restore.
func (rt *RestoreTester) checkMigrationVersion(ctx context.Context, scratch *pgxpool.Pool, report *RestoreReport) error {
	var live, restored int64
	if err := rt.Pool.QueryRow(ctx,
		`SELECT COALESCE(max(version_id), 0) FROM goose_db_version`).Scan(&live); err != nil {
		return fmt.Errorf("read live migration version: %w", err)
	}
	if err := scratch.QueryRow(ctx,
		`SELECT COALESCE(max(version_id), 0) FROM goose_db_version`).Scan(&restored); err != nil {
		report.Problems = append(report.Problems,
			fmt.Sprintf("the restored database has no usable goose_db_version table (%v), so its schema version is unknown", err))
		return nil
	}
	if live != restored {
		report.Problems = append(report.Problems, fmt.Sprintf(
			"schema version mismatch: the live database is at migration %d, the dump restores to %d. "+
				"The dump predates a migration, so restoring it would need the migrations re-run", live, restored))
	}
	return nil
}

// checkRowCounts compares every InExport table between the live database and
// the restored copy.
//
// The table list comes from the coverage registry, which is the compounding
// payoff described in coverage.go: a table added in a later wave and classified
// InExport is verified here from that moment on, with nothing in this file
// changed.
//
// The failing condition is narrow on purpose. Ordinary drift — rows added since
// the dump was taken — is expected and is reported, not failed. What fails is a
// table that has rows in the live database and none in the restore, because
// that is not drift, that is a table which is not in the backup at all. That is
// the exact shape of the disaster this package exists to catch, and it is
// invisible to any check that only asks whether pg_restore exited zero.
func (rt *RestoreTester) checkRowCounts(ctx context.Context, scratch *pgxpool.Pool, report *RestoreReport) error {
	tables := TablesWithCoverage(InExport)
	sort.Strings(tables)

	var drift []string
	for _, table := range tables {
		liveCount, err := countRows(ctx, rt.Pool, table)
		if err != nil {
			return fmt.Errorf("count live %s: %w", table, err)
		}
		restoredCount, err := countRows(ctx, scratch, table)
		if err != nil {
			report.Problems = append(report.Problems, fmt.Sprintf(
				"table %q could not be read from the restored database (%v) — it may not be in the dump at all", table, err))
			continue
		}

		report.TablesChecked++
		report.RowsRestored += restoredCount

		switch {
		case liveCount > 0 && restoredCount == 0:
			report.Problems = append(report.Problems, fmt.Sprintf(
				"table %q has %d rows live and 0 restored — this table is not being backed up", table, liveCount))
		case restoredCount < liveCount:
			drift = append(drift, fmt.Sprintf("%s %d->%d", table, liveCount, restoredCount))
		}
	}

	// Drift is recorded, never failed on, so the operator sees the dump's age
	// as real numbers rather than inferring it from a timestamp.
	report.Drift = drift
	return nil
}

// checkDocument opens one document end to end: out of the restored database,
// out of the archive, through the cipher, and against its recorded hash.
//
// This is the check that makes the archive worth taking. All three of dump,
// archive and ENCRYPTION_KEY have to agree for it to pass, and a two-of-three
// restore is precisely the failure that looks completely fine until somebody
// opens a tax return and finds it will not open.
//
// Never fatal on its own: a household with no documents yet is not a broken
// backup, and neither is a vault whose archive has not been taken this cycle.
func (rt *RestoreTester) checkDocument(ctx context.Context, scratch *pgxpool.Pool, archiveRoot string, report *RestoreReport) {
	archive, ok, err := Latest(archiveRoot, KindDocumentsArchive)
	if err != nil || !ok {
		return
	}

	var id, storageKey, contentHash string
	err = scratch.QueryRow(ctx,
		`SELECT id::text, storage_key, content_hash FROM documents ORDER BY created_at LIMIT 1`,
	).Scan(&id, &storageKey, &contentHash)
	if err != nil {
		return // no documents restored; nothing to prove
	}

	sealed, err := ExtractBlob(archive.Path, storageKey)
	if err != nil {
		report.Problems = append(report.Problems, fmt.Sprintf(
			"document %s is in the restored database but not in the document archive (%v). "+
				"A restore would list this document and be unable to open it", id, err))
		return
	}

	plaintext, err := rt.Cipher.Open(sealed)
	if err != nil {
		report.Problems = append(report.Problems, fmt.Sprintf(
			"document %s could not be decrypted with the current ENCRYPTION_KEY (%v). "+
				"The archive and the key disagree, so no document in it can be opened", id, err))
		return
	}

	sum := sha256.Sum256(plaintext)
	if got := hex.EncodeToString(sum[:]); got != contentHash {
		report.Problems = append(report.Problems, fmt.Sprintf(
			"document %s decrypted but its contents do not match the recorded hash "+
				"(expected %s, got %s) — the archive holds the wrong bytes for this document",
			id, contentHash, got))
		return
	}
	report.DocumentChecked = id
}

// checkAdvisorMessage opens one restored advisor transcript turn.
//
// The same argument as checkDocument, applied to the other sealed thing this
// app stores — and it is worth its own check for a reason particular to this
// table. advisor_messages.content is BYTEA, which means the PORTABLE export
// withholds it by type: a household restoring from that file gets threads with
// empty bodies. The dump is therefore the ONLY path that brings a transcript
// back, and "the only path" is exactly the kind of claim that has to be proven
// rather than assumed.
//
// Two of three is the failure being hunted here as well: a dump that restores
// the rows and an ENCRYPTION_KEY that has since been rotated looks entirely
// healthy until somebody opens a conversation and finds it will not decrypt.
//
// No archive is involved — these bytes live in Postgres — so this runs whether
// or not a document archive was taken. Never fatal on its own: a household that
// has not saved a conversation is not a broken backup.
func (rt *RestoreTester) checkAdvisorMessage(ctx context.Context, scratch *pgxpool.Pool, report *RestoreReport) {
	if rt.Cipher == nil {
		return
	}

	var id string
	var sealed []byte
	err := scratch.QueryRow(ctx,
		`SELECT id::text, content FROM advisor_messages ORDER BY created_at LIMIT 1`,
	).Scan(&id, &sealed)
	if err != nil {
		return // no advisor messages restored; nothing to prove
	}

	plaintext, err := rt.Cipher.Open(sealed)
	if err != nil {
		report.Problems = append(report.Problems, fmt.Sprintf(
			"advisor message %s could not be decrypted with the current ENCRYPTION_KEY (%v). "+
				"The dump and the key disagree, so no saved conversation can be read back — "+
				"and the portable export does not carry these bodies at all", id, err))
		return
	}
	if len(plaintext) == 0 {
		report.Problems = append(report.Problems, fmt.Sprintf(
			"advisor message %s decrypted to an empty body — the dump holds the wrong bytes for this turn", id))
		return
	}
	report.AdvisorMessageChecked = id
}

// --------------------------------------------------------------------------
// Scratch database plumbing
// --------------------------------------------------------------------------

func (rt *RestoreTester) createScratch(ctx context.Context, name string) error {
	if err := guardScratchName(name); err != nil {
		return err
	}
	if _, err := rt.Pool.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %q`, name)); err != nil {
		return fmt.Errorf("create scratch database: %w", err)
	}
	return nil
}

func (rt *RestoreTester) dropScratch(ctx context.Context, name string) error {
	if err := guardScratchName(name); err != nil {
		return err
	}
	// FORCE terminates any connection still open against it. Without it a
	// lingering session — our own pool draining, say — turns cleanup into a
	// permanent leak of a full copy of the database.
	if _, err := rt.Pool.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %q WITH (FORCE)`, name)); err != nil {
		return err
	}
	return nil
}

// guardScratchName is the last thing standing between a bug in scratch-name
// construction and a DROP DATABASE against the live database.
//
// It is cheap, it is redundant with every caller, and it stays. The operation
// it guards is unrecoverable and the code path runs unattended at 3am.
func guardScratchName(name string) error {
	prefix := config.ScratchDBPrefix()
	if !strings.HasPrefix(name, prefix) || len(name) <= len(prefix) {
		return fmt.Errorf("refusing to operate on database %q: restore-test databases must be named %s*", name, prefix)
	}
	if strings.ContainsAny(name, `"\ ;`) {
		return fmt.Errorf("refusing to operate on database %q: illegal characters in name", name)
	}
	return nil
}

// restore loads a dump into the scratch database.
func (rt *RestoreTester) restore(ctx context.Context, scratch, dumpPath string) error {
	runCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	bin := "pg_restore"
	if rt.BinDir != "" {
		bin = rt.BinDir + "/pg_restore"
	}
	cmd := exec.CommandContext(runCtx, bin,
		"--dbname="+rt.urlFor(scratch),
		"--no-owner",
		"--no-privileges",
		// Fail on the first error rather than restoring what it can and
		// exiting zero. A partially restored database that reports success is
		// the single worst outcome available to this function.
		"--exit-on-error",
		dumpPath,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pg_restore failed on %s: %w: %s", dumpPath, err, tidyStderr(stderr.String()))
	}
	return nil
}

// urlFor rewrites the configured database URL to point at another database on
// the same server.
func (rt *RestoreTester) urlFor(database string) string {
	u, err := url.Parse(rt.DatabaseURL)
	if err != nil {
		return rt.DatabaseURL
	}
	u.Path = "/" + database
	return u.String()
}

func countRows(ctx context.Context, pool *pgxpool.Pool, table string) (int64, error) {
	var n int64
	// The table name is from the coverage registry — a Go map literal in this
	// repository — and is quoted regardless.
	err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %q`, table)).Scan(&n)
	return n, err
}
