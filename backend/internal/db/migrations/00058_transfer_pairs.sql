-- +goose Up
--
-- Structural transfer pairing.
--
-- The app detects internal transfers (money moving between two of the
-- household's own accounts) by matching a debit on one account to a credit of
-- equal magnitude on another, within a date window. This is name-independent
-- and so catches transfers the payee-name heuristics and Plaid miss — most
-- damagingly the case that motivated this: a $700 checking→savings move whose
-- outgoing leg Plaid's counterparty labelled "ACH CAPITAL ONE - TRANSFER" and
-- whose incoming leg a different institution labelled "ALTRA FEDERAL CREDIT
-- UNION". The name heuristic never saw the second leg, the merchant cache
-- misfiled the first as "Investments", and the two were never linked.
--
-- A pair is one row linking the OUT leg (the debit, amount > 0 under the Plaid
-- money-out convention) to the IN leg (the credit, amount < 0). The UNIQUE
-- constraints make a transaction appear in at most one pair, which is what makes
-- the pairing pass idempotent: a re-run simply never reconsiders a leg already
-- linked. The matching itself lives in Go (internal/categorize/pairing.go); the
-- candidates query excludes loan and credit-card accounts, income, manually
-- categorised rows, and anything already paired.
--
-- Numbered 00058, above 00057_account_return_rate.sql.

CREATE TABLE transaction_pairs (
    id           uuid         PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id uuid         NOT NULL REFERENCES households (id) ON DELETE CASCADE,
    -- out_txn_id is the debit leg (amount > 0, money leaving an account).
    out_txn_id   uuid         NOT NULL REFERENCES transactions (id) ON DELETE CASCADE,
    -- in_txn_id is the credit leg (amount < 0, money arriving in another).
    in_txn_id    uuid         NOT NULL REFERENCES transactions (id) ON DELETE CASCADE,
    amount       numeric(20,4) NOT NULL,
    created_at   timestamptz  NOT NULL DEFAULT now(),
    -- A transaction is in at most one pair, in either role. This is the
    -- idempotency guarantee: the pairing pass skips any leg already linked here.
    CONSTRAINT transaction_pairs_out_uniq UNIQUE (out_txn_id),
    CONSTRAINT transaction_pairs_in_uniq  UNIQUE (in_txn_id),
    CONSTRAINT transaction_pairs_out_in_diff CHECK (out_txn_id <> in_txn_id)
);

CREATE INDEX transaction_pairs_household_idx ON transaction_pairs (household_id);

-- category_source = 'pairing' marks the legs the structural pairer re-filed.
-- The original CHECK (00001, widened by 00015) predates this source, so widen
-- it once more — same drop-and-re-add shape 00015 used, for the same reason.
ALTER TABLE transactions DROP CONSTRAINT IF EXISTS transactions_category_source_check;
ALTER TABLE transactions ADD CONSTRAINT transactions_category_source_check
    CHECK (category_source IS NULL
           OR category_source IN ('manual', 'rule', 'cache', 'plaid', 'llm', 'heuristic', 'pairing'));

COMMENT ON TABLE transaction_pairs IS
    'Two legs of one internal transfer (a debit on one of the household''s '
    'accounts matched to an equal credit on another). Structural: matched by '
    'amount and date, not by payee name, so it catches transfers the name '
    'heuristics and Plaid miss. Each transaction is in at most one pair.';

-- +goose Down

DROP TABLE IF EXISTS transaction_pairs;

-- Restore the narrower CHECK. 'pairing' rows would already be gone with the
-- pairs table dropped and the legs re-categorised, so the constraint widens
-- cleanly again.
ALTER TABLE transactions DROP CONSTRAINT IF EXISTS transactions_category_source_check;
ALTER TABLE transactions ADD CONSTRAINT transactions_category_source_check
    CHECK (category_source IS NULL
           OR category_source IN ('manual', 'rule', 'cache', 'plaid', 'llm', 'heuristic'));
