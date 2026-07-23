-- +goose Up
-- A small, extensible key/value settings store, keyed by scope. Notifications
-- (03), the insight feed (04), and the digest (10) read per-user knobs from
-- here. JSONB values so a new setting never needs a migration — the same
-- deliberate choice as alerts.config.
CREATE TABLE preferences (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    scope        TEXT        NOT NULL CHECK (scope IN ('user', 'household')),
    -- Exactly one of these is set, matching scope. The CHECKs below enforce it.
    user_id      UUID        REFERENCES users (id)      ON DELETE CASCADE,
    household_id UUID        REFERENCES households (id) ON DELETE CASCADE,
    key          TEXT        NOT NULL,
    value        JSONB       NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT preferences_user_scope CHECK (
        scope <> 'user' OR (user_id IS NOT NULL AND household_id IS NULL)
    ),
    CONSTRAINT preferences_household_scope CHECK (
        scope <> 'household' OR (household_id IS NOT NULL AND user_id IS NULL)
    )
);

-- NULLs are not equal in a plain unique index, so a single UNIQUE across the
-- nullable columns would never dedupe. Partial indexes per scope give each
-- upsert an exact, unambiguous conflict target — the same pattern budgets uses
-- (00003_budget_unique.sql).
CREATE UNIQUE INDEX preferences_user_key
    ON preferences (user_id, key) WHERE scope = 'user';
CREATE UNIQUE INDEX preferences_household_key
    ON preferences (household_id, key) WHERE scope = 'household';

-- +goose Down
DROP TABLE IF EXISTS preferences;
