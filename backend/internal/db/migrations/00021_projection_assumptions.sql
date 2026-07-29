-- +goose Up
-- Inputs to the retirement projection. Nothing here is a result: every column
-- is something the user told us, and every one of them is rendered beside the
-- curve it produced. project.go's comment ("a forecast whose workings are
-- hidden is not something anyone should plan around") is the rule, and it binds
-- harder here — somebody will decide when to stop working off this output.

-- --------------------------------------------------------------------------
-- 1. Per-household assumptions
-- --------------------------------------------------------------------------

CREATE TABLE projection_assumptions (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id UUID NOT NULL UNIQUE REFERENCES households (id) ON DELETE CASCADE,

    -- REAL, not nominal, and the naming is deliberate. Mixing a nominal return
    -- with today's dollars is the most common way a retirement projection lies,
    -- and it always lies in the flattering direction. Everything downstream is
    -- expressed in today's dollars; inflation_rate is stored so the UI can
    -- label the basis and so a future nominal toggle (TODO #12's CPI series)
    -- has something to convert with, not because the engine grosses up.
    real_return_rate NUMERIC(6, 4) NOT NULL DEFAULT 0.05,
    inflation_rate   NUMERIC(6, 4) NOT NULL DEFAULT 0.03,

    -- The withdrawal rate the "supported spending" figure is drawn at. 4% is
    -- the convention, not a law, which is exactly why it is editable.
    withdrawal_rate NUMERIC(6, 4) NOT NULL DEFAULT 0.04,

    -- Nullable because "I have not decided" is a real answer. A projection
    -- still runs without a target age; only the required-savings-rate solve
    -- needs one.
    target_retirement_age INT,
    current_age           INT,

    -- Expected Social Security or pension income, in today's dollars, starting
    -- at ss_start_age. Counted toward supported spending from that age onward
    -- and not one year sooner.
    annual_ss_income NUMERIC(20, 4),
    ss_start_age     INT,

    -- Annual spending the household is planning to support in retirement. NULL
    -- means "use what we actually spend", which the app can compute from
    -- GetSpendingSummary — do not make somebody guess a number we already know.
    target_annual_spending NUMERIC(20, 4),

    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- --------------------------------------------------------------------------
-- 2. Per-account contribution plan
-- --------------------------------------------------------------------------

-- What is being paid into each account, and what an employer adds. Drives both
-- the compounding and the limit headroom the UI shows.
--
-- Separate from `accounts` rather than more columns on it: accounts is written
-- by every Plaid sync, and a user's savings plan must not be at the mercy of an
-- UpsertAccount column list.
CREATE TABLE account_contributions (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id UUID NOT NULL UNIQUE REFERENCES accounts (id) ON DELETE CASCADE,

    monthly_contribution NUMERIC(20, 4) NOT NULL DEFAULT 0
        CHECK (monthly_contribution >= 0),

    -- Percentage OF SALARY, not of the employee's contribution — the two are
    -- routinely confused and they give different answers. Stored as a fraction
    -- (0.05 = 5%). annual_salary is what it applies to; both must be present
    -- for a match to be counted at all.
    employer_match_pct   NUMERIC(6, 4) CHECK (employer_match_pct >= 0),
    annual_salary        NUMERIC(20, 4) CHECK (annual_salary >= 0),
    -- Annual cap on the employer's contribution, applied after the percentage.
    employer_match_limit NUMERIC(20, 4) CHECK (employer_match_limit >= 0),

    -- A 529 does not run to retirement: it runs to the year the beneficiary
    -- starts college. Projecting one on the retirement horizon overstates the
    -- household's retirement assets by the entire balance.
    beneficiary_current_age INT CHECK (beneficiary_current_age >= 0),
    beneficiary_target_age  INT CHECK (beneficiary_target_age >= 0),

    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX account_contributions_account_idx ON account_contributions (account_id);

-- +goose Down
DROP TABLE IF EXISTS account_contributions;
DROP TABLE IF EXISTS projection_assumptions;
