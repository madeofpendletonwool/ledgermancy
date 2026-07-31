-- +goose Up
-- A per-household suppression list for the anomaly detectors, modelled directly
-- on recurring_overrides (00016). Both detectors are statistics over a merchant's
-- own history, so both will sometimes flag something the household knows is fine:
-- the plumber who charges a different amount every visit, the parking garage that
-- legitimately bills twice on a long day. When that happens the user marks the
-- merchant normal and it drops out of the detector at once.
--
-- Presence of a row = suppressed; there is no state beyond that. The label is
-- captured at suppression time so the "restore" UI can name the merchant without
-- re-deriving it from transactions.
--
-- The exclusion lives in the DETECTION QUERIES (a NOT EXISTS on this table,
-- resolved on both sides), not in Go — same reason 00016 gives. One place to
-- enforce it means no consumer can forget, and the two detectors cannot drift
-- apart about what "suppressed" means.
--
-- The merchant_key is stored RESOLVED (see the header of queries/merchants.sql).
-- Resolution is idempotent, so a row written against a raw descriptor before a
-- merge still silences the whole merged merchant afterwards.
--
-- scope is the one thing recurring_overrides did not need. "This merchant always
-- charges wildly different amounts" and "this merchant legitimately bills me
-- twice in a day" are different claims about the same merchant, and silencing one
-- must not silence the other — a plumber suppressed for outliers should still
-- raise a genuine double charge. 'all' exists so the UI can offer a single "never
-- flag this merchant" without writing two rows, and so a scope added later is
-- covered by existing 'all' rows rather than leaking.
CREATE TABLE anomaly_overrides (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id   UUID        NOT NULL REFERENCES households (id) ON DELETE CASCADE,
    merchant_key   TEXT        NOT NULL,
    merchant_label TEXT        NOT NULL DEFAULT '',
    scope          TEXT        NOT NULL DEFAULT 'all',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT anomaly_overrides_scope_check
        CHECK (scope IN ('all', 'outlier', 'duplicate')),
    UNIQUE (household_id, merchant_key, scope)
);

-- +goose Down
DROP TABLE IF EXISTS anomaly_overrides;
