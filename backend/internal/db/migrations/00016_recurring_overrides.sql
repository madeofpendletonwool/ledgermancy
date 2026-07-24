-- +goose Up
-- A per-household suppression list for the recurring/subscription detector.
-- Detection is a heuristic on the shape of the charge history, so it will
-- sometimes flag a coincidence — three grocery runs in a month, a short burst of
-- charges at one merchant. When it does, the user marks the merchant "not
-- recurring" and its merchant_key drops out of everything that reads the
-- recurring detector at once: the Spending recurring table, the new_recurring
-- and subscription insight producers, the monthly recap's recurring total, and
-- the chat tool. The exclusion lives in GetRecurringMerchants itself (a NOT
-- EXISTS on this table, keyed by the household_id the query already takes), so
-- there is a single place suppression is enforced.
--
-- Presence of a row = suppressed; there is no state beyond that. The label is
-- captured at suppression time so the "restore" UI can name the merchant without
-- re-deriving it from transactions.
CREATE TABLE recurring_overrides (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id   UUID        NOT NULL REFERENCES households (id) ON DELETE CASCADE,
    merchant_key   TEXT        NOT NULL,
    merchant_label TEXT        NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (household_id, merchant_key)
);

-- +goose Down
DROP TABLE IF EXISTS recurring_overrides;
