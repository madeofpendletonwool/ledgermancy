-- +goose Up
--
-- Numbered 00049, not the 00038 doc 25 reserved. Plan docs reserve migration
-- numbers up front, but the docs have shipped out of order: the schema is
-- already at 00048, and goose refuses to apply anything below the current
-- version ("found N missing migrations"). A 00038 file would fail every
-- existing deployment at boot rather than merely looking untidy. Reserve-ahead
-- numbering only holds while the docs land in order — take the next free number
-- when it does not.
--
-- The digest as CONTENT, alongside digest_deliveries (00013) which is the
-- delivery log. Keeping both is deliberate: one answers "did we send?", this one
-- answers "what did it say?", and until now the answer to the second was thrown
-- away the moment a push left the process.
--
-- payload is a SNAPSHOT of the figures as they stood when the digest was
-- generated, and is rendered verbatim on read. It is never recomputed. A digest
-- is a statement about a past period, so recategorising a transaction inside
-- that period must not silently rewrite what a user already read — the same
-- rationale as the monthly_summaries cache.
CREATE TABLE digest_entries (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id UUID NOT NULL REFERENCES households (id) ON DELETE CASCADE,
    -- Which member's view this is. Visibility scoping (own items ∪ shared)
    -- means two members of one household legitimately see different figures,
    -- so one shared row per household would leak private institutions.
    user_id      UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    cadence      TEXT NOT NULL,             -- matches digestDue's vocabulary
    -- The same period vocabulary digest_deliveries uses: '2026-W29' weekly,
    -- '2026-06' monthly. It is the uniqueness key rather than period_start
    -- because a weekly digest reports over the month-to-date window, so four
    -- weekly digests in one month share a period_start and only the key tells
    -- them apart. Doc 25 proposed period_start here; that would have capped a
    -- weekly household at one stored digest per month.
    period_key   TEXT NOT NULL,
    period_start DATE NOT NULL,
    period_end   DATE NOT NULL,
    label        TEXT NOT NULL,             -- the existing digestWindow label
    payload      JSONB NOT NULL,
    -- The AI narrative, when one was generated. NULL when AI is disabled, which
    -- is a fully supported way to run: the payload alone is a complete digest.
    narrative    TEXT,
    read_at      TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, period_key)
);

CREATE INDEX digest_entries_user_created_idx ON digest_entries (user_id, created_at DESC);

-- +goose Down
DROP TABLE IF EXISTS digest_entries;
