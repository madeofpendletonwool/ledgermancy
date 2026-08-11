-- +goose Up
-- Per-occurrence satisfaction: the record that one instance of a recurring
-- obligation was paid, so the overdue-reminder producer can stop nagging once it
-- has been dealt with — without losing the cadence that generates the next one.
--
-- Why per-occurrence rather than a "satisfied_through" cursor on the obligation:
-- a cursor can only say "everything up to X is done", which collapses two
-- different states the UI needs to tell apart (the matcher thinks it was paid vs
-- a person confirmed it), and it cannot represent a skipped or partial payment.
-- One row per (obligation, due_date) is the smallest model that supports a
-- manual "mark paid" alongside an automatic match, and that stays honest when a
-- payment is late by a few days.
--
-- source values:
--   manual  — a member pressed "mark paid". Always authoritative.
--   matched — the producer found a transaction it believes covers this
--             occurrence. The UI presents this as "we think this was paid" and
--             lets a member confirm (which upserts a manual row) or clear it.
--
-- matched_txn_id is nullable: a manual mark carries no transaction, only the
-- assertion. The (obligation_id, due_date) PK makes a satisfaction unique per
-- occurrence — re-marking is an upsert, never a duplicate, and the producer's
-- existence check is a single JOIN.

CREATE TABLE obligation_satisfaction (
    obligation_id  UUID         NOT NULL REFERENCES recurring_obligations (id) ON DELETE CASCADE,
    -- due_date is the specific occurrence this satisfaction covers, in the same
    -- DATE shape the cadence expansion produces. It is part of the key, so two
    -- occurrences of the same bill are two independent rows.
    due_date       DATE         NOT NULL,
    satisfied_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    source         TEXT         NOT NULL CHECK (source IN ('manual', 'matched')),
    -- Set on a 'matched' row (the transaction the producer paired this with) and
    -- NULL on a 'manual' row. Kept for audit, not for enforcement: deleting the
    -- transaction does not un-satisfy an occurrence a person already confirmed.
    matched_txn_id UUID         REFERENCES transactions (id) ON DELETE SET NULL,
    -- Who pressed "mark paid". NULL on a 'matched' row (no member was involved)
    -- and on obligations with no owning member (detected/household-owned). A
    -- manual row from a member carries that member's id for the audit surface.
    user_id        UUID         REFERENCES users (id) ON DELETE SET NULL,
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT obligation_satisfaction_pk PRIMARY KEY (obligation_id, due_date)
);

CREATE INDEX obligation_satisfaction_obligation_idx
    ON obligation_satisfaction (obligation_id);

COMMENT ON TABLE obligation_satisfaction IS
    'One paid instance of a recurring obligation. The overdue-reminder producer '
    'treats a row here as "this occurrence is dealt with" and stops raising it; '
    'a member can add a manual row to confirm a match or record a payment the '
    'matcher missed.';

-- +goose Down
DROP TABLE IF EXISTS obligation_satisfaction;
