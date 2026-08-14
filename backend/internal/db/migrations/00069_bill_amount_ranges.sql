-- +goose Up
-- Expected amount RANGES on the bill calendar.
--
-- Until now an obligation carried one `amount`, and the matcher decided whether a
-- charge was "the bill" with a hardcoded ±25% band on the category branch. That
-- band is the app guessing. A range is the user stating it: "the phone bill runs
-- $40 to $60", which is both a better matching rule and — the point of MAD-120 —
-- the only way the app can say "this month it was $90" with any authority.
--
-- `amount` stays and keeps its meaning: the expected/typical figure, and the one
-- the balance projection and monthly-estimate arithmetic use. A range is not a
-- second amount, it is a tolerance around this one, so nothing downstream of the
-- projection has to learn about ranges to keep working.
--
-- Both-or-neither is enforced rather than allowing a lone bound. A half-open
-- range ("at least $40") reads as a rule but cannot answer "was this the bill?"
-- in the direction that matters, and every surface would need a second empty
-- state for it. NULL/NULL — the default, and what every existing row keeps — means
-- "no range stated", and the ±25% heuristic continues to apply to those.
ALTER TABLE recurring_obligations
    ADD COLUMN amount_min NUMERIC(20,4),
    ADD COLUMN amount_max NUMERIC(20,4);

ALTER TABLE recurring_obligations
    ADD CONSTRAINT recurring_obligations_amount_range
        CHECK (
            (amount_min IS NULL AND amount_max IS NULL)
            OR (amount_min IS NOT NULL AND amount_max IS NOT NULL
                AND amount_min > 0
                AND amount_min <= amount_max)
        );

COMMENT ON COLUMN recurring_obligations.amount_min IS
    'Low end of the amount the household expects this bill to land at. NULL '
    'together with amount_max means no range was stated and the matcher falls '
    'back to its +/-25% band around amount.';
COMMENT ON COLUMN recurring_obligations.amount_max IS
    'High end of the expected amount. A charge inside the payment window but '
    'outside [amount_min, amount_max] does not mark the occurrence paid; it '
    'raises the bill_out_of_range insight instead.';

-- +goose Down
ALTER TABLE recurring_obligations
    DROP CONSTRAINT IF EXISTS recurring_obligations_amount_range;
ALTER TABLE recurring_obligations DROP COLUMN IF EXISTS amount_min;
ALTER TABLE recurring_obligations DROP COLUMN IF EXISTS amount_max;
