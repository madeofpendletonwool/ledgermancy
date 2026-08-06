-- +goose Up
--
-- The allocation planner (doc 32): splitting a lump sum and a monthly surplus
-- across Roth / 529 / brokerage / debt / emergency fund, with contribution caps,
-- eligibility, per-bucket projections and goal mapping.
--
-- Numbered 00055, which is what docs/plans/README.md's reservation table
-- assigns. The doc's own text still says 00053; that number was consumed by
-- 00053_manual_accounts.sql when doc 30 renumbered up from its reserved 00047,
-- and doc 31 landed at 00054 for the same reason. goose runs in strict-ordering
-- mode (db.go), so a number below the highest applied migration would refuse to
-- start every instance that has run them.
--
-- Nothing here stores a RESULT. Every column is an input the household gave us
-- or a decision it made; per-bucket projections are recomputed against the live
-- baseline every time a plan is opened, because a saved projection is a figure
-- that quietly stops being true.

-- --------------------------------------------------------------------------
-- 1. accounts.deposit_apy — the cash-drag input
-- --------------------------------------------------------------------------
--
-- Plaid does not serve deposit yields reliably, so this is user-entered. NULL
-- means UNKNOWN, and the cash-drag detector stays silent on an unknown rather
-- than assuming zero: "this account earns nothing" and "nobody has told us what
-- this account earns" are different facts, and collapsing them would tell a
-- household its high-yield savings account was costing it money.
--
-- A percent, matching every other rate a user types in this app (liabilities.apr
-- stores 6.25 for 6.25%): 4.50 means 4.50% a year.

ALTER TABLE accounts ADD COLUMN deposit_apy NUMERIC(5, 2);

COMMENT ON COLUMN accounts.deposit_apy IS
    'User-entered deposit yield as a PERCENT (4.50 = 4.50%). NULL = unknown, never zero.';

-- --------------------------------------------------------------------------
-- 2. projection_assumptions.college_inflation_rate
-- --------------------------------------------------------------------------
--
-- College costs have run at roughly 5-6%/yr historically, well above general
-- CPI. Inflating a tuition target at the household's general inflation
-- assumption understates it by several years of compounding, and it understates
-- in the flattering direction.
--
-- It lives on projection_assumptions rather than in code so a saved scenario
-- sees the rate it was built with, and so a household that disagrees with 5.5%
-- can say so. A percent, like deposit_apy above.

ALTER TABLE projection_assumptions
    ADD COLUMN college_inflation_rate NUMERIC(5, 2) NOT NULL DEFAULT 5.5;

-- --------------------------------------------------------------------------
-- 3. goals — a 'college' kind, and the years it pays for
-- --------------------------------------------------------------------------
--
-- READ THIS BEFORE ASSUMING THE DROP BELOW IS A NO-OP EDIT. goals.kind has had
-- NO check constraint since 00012_goals.sql:15 — it is a plain `TEXT NOT NULL`
-- with a comment naming the two intended values, and the vocabulary has lived
-- in goal_handlers.go (goalKindSavings / goalKindPayoff) as a convention. The
-- DROP is kept only for idempotency; the ADD is a NEW constraint tightening a
-- previously free column.
--
-- Two consequences. This migration FAILS if any live row holds a kind outside
-- the set — which is the correct outcome, because the app has never written one
-- and a row that has one is a fact somebody needs to see rather than a row to
-- quietly widen the constraint around. And 'savings' | 'debt_payoff' stop being
-- a Go convention and become a database invariant, which they should have been
-- from the start.

ALTER TABLE goals DROP CONSTRAINT IF EXISTS goals_kind_check;
ALTER TABLE goals ADD  CONSTRAINT goals_kind_check
    CHECK (kind IN ('savings', 'debt_payoff', 'college'));

-- College is FOUR YEARS OF SPENDING, not a bill on the first day, and the count
-- is per goal because community-college transfers and five-year programmes
-- exist. The engine has no business assuming; four is the default, not the law.
--
-- For a college goal, target_amount is ONE YEAR'S cost in today's dollars. The
-- engine inflates each of the years separately and draws them down — see
-- allocation/college.go. Storing the four-year total instead would make the
-- per-year shortfall ("funded through sophomore year") impossible to compute.
ALTER TABLE goals ADD COLUMN college_years SMALLINT NOT NULL DEFAULT 4
    CHECK (college_years BETWEEN 1 AND 10);

COMMENT ON COLUMN goals.college_years IS
    'Years of study a college goal funds. target_amount is ONE year in today''s dollars.';

-- --------------------------------------------------------------------------
-- 4. households — MAGI, for the Roth eligibility check
-- --------------------------------------------------------------------------
--
-- Doc 31 shipped households.filing_status specifically so this doc's
-- eligibility check would have something to key on. It also needs an income
-- figure, and the app does not have one: MAGI is not modified AGI is not gross
-- income, and a full MAGI needs a tax return. Doc 23's paystub data gets closer
-- but does not get there.
--
-- So MAGI is USER-ENTERED and OPTIONAL, and absent means `unknown` — the plan
-- is projected with a labelled "assumes you're eligible to contribute" caveat
-- rather than silently assuming the flattering answer.
--
-- magi_tax_year is what makes the figure honest a year later. A MAGI is a
-- statement about one tax year; carrying 2026's number into 2028's eligibility
-- check would be exactly the silent staleness limits.go refuses for IRS limits.
-- When the stored year is not the year being checked, the value is treated as
-- absent and eligibility is `unknown`.
ALTER TABLE households
    ADD COLUMN magi          NUMERIC(20, 4),
    ADD COLUMN magi_tax_year INT;

COMMENT ON COLUMN households.magi IS
    'User-entered modified AGI for magi_tax_year. NULL, or a stale year, = unknown (never "eligible").';

-- --------------------------------------------------------------------------
-- 5. allocation_plans — saved splits
-- --------------------------------------------------------------------------
--
-- Schema-versioned like doc 28's scenarios, and results are deliberately NOT
-- stored: opening a plan recomputes it against the live baseline, so a plan
-- saved in March reads against today's balances rather than March's.
--
-- MONEY INSIDE THESE JSONB BLOBS IS A STRING, NEVER A JSON NUMBER, and this is
-- a real hole in the continuity rule rather than a style preference. The rule
-- says money is cast to text in SQL and never travels as a JSON number, and
-- export.go:215 enforces it for `numeric` COLUMNS — but it cannot reach inside
-- a jsonb column, which `normalise` passes through as json.RawMessage. So
-- {"lump": 30000.50} would leave the export as a JSON number and be parsed by
-- whatever reads it as a float64. Every money field in `inputs` and
-- `assumptions` is therefore a StringFixed(2) string, marshalled through
-- decimal.Decimal, which round-trips strings exactly. Counts (horizon in years,
-- input_version) stay numbers: they are not money and are exact in float64.
--
-- input_version is load-bearing. Saved plans are kept for years; version from
-- day one and migrate forward explicitly rather than guessing at an old shape.

CREATE TABLE allocation_plans (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id  UUID NOT NULL REFERENCES households (id) ON DELETE CASCADE,
    -- SET NULL rather than CASCADE, matching advisor_threads: a departing
    -- member's saved plan belongs to the household it was built for.
    created_by    UUID REFERENCES users (id) ON DELETE SET NULL,
    name          TEXT NOT NULL,
    inputs        JSONB NOT NULL,        -- {lump, monthly, horizon_years, target, buckets}
    input_version INT   NOT NULL DEFAULT 1,
    assumptions   JSONB NOT NULL,        -- snapshot at save time (doc 28's rule)
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX allocation_plans_household_idx ON allocation_plans (household_id, updated_at DESC);

-- +goose Down

DROP TABLE IF EXISTS allocation_plans;

ALTER TABLE households DROP COLUMN IF EXISTS magi_tax_year;
ALTER TABLE households DROP COLUMN IF EXISTS magi;

ALTER TABLE goals DROP COLUMN IF EXISTS college_years;
ALTER TABLE goals DROP CONSTRAINT IF EXISTS goals_kind_check;

ALTER TABLE projection_assumptions DROP COLUMN IF EXISTS college_inflation_rate;

ALTER TABLE accounts DROP COLUMN IF EXISTS deposit_apy;
