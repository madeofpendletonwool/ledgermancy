-- +goose Up
-- Envelope budgeting: when a budget rolls over, whatever is left unspent at the
-- end of a month is added to next month's available amount (and an overspend
-- carries forward as a reduction). A plain budget resets to its full amount each
-- month, as before; rollover is opt-in per budget and defaults off so existing
-- budgets are unchanged.
--
-- The carried balance is not stored — it is derived on read from the budget's
-- amount, its start month (effective_from, else the month it was created), and
-- the category's spend since then. Nothing to backfill.
ALTER TABLE budgets ADD COLUMN rollover BOOLEAN NOT NULL DEFAULT FALSE;

-- +goose Down
ALTER TABLE budgets DROP COLUMN rollover;
