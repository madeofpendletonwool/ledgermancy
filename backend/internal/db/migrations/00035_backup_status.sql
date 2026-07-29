-- +goose Up
-- One row per backup, archive, export, restore-test or mirror attempt.
--
-- This table exists so the app can tell the operator the truth about their
-- recovery posture instead of assuming it. A backup nobody has verified is a
-- guess, and the whole failure mode this subsystem addresses is an operator who
-- believes they are covered and is not.
--
-- Deliberately NOT household-scoped. This is operator-level infrastructure
-- state, not user data — one Postgres and one document volume serve every
-- household on the instance. The endpoint that reads it is gated on
-- auth.RequireOwner accordingly.
--
-- 'key_ack' is the odd kind out: it records a human saying "I have stored
-- ENCRYPTION_KEY somewhere safe" rather than anything the app did. It lives here
-- because it answers the same question as the rest — can this deployment
-- actually be restored — and because "latest row per kind" then answers the
-- whole continuity panel with one query.
CREATE TABLE backup_runs (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    kind          TEXT NOT NULL CHECK (kind IN (
                      'db_dump',
                      'documents_archive',
                      'export',
                      'restore_test',
                      'mirror_push',
                      'key_ack')),
    status        TEXT NOT NULL CHECK (status IN ('success', 'failure')),
    started_at    TIMESTAMPTZ NOT NULL,
    finished_at   TIMESTAMPTZ NOT NULL,
    size_bytes    BIGINT,
    -- Which destination directory the artefact landed in. Nullable: a restore
    -- test and a key acknowledgement produce no artefact.
    destination   TEXT,
    -- The artefact's path, so the panel can name a file and the restore runbook
    -- can be followed without guessing at a filename.
    artifact_path TEXT,
    -- Error message on failure; row counts and deltas on a restore test. This
    -- is what an operator reads at 1am, so producers should write it for that
    -- reader rather than for a log parser.
    detail        TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The panel's only query is "latest run per kind", and the history view is the
-- same ordering unfiltered.
CREATE INDEX backup_runs_kind_started_idx ON backup_runs (kind, started_at DESC);

-- +goose Down
DROP TABLE IF EXISTS backup_runs;
