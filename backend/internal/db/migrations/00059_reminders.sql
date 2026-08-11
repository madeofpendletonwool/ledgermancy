-- +goose Up
-- Reminders opt-in. Both recurring_obligations and goals gain a `remind` flag so
-- a household can silence a single noisy item without deleting it. Defaults TRUE:
-- the bill calendar and goals exist to keep things in front of you, so the
-- ordinary case is "remind me", and a member opts out per item.
--
-- The new reminder producers (overdue_bill, payoff_progress) read this column and
-- skip rows where it is FALSE. upcoming_bill/document_expiry keep their existing
-- universal behaviour — those are informational ("this is due soon"), where the
-- toggle here governs the action-taking reminders specifically.

ALTER TABLE recurring_obligations
    ADD COLUMN remind BOOLEAN NOT NULL DEFAULT TRUE;

ALTER TABLE goals
    ADD COLUMN remind BOOLEAN NOT NULL DEFAULT TRUE;

-- +goose Down
ALTER TABLE recurring_obligations DROP COLUMN IF EXISTS remind;
ALTER TABLE goals DROP COLUMN IF EXISTS remind;
