-- +goose Up
--
-- Per-account balance snapshots for Plaid accounts (MAD-119).
--
-- account_balance_history was created (00053) as the trail behind a MANUAL
-- balance: a row appeared only when the user moved the figure, and 'reason'
-- named the kind of move (manual / scheduled / holding_revalue / fee /
-- dividend). Plaid accounts were never written here because Plaid owns their
-- balances outright, so the only place a Plaid account's balance existed was
-- the single current_balance column it is overwritten with on every sync.
--
-- The consequence is the same one that made net_worth_snapshots necessary (see
-- docs/concepts.md "Why net worth is snapshotted"): balances carry no history,
-- so once a Plaid balance moves the previous value is gone. Net worth is
-- recorded daily to recover a trend; a per-account trend has never been, and
-- "is my checking account slowly draining?" — one of the most-asked-for
-- account-level charts — has no data behind it.
--
-- This migration adds the provenance value the snapshot path writes under, so
-- the existing account_balance_history table can hold Plaid balance points
-- alongside manual ones without a parallel table. The read endpoint
-- (ListAccountBalanceHistory) already joins account_access for visibility and
-- never filtered on source, so it serves both once the rows exist; a Plaid
-- account's trend simply starts on the day the app began snapshotting it, the
-- same honest bound net worth carries.
--
-- 'snapshot' is the marker for "the app captured the current balance at this
-- instant", whether the trigger was a just-finished sync (fresh figures) or the
-- daily sweep (a quiet account still getting a point). One value covers both,
-- matching how net_worth_snapshots records one row per household per day
-- regardless of which trigger wrote it. It is deliberately absent from the
-- user-settable reason list in handleSetManualBalance, for the same reason
-- 'scheduled' is: a caller able to write it by hand would make the audit trail
-- unable to answer "did the app record this, or did I".

ALTER TABLE account_balance_history DROP CONSTRAINT IF EXISTS account_balance_history_reason_check;
ALTER TABLE account_balance_history ADD CONSTRAINT account_balance_history_reason_check
    CHECK (reason IN ('manual', 'scheduled', 'holding_revalue', 'fee', 'dividend', 'snapshot'));

-- +goose Down
ALTER TABLE account_balance_history DROP CONSTRAINT IF EXISTS account_balance_history_reason_check;
-- Rows the snapshot path wrote have no equivalent under the old vocabulary, so
-- they are removed before the narrower CHECK is restored or the constraint
-- would reject them.
DELETE FROM account_balance_history WHERE reason = 'snapshot';
ALTER TABLE account_balance_history ADD CONSTRAINT account_balance_history_reason_check
    CHECK (reason IN ('manual', 'scheduled', 'holding_revalue', 'fee', 'dividend'));
