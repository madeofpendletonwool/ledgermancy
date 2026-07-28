-- +goose Up
-- Everything the Investments surface needs that the schema could not already
-- answer: what tax treatment an account has, what the portfolio was worth
-- yesterday, and what a benchmark did over the same span.

-- --------------------------------------------------------------------------
-- 1. Account tax treatment
-- --------------------------------------------------------------------------

-- accounts.type/subtype come from Plaid and do not reliably distinguish tax
-- treatment, which is the thing that actually matters — both for grouping the
-- Investments page and for the account-aware retirement projection that builds
-- on this. Plaid returns a subtype of '401k' for a Roth 401k as often as for a
-- traditional one, and 'brokerage' covers everything from a taxable account to
-- a managed trust.
--
-- Deliberately NULLable. Existing accounts have no correct value and guessing
-- one would be exactly the quiet dishonesty this app rejects: a traditional-vs-
-- Roth mistake changes every retirement number downstream. A suggestion is
-- inferred from the Plaid subtype and shown for confirmation; only what the
-- user confirms is stored. Untagged accounts group under "Untagged" and are
-- excluded from tax-aware projections rather than silently defaulted.
ALTER TABLE accounts ADD COLUMN tax_treatment TEXT
    CHECK (tax_treatment IN (
        'taxable', 'trad_401k', 'roth_401k', 'trad_ira', 'roth_ira',
        '529', 'hsa', 'trust', 'other'
    ));

-- Whether somebody is paid to run the account. Nullable for the same reason:
-- unknown is a real state and must not read as "self-directed".
ALTER TABLE accounts ADD COLUMN is_managed BOOLEAN;

-- --------------------------------------------------------------------------
-- 2. Investment value over time
-- --------------------------------------------------------------------------

-- Return is not measurable without a history of values, and Plaid serves none:
-- /investments/holdings/get reports what is held *now*. So, exactly like
-- net_worth_snapshots, the figure is written down as time passes.
--
-- The consequence is load-bearing and must be surfaced in the UI: returns are
-- "since we started watching", never "since inception". A three-day-old install
-- has three days of history and must not be allowed to render an annualised
-- number off it.
CREATE TABLE investment_snapshots (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id   UUID NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    as_of        DATE NOT NULL,
    -- The whole account, uninvested cash included — the same basis
    -- ComputeNetWorth uses, so the two surfaces cannot disagree about what an
    -- investment account is worth.
    market_value NUMERIC(20, 4) NOT NULL,
    -- Summed over holdings that report one. NULL when no holding does, which is
    -- normal for an account Plaid gives no basis for.
    cost_basis   NUMERIC(20, 4),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- One row per account per day; re-running the job replaces the day's row.
    UNIQUE (account_id, as_of)
);

CREATE INDEX investment_snapshots_account_date_idx
    ON investment_snapshots (account_id, as_of);

-- --------------------------------------------------------------------------
-- 3. Benchmark prices
-- --------------------------------------------------------------------------

-- End-of-day closes for a small fixed set of benchmark tickers, so portfolio
-- return can be plotted against something. Same shape as the CPI and FX series
-- tables planned elsewhere — keep them consistent.
--
-- Populated by a job that is OFF by default: it is the app's first outbound
-- call to a host that is neither Plaid nor the configured AI provider, and the
-- README's "phones home to nothing but Plaid and (optionally) your AI provider"
-- claim has to stay true for anyone who has not opted in.
CREATE TABLE asset_prices (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    ticker     TEXT NOT NULL,
    as_of      DATE NOT NULL,
    close      NUMERIC(20, 4) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (ticker, as_of)
);

CREATE INDEX asset_prices_ticker_date_idx ON asset_prices (ticker, as_of);

-- +goose Down
DROP TABLE IF EXISTS asset_prices;
DROP TABLE IF EXISTS investment_snapshots;
ALTER TABLE accounts DROP COLUMN IF EXISTS is_managed;
ALTER TABLE accounts DROP COLUMN IF EXISTS tax_treatment;
