-- +goose Up
-- Per-rule push switch. `enabled` decides whether a rule fires at all (and thus
-- shows in the in-app feed); `push` decides whether a fired event is also sent
-- to members' external notification channels. Default FALSE so upgrading never
-- starts pushing an alert the household had not opted into — in-app stays the
-- baseline, push is the deliberate add-on. Household-scoped like the rule
-- itself: turning it on pushes to every member who has configured a channel.
-- This replaces the per-user notify.push_kinds preference, which conflated
-- "which alerts fire" (a shared rule property) with "where I get them" (a
-- personal channel choice).
ALTER TABLE alerts ADD COLUMN push BOOLEAN NOT NULL DEFAULT FALSE;

-- +goose Down
ALTER TABLE alerts DROP COLUMN IF EXISTS push;
