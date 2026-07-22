-- +goose Up
-- Caches the AI-written monthly narrative so it is generated once per household
-- per month rather than on every page view. month is the first day of the month
-- the text summarises; the unique key makes regeneration an upsert.
CREATE TABLE monthly_summaries (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id UUID        NOT NULL REFERENCES households (id) ON DELETE CASCADE,
    month        DATE        NOT NULL,
    summary      TEXT        NOT NULL,
    -- The model that produced it, for provenance when the provider changes.
    model        TEXT        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (household_id, month)
);

-- +goose Down
DROP TABLE IF EXISTS monthly_summaries;
