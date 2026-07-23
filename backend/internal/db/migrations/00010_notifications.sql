-- +goose Up
-- Marks when an alert event has been processed for external push, so an
-- overlapping evaluation sweep never re-notifies. Nullable: NULL means "not yet
-- dispatched". The insight feed (04) and digest (10) have their own dedupe
-- (read_at / cadence window) and do not use this column.
ALTER TABLE alert_events ADD COLUMN notified_at TIMESTAMPTZ;

-- The notify dispatcher scans for un-dispatched events per household, so a
-- partial index keeps that scan cheap as the events table grows.
CREATE INDEX alert_events_unnotified_idx
    ON alert_events (alert_id) WHERE notified_at IS NULL;

-- +goose Down
DROP INDEX IF EXISTS alert_events_unnotified_idx;
ALTER TABLE alert_events DROP COLUMN IF EXISTS notified_at;
