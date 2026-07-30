-- +goose Up
-- Debt terms the household typed in, as opposed to the ones Plaid reported.
--
-- WHY THIS IS NOT COLUMNS ON `liabilities`. Two reasons, either one sufficient:
--
--   1. `liabilities` is a MIRROR of Plaid's /liabilities/get. UpsertLiability
--      rewrites every column on conflict, which is exactly right for a mirror
--      and fatal for anything a human typed. Putting manual values there would
--      make the safety of a user's own data depend on nobody ever adding a
--      column to that DO UPDATE list — a landmine, not a design.
--   2. `liabilities.kind` is NOT NULL and CHECKed to (credit|student|mortgage).
--      A manual-only row has to supply one, and an auto loan or a personal line
--      of credit has no legal value to supply.
--
-- So the split is: `liabilities` is what the institution said, `account_terms`
-- is what the household said, and the two are merged per field at read time with
-- manual winning. See mergeDebtTerms in backend/internal/api/debt_terms.go. The
-- Plaid sync path touches only the first and needs no knowledge of this table.
--
-- This exists because Plaid serves the Liabilities product for a minority of
-- institutions. Capital One and Altra FCU both decline it, so the household that
-- prompted this had three real debts, an APR for none of them, and a payoff-goal
-- picker that refused to list any of them.
--
-- No household_id column: scoping runs accounts -> plaid_items -> users, the
-- same chain every other account-keyed table uses. A second copy of the
-- household id is a second thing that can disagree.
CREATE TABLE account_terms (
    -- One row per account, so the account id IS the key. Nothing references this
    -- table, so a surrogate id would only be a second thing to join on.
    account_id      UUID PRIMARY KEY REFERENCES accounts (id) ON DELETE CASCADE,

    -- A PERCENTAGE, matching liabilities.apr: 18.99 means 18.99%, never 0.1899.
    -- ComputePayoff divides by 100, so storing the fraction inflates nothing and
    -- silently reports a 19% card as very nearly interest-free.
    --
    -- The upper bound catches a fat finger, not an expensive loan. It is 200 and
    -- not 100 because payday credit really does exceed 100% APR, and rejecting a
    -- legitimate rate is a worse failure than accepting an absurd one.
    apr             NUMERIC(9, 4)
                        CHECK (apr IS NULL OR (apr >= 0 AND apr <= 200)),

    -- What the household actually pays each month.
    --
    -- NULL is a real state and must never be read as zero: zero means "paying
    -- nothing", which ComputePayoff correctly reports as NeverPaysOff. "We don't
    -- know" and "you are not paying this down" are different answers and the
    -- schema has to be able to tell them apart.
    minimum_payment NUMERIC(20, 4)
                        CHECK (minimum_payment IS NULL OR minimum_payment >= 0),

    -- The bill this debt is paid by.
    --
    -- WHY A LINK AND NOT A due_date COLUMN. A payment date is not a date, it is
    -- a RECURRENCE, and recurring_obligations already models one exactly:
    -- (anchor_date, interval_count, interval_unit), expanded in Postgres so that
    -- month ends clamp the way a person reads them and a bill anchored on the
    -- 31st never drifts off it. Duplicating that here would be a second, worse
    -- copy of arithmetic the repo deliberately keeps in one place.
    --
    -- It also makes the payment a first-class bill: it shows up on the schedule
    -- and in cash-flow forecasting alongside everything else, which a column on
    -- this table could never do.
    --
    -- Explicit rather than looked up by (account_id, source='manual'): an
    -- account may legitimately carry more than one manual obligation — a card
    -- with both a payment and an annual fee — and guessing which one is "the
    -- payment" is not something a schema should leave to a heuristic.
    --
    -- ON DELETE SET NULL: deleting the bill must not delete the APR.
    payment_obligation_id UUID REFERENCES recurring_obligations (id) ON DELETE SET NULL,

    -- Who last set these. A household member acting on shared accounts should be
    -- attributable; ON DELETE SET NULL because removing a user must not delete
    -- the terms every other member's payoff goals are computed from.
    updated_by      UUID REFERENCES users (id) ON DELETE SET NULL,

    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One row per obligation at most: a bill pays one debt.
CREATE UNIQUE INDEX account_terms_payment_obligation_idx
    ON account_terms (payment_obligation_id)
    WHERE payment_obligation_id IS NOT NULL;

-- A row saying nothing at all is not a state worth storing, and the write path
-- deletes rather than creating one — see DeleteAccountTerms. This constraint
-- keeps that an invariant of the table rather than a habit of one handler.
ALTER TABLE account_terms
    ADD CONSTRAINT account_terms_not_empty
    CHECK (apr IS NOT NULL
        OR minimum_payment IS NOT NULL
        OR payment_obligation_id IS NOT NULL);

-- +goose StatementBegin
DO $$
BEGIN
    EXECUTE 'CREATE TRIGGER account_terms_set_updated_at BEFORE UPDATE
             ON account_terms FOR EACH ROW EXECUTE FUNCTION set_updated_at()';
END $$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE IF EXISTS account_terms;
