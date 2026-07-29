-- Continuity: the record of what has been backed up, archived, exported and
-- verified. Operator-level state, not household-scoped — see the comment on the
-- table in migration 00035.

-- name: RecordBackupRun :one
-- Insert one attempt's outcome. Failures are recorded exactly like successes:
-- a run that failed silently is indistinguishable from one that never ran, and
-- the panel needs to tell those apart.
INSERT INTO backup_runs (
    kind, status, started_at, finished_at,
    size_bytes, destination, artifact_path, detail
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: LatestBackupRunPerKind :many
-- The newest run of each kind, whatever its status. This is the continuity
-- panel's whole query.
--
-- DISTINCT ON rather than a window function or a lateral join: it reads
-- directly off backup_runs_kind_started_idx, and the ORDER BY here must keep
-- matching that index or the plan degrades to a sort over the full history.
SELECT DISTINCT ON (kind) *
FROM backup_runs
ORDER BY kind, started_at DESC;

-- name: LatestSuccessfulBackupRun :one
-- The newest *successful* run of one kind. The restore test uses this to find
-- the dump it should restore, which is why it must not settle for a failure —
-- restoring the artefact of a failed dump is how a green restore test comes to
-- mean nothing.
SELECT * FROM backup_runs
WHERE kind = $1 AND status = 'success'
ORDER BY started_at DESC
LIMIT 1;

-- name: ListBackupRuns :many
-- Recent history for the panel's expandable detail, newest first.
SELECT * FROM backup_runs
ORDER BY started_at DESC
LIMIT $1;

-- name: ListAllDocumentBlobs :many
-- Every document blob on the instance, across all households, for the vault
-- archive and the restore test's document check.
--
-- Deliberately not household-scoped: this is the backup path, and a backup that
-- covered one household would be worse than none. content_hash rides along so
-- the restore test can verify a decrypted blob without a second lookup.
SELECT id, storage_key, content_hash, size_bytes
FROM documents
ORDER BY created_at;
