-- +goose Up
-- Idempotency guard for the scheduled digest: one row per (user, period) once a
-- digest has been sent, so an overlapping sweep or a second worker never
-- double-sends. period_key is the ISO week ('2026-W30') for a weekly cadence or
-- the month ('2026-07') for a monthly one — whatever the digest covered.
-- Mirrors the alert_events.notified_at idempotency pattern, as its own table
-- because the digest has no natural event row to stamp.
CREATE TABLE digest_deliveries (
    user_id    UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    period_key TEXT        NOT NULL,
    sent_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, period_key)
);

-- +goose Down
DROP TABLE IF EXISTS digest_deliveries;
