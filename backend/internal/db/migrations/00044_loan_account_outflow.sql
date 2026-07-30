-- +goose Up
-- Plaid signs amounts as POSITIVE = money leaving the account — that is the
-- convention every report in reports.sql and alerts.sql is built on. It holds
-- for depository and credit accounts, where a purchase is positive and a
-- payment/refund is negative.
--
-- It inverts for `loan` accounts. There, Plaid mirrors how a loan statement
-- reads: a new charge (interest, escrow) is positive, and a PAYMENT — the
-- thing that reduces what you owe — is negative. A report that filters
-- `amount > 0` uniformly therefore excludes every loan payment a household
-- makes, permanently, regardless of date range: the money is real and spent,
-- and it never shows up. This surfaced as a mortgage that had two years of
-- payments but a merchant page insisting none of them existed.
--
-- is_spend() is the one place that account-type exception lives, so every
-- query that means "this row is money going out" calls it instead of
-- repeating the exception inline. IMMUTABLE so the planner can treat it like
-- any other expression in a WHERE clause.
--
-- The depository/credit side of a transfer that FUNDS a loan payment (e.g. a
-- savings withdrawal on its way to the mortgage servicer) is unaffected by
-- this function — it stays out of spend reports because it is categorised as
-- a transfer, exactly as before. is_spend() only changes which sign counts as
-- "out" for the account the row belongs to; it does not touch is_transfer.
CREATE OR REPLACE FUNCTION is_spend(amount NUMERIC, account_type TEXT) RETURNS BOOLEAN AS $$
    SELECT CASE WHEN account_type = 'loan' THEN amount < 0 ELSE amount > 0 END
$$ LANGUAGE sql IMMUTABLE PARALLEL SAFE;

-- +goose Down
DROP FUNCTION IF EXISTS is_spend(NUMERIC, TEXT);
