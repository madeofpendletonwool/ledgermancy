-- +goose Up
--
-- Per-account assumed real return.
--
-- Until now every investment account has compounded at the household's single
-- real_return_rate (projection_assumptions), which is the right DEFAULT but the
-- wrong RULE: a 529 on a 17-year horizon for a one-year-old, a brokerage, and a
-- money-market fund are not the same investment, and forcing them to one rate
-- silently misstates every projection that touches them. For this household it
-- meant a 529 projected at 3% real — conservative enough that the monthly
-- figure the app computed was meaningfully higher than a 529-appropriate 5–6%
-- would have been.
--
-- The engine already has the plumbing — networth.AccountPlan.RealReturnRate,
-- "zero means use the household rate" (retirement.go) — but it was wired to
-- nothing: no column, no population, and the college drawdown read the split
-- override instead of the plan. This migration adds the column; the query and
-- engine wiring land alongside it.
--
-- It lives on account_contributions because that table already holds the
-- per-account projection inputs (monthly contribution, beneficiary horizon,
-- employer match) — an assumed return is another one of those, and setting it
-- belongs in the same place setting those does. NULL means "use the household
-- rate", which is the behaviour every account has today, so existing rows and
-- accounts without a contribution plan are unchanged.
--
-- Numbered 00057, the next free number above 00056_likelihood_layer.sql. The
-- reservation table in docs/plans/README.md held 00057 for wave 7; an
-- out-of-wave migration takes the next number above everything applied as it
-- must (see 00050_merchant_logos.sql consuming doc 26's reservation for the
-- same reason), and wave 7 renumbers up by one when it ships. goose runs in
-- strict-ordering mode (db.go), so this must land above 00056.

ALTER TABLE account_contributions
    ADD COLUMN assumed_real_return NUMERIC(6, 4);

COMMENT ON COLUMN account_contributions.assumed_real_return IS
    'This account''s assumed REAL annual return as a fraction (0.06 = 6%), '
    'used by the retirement and college projections in place of the household '
    'real_return_rate. NULL means use the household rate. Stored REAL '
    '(inflation-adjusted) to match every other rate in the app — the drawdown '
    'and projection both work in today''s dollars.';

-- A real return is a fraction. Negative real returns are not something a
-- household sets for an investment (cash below inflation is modelled from its
-- APY, not typed here), and anything >= 100% is a typo. The light bound catches
-- both without constraining a genuine 0% (a cash-like investment held flat).
ALTER TABLE account_contributions
    ADD CONSTRAINT account_contributions_assumed_real_return_check
        CHECK (assumed_real_return IS NULL OR (assumed_real_return >= 0 AND assumed_real_return < 1));

-- +goose Down

ALTER TABLE account_contributions
    DROP CONSTRAINT IF EXISTS account_contributions_assumed_real_return_check;

ALTER TABLE account_contributions
    DROP COLUMN IF EXISTS assumed_real_return;
