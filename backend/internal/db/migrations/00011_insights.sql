-- +goose Up
-- The proactive insight feed. Deterministic detectors raise rows here; the feed
-- reads them back in priority order. Every money figure lives in `data` as a
-- decimal STRING, never a float, so the narrative can be regenerated (or shown
-- structured) without ever recomputing an amount.
--
-- dedupe_key is the stable identity of one logical insight (e.g.
-- 'spending_spike:dining:2026-07'); the generation job upserts on
-- (household_id, dedupe_key) so a re-run refreshes an insight rather than
-- duplicating it, while a user's dismissal on that row sticks.
CREATE TABLE insights (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id  UUID        NOT NULL REFERENCES households (id) ON DELETE CASCADE,
    kind          TEXT        NOT NULL,   -- 'spending_spike' | 'new_recurring'
                                          -- | 'budget_pace' | 'low_leftover' | ...
    priority      SMALLINT    NOT NULL DEFAULT 0,  -- higher = more important
    title         TEXT        NOT NULL,
    body          TEXT        NOT NULL,
    data          JSONB       NOT NULL DEFAULT '{}',  -- money as decimal STRINGS
    period        DATE,
    dedupe_key    TEXT        NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    read_at       TIMESTAMPTZ,
    dismissed_at  TIMESTAMPTZ,
    UNIQUE (household_id, dedupe_key)
);

-- Feed order: newest, highest-priority, non-dismissed first.
CREATE INDEX insights_feed_idx
    ON insights (household_id, dismissed_at, priority DESC, created_at DESC);

-- +goose Down
DROP TABLE IF EXISTS insights;
