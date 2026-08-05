-- +goose Up
--
-- Real-asset revaluation, depreciation, and directly-held bonds (doc 26).
--
-- Numbered 00051, not the 00050 doc 26 reserved. 00050 was taken by
-- 00050_merchant_logos.sql, an out-of-wave change that landed first, and goose
-- runs in strict-ordering mode: a second 00050 would fail every deployment at
-- boot. Doc 27's reservation moves up accordingly — see docs/plans/README.md.
--
-- The problem this solves: manual_assets.value is a number typed in once and
-- never revisited. For most households the home is the largest line on the
-- net-worth sheet and it is wrong within months. A directly-held savings bond
-- is worse, because its correct value is not an estimate — it is arithmetic
-- against published rates, and the app was reporting a frozen purchase price.

-- --------------------------------------------------------------------------
-- 1. Class-specific asset metadata
-- --------------------------------------------------------------------------

-- Kept in a side table rather than widening manual_assets, so a 'cash' or
-- 'collectible' asset carries no vehicle columns. One row per asset at most.
CREATE TABLE asset_details (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    manual_asset_id UUID NOT NULL UNIQUE REFERENCES manual_assets (id) ON DELETE CASCADE,

    -- Real estate
    address         TEXT,
    beds            NUMERIC(4,1),
    baths           NUMERIC(4,1),
    sqft            INT,
    lot_sqft        INT,

    -- Vehicle
    year            INT,
    make            TEXT,
    model           TEXT,
    trim            TEXT,
    mileage         INT,
    -- Used to age a vehicle's mileage between manual updates, so a curve run
    -- six months after the last odometer reading is not valuing a car the user
    -- has since driven 8,000 miles in.
    annual_mileage  INT,

    -- Bond. Enough to value the thing from first principles; nothing derived.
    bond_series     TEXT CHECK (bond_series IN ('i_savings','ee_savings','treasury','other')),
    issue_date      DATE,
    -- What was paid. For electronic savings bonds this equals face value; paper
    -- EE bonds were sold at HALF face, which is the single most common way a
    -- savings bond gets entered at twice its real cost.
    purchase_price  NUMERIC(20,4),
    face_value      NUMERIC(20,4),
    -- Marketable Treasuries only. Stored as a percentage (4.25 = 4.25%),
    -- matching liabilities.apr.
    coupon_rate     NUMERIC(9,4),
    maturity_date   DATE,
    -- Municipal interest is federally tax-exempt. Recorded so a future tax view
    -- has it; nothing in this migration's feature set computes tax.
    tax_exempt      BOOLEAN,

    condition       TEXT CHECK (condition IN ('excellent','good','fair','poor')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- --------------------------------------------------------------------------
-- 2. Published savings-bond rates
-- --------------------------------------------------------------------------

-- One row per series per six-month announcement period. This is reference
-- data, and its provenance is the whole point: each row carries the URL it came
-- from so any figure the app produces can be walked back to treasurydirect.gov
-- by hand.
--
-- Doc 15 refused to bundle a historical equity-return series on the grounds
-- that it would be "a transcribed table of numbers nobody can verify". This
-- table is different in the way that matters: each row is a single number,
-- announced on one date by one issuer, checkable against one page. The rows are
-- editable through the API, so a household that distrusts the seed can replace
-- it.
--
-- Rates are stored as PERCENTAGES (3.40 means 3.40%), matching liabilities.apr
-- and asset_details.coupon_rate. Not as decimal fractions.
CREATE TABLE savings_bond_rates (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    series         TEXT NOT NULL CHECK (series IN ('i_savings','ee_savings')),
    -- The six-month period this rate was announced for. Normally a May 1 or
    -- Nov 1 date; the very first I bond period starts 1998-09-01, which is why
    -- lookup is "greatest period_start <= date" rather than arithmetic on a
    -- May/Nov grid.
    period_start   DATE NOT NULL,
    -- I bonds: both. EE bonds: fixed only, inflation_rate NULL.
    fixed_rate     NUMERIC(9,6) NOT NULL,
    inflation_rate NUMERIC(9,6),
    source_url     TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (series, period_start)
);

-- A rate row with no provenance is exactly the "transcribed table nobody can
-- verify" this design set out to avoid, so the constraint is in the schema
-- rather than in a comment.
ALTER TABLE savings_bond_rates
    ADD CONSTRAINT savings_bond_rates_source_url_present CHECK (source_url <> '');

-- Series I: fixed rate + semiannual inflation rate, every period since the
-- series launched in September 1998.
-- Source: https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/
INSERT INTO savings_bond_rates (series, period_start, fixed_rate, inflation_rate, source_url)
VALUES
    ('i_savings', '1998-09-01', 3.40,  0.62, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '1998-11-01', 3.30,  0.86, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '1999-05-01', 3.30,  0.86, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '1999-11-01', 3.40,  1.76, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2000-05-01', 3.60,  1.91, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2000-11-01', 3.40,  1.52, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2001-05-01', 3.00,  1.44, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2001-11-01', 2.00,  1.19, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2002-05-01', 2.00,  0.28, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2002-11-01', 1.60,  1.23, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2003-05-01', 1.10,  1.77, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2003-11-01', 1.10,  0.54, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2004-05-01', 1.00,  1.19, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2004-11-01', 1.00,  1.33, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2005-05-01', 1.20,  1.79, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2005-11-01', 1.00,  2.85, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2006-05-01', 1.40,  0.50, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2006-11-01', 1.40,  1.55, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2007-05-01', 1.30,  1.21, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2007-11-01', 1.20,  1.53, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2008-05-01', 0.00,  2.42, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2008-11-01', 0.70,  2.46, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2009-05-01', 0.10, -2.78, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2009-11-01', 0.30,  1.53, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2010-05-01', 0.20,  0.77, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2010-11-01', 0.00,  0.37, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2011-05-01', 0.00,  2.30, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2011-11-01', 0.00,  1.53, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2012-05-01', 0.00,  1.10, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2012-11-01', 0.00,  0.88, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2013-05-01', 0.00,  0.59, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2013-11-01', 0.20,  0.59, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2014-05-01', 0.10,  0.92, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2014-11-01', 0.00,  0.74, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2015-05-01', 0.00, -0.80, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2015-11-01', 0.10,  0.77, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2016-05-01', 0.10,  0.08, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2016-11-01', 0.00,  1.38, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2017-05-01', 0.00,  0.98, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2017-11-01', 0.10,  1.24, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2018-05-01', 0.30,  1.11, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2018-11-01', 0.50,  1.16, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2019-05-01', 0.50,  0.70, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2019-11-01', 0.20,  1.01, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2020-05-01', 0.00,  0.53, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2020-11-01', 0.00,  0.84, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2021-05-01', 0.00,  1.77, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2021-11-01', 0.00,  3.56, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2022-05-01', 0.00,  4.81, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2022-11-01', 0.40,  3.24, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2023-05-01', 0.90,  1.69, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2023-11-01', 1.30,  1.97, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2024-05-01', 1.30,  1.48, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2024-11-01', 1.20,  0.95, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2025-05-01', 1.10,  1.43, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2025-11-01', 0.90,  1.56, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/'),
    ('i_savings', '2026-05-01', 0.90,  1.67, 'https://www.treasurydirect.gov/savings-bonds/i-bonds/i-bonds-interest-rates/');

-- Series EE: the fixed rate set at issue, which the bond then earns for its
-- first 20 years. Seeded from May 2005 only, and that boundary is deliberate —
-- EE bonds issued before May 2005 earned variable market-based rates (90% of
-- 5-year Treasury averages, reset every six months, with guarantee periods that
-- changed several times). There is no rate table that values those, so the app
-- refuses them rather than applying a fixed-rate model that does not describe
-- the instrument.
-- Source: https://www.treasurydirect.gov/savings-bonds/ee-bonds/may-2005-and-later/
INSERT INTO savings_bond_rates (series, period_start, fixed_rate, inflation_rate, source_url)
VALUES
    ('ee_savings', '2005-05-01', 3.50, NULL, 'https://www.treasurydirect.gov/savings-bonds/ee-bonds/may-2005-and-later/'),
    ('ee_savings', '2005-11-01', 3.20, NULL, 'https://www.treasurydirect.gov/savings-bonds/ee-bonds/may-2005-and-later/'),
    ('ee_savings', '2006-05-01', 3.70, NULL, 'https://www.treasurydirect.gov/savings-bonds/ee-bonds/may-2005-and-later/'),
    ('ee_savings', '2006-11-01', 3.60, NULL, 'https://www.treasurydirect.gov/savings-bonds/ee-bonds/may-2005-and-later/'),
    ('ee_savings', '2007-05-01', 3.40, NULL, 'https://www.treasurydirect.gov/savings-bonds/ee-bonds/may-2005-and-later/'),
    ('ee_savings', '2007-11-01', 3.00, NULL, 'https://www.treasurydirect.gov/savings-bonds/ee-bonds/may-2005-and-later/'),
    ('ee_savings', '2008-05-01', 1.40, NULL, 'https://www.treasurydirect.gov/savings-bonds/ee-bonds/may-2005-and-later/'),
    ('ee_savings', '2008-11-01', 1.30, NULL, 'https://www.treasurydirect.gov/savings-bonds/ee-bonds/may-2005-and-later/'),
    ('ee_savings', '2009-05-01', 0.70, NULL, 'https://www.treasurydirect.gov/savings-bonds/ee-bonds/may-2005-and-later/'),
    ('ee_savings', '2009-11-01', 1.20, NULL, 'https://www.treasurydirect.gov/savings-bonds/ee-bonds/may-2005-and-later/'),
    ('ee_savings', '2010-05-01', 1.40, NULL, 'https://www.treasurydirect.gov/savings-bonds/ee-bonds/may-2005-and-later/'),
    ('ee_savings', '2010-11-01', 0.60, NULL, 'https://www.treasurydirect.gov/savings-bonds/ee-bonds/may-2005-and-later/'),
    ('ee_savings', '2011-05-01', 1.10, NULL, 'https://www.treasurydirect.gov/savings-bonds/ee-bonds/may-2005-and-later/'),
    ('ee_savings', '2011-11-01', 0.60, NULL, 'https://www.treasurydirect.gov/savings-bonds/ee-bonds/may-2005-and-later/'),
    ('ee_savings', '2012-05-01', 0.60, NULL, 'https://www.treasurydirect.gov/savings-bonds/ee-bonds/may-2005-and-later/'),
    ('ee_savings', '2012-11-01', 0.20, NULL, 'https://www.treasurydirect.gov/savings-bonds/ee-bonds/may-2005-and-later/'),
    ('ee_savings', '2013-05-01', 0.20, NULL, 'https://www.treasurydirect.gov/savings-bonds/ee-bonds/may-2005-and-later/'),
    ('ee_savings', '2013-11-01', 0.10, NULL, 'https://www.treasurydirect.gov/savings-bonds/ee-bonds/may-2005-and-later/'),
    ('ee_savings', '2014-05-01', 0.50, NULL, 'https://www.treasurydirect.gov/savings-bonds/ee-bonds/may-2005-and-later/'),
    ('ee_savings', '2014-11-01', 0.10, NULL, 'https://www.treasurydirect.gov/savings-bonds/ee-bonds/may-2005-and-later/'),
    ('ee_savings', '2015-05-01', 0.30, NULL, 'https://www.treasurydirect.gov/savings-bonds/ee-bonds/may-2005-and-later/'),
    ('ee_savings', '2015-11-01', 0.10, NULL, 'https://www.treasurydirect.gov/savings-bonds/ee-bonds/may-2005-and-later/'),
    ('ee_savings', '2016-05-01', 0.10, NULL, 'https://www.treasurydirect.gov/savings-bonds/ee-bonds/may-2005-and-later/'),
    ('ee_savings', '2016-11-01', 0.10, NULL, 'https://www.treasurydirect.gov/savings-bonds/ee-bonds/may-2005-and-later/'),
    ('ee_savings', '2017-05-01', 0.10, NULL, 'https://www.treasurydirect.gov/savings-bonds/ee-bonds/may-2005-and-later/'),
    ('ee_savings', '2017-11-01', 0.10, NULL, 'https://www.treasurydirect.gov/savings-bonds/ee-bonds/may-2005-and-later/'),
    ('ee_savings', '2018-05-01', 0.10, NULL, 'https://www.treasurydirect.gov/savings-bonds/ee-bonds/may-2005-and-later/'),
    ('ee_savings', '2018-11-01', 0.10, NULL, 'https://www.treasurydirect.gov/savings-bonds/ee-bonds/may-2005-and-later/'),
    ('ee_savings', '2019-05-01', 0.10, NULL, 'https://www.treasurydirect.gov/savings-bonds/ee-bonds/may-2005-and-later/'),
    ('ee_savings', '2019-11-01', 0.10, NULL, 'https://www.treasurydirect.gov/savings-bonds/ee-bonds/may-2005-and-later/'),
    ('ee_savings', '2020-05-01', 0.10, NULL, 'https://www.treasurydirect.gov/savings-bonds/ee-bonds/may-2005-and-later/'),
    ('ee_savings', '2020-11-01', 0.10, NULL, 'https://www.treasurydirect.gov/savings-bonds/ee-bonds/may-2005-and-later/'),
    ('ee_savings', '2021-05-01', 0.10, NULL, 'https://www.treasurydirect.gov/savings-bonds/ee-bonds/may-2005-and-later/'),
    ('ee_savings', '2021-11-01', 0.10, NULL, 'https://www.treasurydirect.gov/savings-bonds/ee-bonds/may-2005-and-later/'),
    ('ee_savings', '2022-05-01', 0.10, NULL, 'https://www.treasurydirect.gov/savings-bonds/ee-bonds/may-2005-and-later/'),
    ('ee_savings', '2022-11-01', 2.10, NULL, 'https://www.treasurydirect.gov/savings-bonds/ee-bonds/may-2005-and-later/'),
    ('ee_savings', '2023-05-01', 2.50, NULL, 'https://www.treasurydirect.gov/savings-bonds/ee-bonds/may-2005-and-later/'),
    ('ee_savings', '2023-11-01', 2.70, NULL, 'https://www.treasurydirect.gov/savings-bonds/ee-bonds/may-2005-and-later/'),
    ('ee_savings', '2024-05-01', 2.70, NULL, 'https://www.treasurydirect.gov/savings-bonds/ee-bonds/may-2005-and-later/'),
    ('ee_savings', '2024-11-01', 2.60, NULL, 'https://www.treasurydirect.gov/savings-bonds/ee-bonds/may-2005-and-later/'),
    ('ee_savings', '2025-05-01', 2.70, NULL, 'https://www.treasurydirect.gov/savings-bonds/ee-bonds/may-2005-and-later/'),
    ('ee_savings', '2025-11-01', 2.50, NULL, 'https://www.treasurydirect.gov/savings-bonds/ee-bonds/may-2005-and-later/'),
    ('ee_savings', '2026-05-01', 2.40, NULL, 'https://www.treasurydirect.gov/savings-bonds/ee-bonds/may-2005-and-later/');

-- --------------------------------------------------------------------------
-- 3. Valuation history
-- --------------------------------------------------------------------------

-- Every valuation, dated. manual_assets.value stays as the CURRENT value so no
-- existing net-worth query changes; this is the trend behind it. Writing a
-- valuation updates both, in one transaction.
CREATE TABLE asset_valuations (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    manual_asset_id UUID NOT NULL REFERENCES manual_assets (id) ON DELETE CASCADE,
    value           NUMERIC(20,4) NOT NULL,
    as_of           DATE NOT NULL,
    -- 'manual'    — the user typed it.
    -- 'estimated' — computed by the app from its own inputs (a bond's accrual
    --               against the rate table; a confirmed depreciation curve).
    -- 'api'       — from an external valuation source.
    source          TEXT NOT NULL CHECK (source IN ('manual','estimated','api')),
    note            TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (manual_asset_id, as_of)
);

CREATE INDEX asset_valuations_asset_idx ON asset_valuations (manual_asset_id, as_of DESC);

-- Backfill, so every pre-existing asset has a history starting from what the
-- user originally entered rather than a gap. Exactly one row per asset,
-- matching its current value and as_of.
INSERT INTO asset_valuations (manual_asset_id, value, as_of, source, note)
SELECT id, value, as_of, 'manual', 'Value as first entered.'
FROM manual_assets;

-- --------------------------------------------------------------------------
-- 4. Linking an asset to the loan secured against it
-- --------------------------------------------------------------------------

-- Equity = asset value − linked liability balance. ON DELETE SET NULL: unlinking
-- a closed account must not delete the house.
--
-- Equity is a DERIVED DISPLAY FIGURE and must never enter the net-worth sum.
-- ComputeNetWorth already counts the asset as an asset and the loan as a
-- liability; adding equity on top would count the asset twice.
ALTER TABLE manual_assets ADD COLUMN loan_account_id UUID
    REFERENCES accounts (id) ON DELETE SET NULL;

CREATE INDEX manual_assets_loan_account_idx ON manual_assets (loan_account_id)
    WHERE loan_account_id IS NOT NULL;

-- manual_assets.kind gains 'bond' with no schema change: the column is
-- TEXT NOT NULL DEFAULT 'other' and the allowed values were only ever a comment
-- (00004:115). Restate the vocabulary here, since that comment is now stale.
COMMENT ON COLUMN manual_assets.kind IS
    'home | vehicle | cash | collectible | bond | other | debt';

-- +goose Down
COMMENT ON COLUMN manual_assets.kind IS NULL;
DROP INDEX IF EXISTS manual_assets_loan_account_idx;
ALTER TABLE manual_assets DROP COLUMN IF EXISTS loan_account_id;
DROP TABLE IF EXISTS asset_valuations;
DROP TABLE IF EXISTS savings_bond_rates;
DROP TABLE IF EXISTS asset_details;
