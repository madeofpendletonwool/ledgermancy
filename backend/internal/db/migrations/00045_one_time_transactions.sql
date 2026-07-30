-- +goose Up
-- A household's forward-looking figures — safe-to-spend, the recurring
-- detector, every "typical month" estimate — are built by averaging what
-- actually happened over a trailing window. That is the right shape for
-- ordinary spending and exactly the wrong shape for a one-off capital event.
--
-- The case that forced this: a $14,295.54 auto-loan PAYOFF, correctly
-- categorised `loan-payments` (a fixed category) and correctly counted as
-- money spent. Six-month averaging then spread it at ~$2,383/month across the
-- next six months of "fixed bills", telling the household its recurring
-- obligations had quadrupled at the precise moment they had actually FALLEN
-- (the $540.22/month payment was gone with the loan).
--
-- Two flags, two different claims, deliberately not merged:
--
--   * excluded_from_reports — "as far as every report is concerned, this did
--     not happen". A total blackout. Already honoured everywhere.
--
--   * is_one_time — "this really happened and belongs in the month it fell,
--     but it is not EVIDENCE ABOUT A TYPICAL MONTH". The Spending page still
--     shows it, because the money genuinely left. Only trailing-baseline and
--     recurrence-detection queries skip it, and only when they opt in via
--     their `exclude_one_time` argument.
--
-- Keeping them separate is what lets a payoff stay visible in the history it
-- belongs to while ceasing to drive predictions about the future.
ALTER TABLE transactions
    ADD COLUMN is_one_time BOOLEAN NOT NULL DEFAULT FALSE;

COMMENT ON COLUMN transactions.is_one_time IS
    'User state: a real but non-repeating event (loan payoff, tax bill, car '
    'purchase). Counted in the month it fell; skipped by trailing-average and '
    'recurring-detection queries that pass exclude_one_time. Preserved across '
    'Plaid sync, like excluded_from_reports and notes.';

-- +goose Down
ALTER TABLE transactions DROP COLUMN IF EXISTS is_one_time;
