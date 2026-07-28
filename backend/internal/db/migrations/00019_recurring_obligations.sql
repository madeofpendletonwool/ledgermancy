-- +goose Up
-- Known future money out: the bill calendar's storage.
--
-- The app already *detects* recurring charges (GetRecurringMerchants), but that
-- detection is derived on every read and never persisted, so nothing in the app
-- knows what is due next week. This table is the persisted, forward-looking
-- version: one row per obligation, with a cadence the user can check by hand.
--
-- Two kinds of row live here and the difference matters:
--
--   * source='manual'   — entered by a member. This is the ONLY way an annual
--                         insurance premium, a dues renewal, or anything paid by
--                         check can ever be known, because no amount of
--                         transaction analysis will find it.
--   * source='detected' — promoted from the recurring detector. Refreshed in
--                         place on every promotion pass (hence the partial
--                         unique index below), EXCEPT when user_edited is set:
--                         a correction the user made must survive re-detection.
--
-- Occurrences are NOT stored. anchor_date + (interval_count, interval_unit)
-- generates them, so there is no "next due" cache to go stale and no sweep to
-- keep it honest. Month arithmetic is done with Postgres interval addition,
-- which clamps month ends correctly (2025-01-31 + interval '1 month' is
-- 2025-02-28); Go's time.AddDate does NOT clamp, so occurrence expansion lives
-- in SQL and nowhere else. See ListUpcomingObligations.
CREATE TABLE recurring_obligations (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id   UUID NOT NULL REFERENCES households (id) ON DELETE CASCADE,

    -- Who entered it, following the per-item visibility pattern: a member's
    -- private obligation must not leak into the household view. NULL means
    -- household-owned, which is what a detected row is — the detector promoted
    -- it from shared items, no member typed it, and it must not vanish when a
    -- member's account is deleted.
    user_id        UUID REFERENCES users (id) ON DELETE CASCADE,
    is_shared      BOOLEAN NOT NULL DEFAULT TRUE,

    label          TEXT NOT NULL,
    -- Positive = money out, the same sign convention as transactions.
    amount         NUMERIC(20,4) NOT NULL CHECK (amount > 0),
    category_id    UUID REFERENCES categories (id) ON DELETE SET NULL,
    account_id     UUID REFERENCES accounts (id) ON DELETE SET NULL,

    -- Cadence as (n, unit) so "every 2 years" and "every 2 weeks" are both
    -- expressible. Deliberately not a cron string: this must be arithmetic a
    -- user can check by hand.
    interval_count INT  NOT NULL CHECK (interval_count > 0),
    interval_unit  TEXT NOT NULL CHECK (interval_unit IN ('day','week','month','year')),
    anchor_date    DATE NOT NULL,          -- first occurrence; all others derive
    end_date       DATE,                   -- NULL = open-ended

    source         TEXT NOT NULL DEFAULT 'manual'
                        CHECK (source IN ('manual','detected')),
    -- Set on detected rows so the detector can recognise its own output and not
    -- double-count against GetRecurringMerchants, and so suppression
    -- (recurring_overrides) can reach the promoted row.
    merchant_key   TEXT,
    -- A detected row the user has corrected. Promotion skips these, or the next
    -- pass would silently undo every correction.
    user_edited    BOOLEAN NOT NULL DEFAULT FALSE,
    is_active      BOOLEAN NOT NULL DEFAULT TRUE,

    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- A manual row was typed by somebody; a detected row was keyed by merchant.
    CONSTRAINT recurring_obligations_manual_owner
        CHECK (source = 'detected' OR user_id IS NOT NULL),
    CONSTRAINT recurring_obligations_detected_key
        CHECK (source = 'manual' OR merchant_key IS NOT NULL),
    -- An end before the first occurrence would generate nothing but read as a
    -- live obligation in the list.
    CONSTRAINT recurring_obligations_window
        CHECK (end_date IS NULL OR end_date >= anchor_date)
);

CREATE INDEX recurring_obligations_household_idx
    ON recurring_obligations (household_id);

-- One detected row per merchant per household; re-detection updates in place.
CREATE UNIQUE INDEX recurring_obligations_detected_merchant_key
    ON recurring_obligations (household_id, merchant_key)
    WHERE source = 'detected';

-- +goose Down
DROP TABLE IF EXISTS recurring_obligations;
