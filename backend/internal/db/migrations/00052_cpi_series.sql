-- +goose Up
--
-- CPI-U, the price index behind inflation-adjusted ("real") views (doc 27).
--
-- Numbered 00052, not the 00057 the reservation table in docs/plans/README.md
-- named. That reservation was allocated on the assumption that wave 6/7's
-- 00052-00056 would land first, but wave 5 ships first, and goose runs in
-- strict-ordering mode: taking 00057 now would kill all five of those
-- reservations outright. Taking the next free number above everything applied
-- (00051_asset_revaluation.sql) shifts them by exactly one instead. The table
-- has been updated to match.
--
-- The problem this solves: every long-horizon comparison in the app is in
-- NOMINAL dollars. "Net worth up 8% this year" in a 6% inflation year is 2%
-- real growth, and until now the app had no way to say so. That is the same
-- class of arithmetic dishonesty it already rejects everywhere else.

-- --------------------------------------------------------------------------
-- The series
-- --------------------------------------------------------------------------

-- Same shape as asset_prices (doc 14) and any future fx_rates (doc 29) --
-- period, value, nothing derived. Keep the three consistent; they are the same
-- kind of thing.
--
-- NOT household-scoped. CPI-U is a public national series, identical for
-- everyone, and scoping it per household would mean 200 duplicate rows per
-- install saying the same thing.
CREATE TABLE cpi_series (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Always the first of the month. The index describes the whole month, so a
    -- mid-month date would imply a precision the series does not have.
    period      DATE NOT NULL UNIQUE CHECK (EXTRACT(DAY FROM period) = 1),
    -- CPI-U for All Urban Consumers, U.S. city average, all items, NOT
    -- seasonally adjusted (BLS series CUUR0000SA0). 1982-84 = 100.
    --
    -- Not seasonally adjusted is the deliberate choice: the SA series is
    -- revised every year for five years running as BLS re-estimates its
    -- seasonal factors, so a deflated figure a user saw last month would
    -- quietly change. NSA is the published index of record and does not move
    -- once released.
    index_value NUMERIC(12,4) NOT NULL CHECK (index_value > 0),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE cpi_series IS
    'CPI-U, U.S. city average, all items, NSA (BLS CUUR0000SA0). 1982-84 = 100.';

-- --------------------------------------------------------------------------
-- Seed
-- --------------------------------------------------------------------------

-- Committed rather than fetched on first boot, and that is the whole reason the
-- outbound fetch can default OFF. A self-hosted install with no route to the
-- internet still gets REAL deflation from real published numbers; the job, when
-- an operator turns it on, only ever fetches the tail.
--
-- January 2010 onward: sixteen years, comfortably past the fifteen doc 27 asks
-- for, and it covers every household history this app can have (Plaid serves at
-- most 24 months of transactions).
--
-- Source: https://data.bls.gov/timeseries/CUUR0000SA0
--
-- OCTOBER 2025 IS ABSENT, and it is absent on purpose. BLS never published it:
-- the 2025 lapse in appropriations stopped collection, and the agency has said
-- the month will not be estimated after the fact. There is no honest value to
-- put here. Deflation of a figure dated October 2025 therefore fails loudly
-- rather than interpolating between September and November -- see
-- reporting.Real, which returns an error for a missing index and never passes
-- the nominal number through. Interpolating would have been within 0.2% and
-- would still have been an invented number in a feature whose entire purpose is
-- to stop inventing numbers.
INSERT INTO cpi_series (period, index_value)
VALUES
    -- 2010
    ('2010-01-01', 216.687),
    ('2010-02-01', 216.741),
    ('2010-03-01', 217.631),
    ('2010-04-01', 218.009),
    ('2010-05-01', 218.178),
    ('2010-06-01', 217.965),
    ('2010-07-01', 218.011),
    ('2010-08-01', 218.312),
    ('2010-09-01', 218.439),
    ('2010-10-01', 218.711),
    ('2010-11-01', 218.803),
    ('2010-12-01', 219.179),
    -- 2011
    ('2011-01-01', 220.223),
    ('2011-02-01', 221.309),
    ('2011-03-01', 223.467),
    ('2011-04-01', 224.906),
    ('2011-05-01', 225.964),
    ('2011-06-01', 225.722),
    ('2011-07-01', 225.922),
    ('2011-08-01', 226.545),
    ('2011-09-01', 226.889),
    ('2011-10-01', 226.421),
    ('2011-11-01', 226.230),
    ('2011-12-01', 225.672),
    -- 2012
    ('2012-01-01', 226.665),
    ('2012-02-01', 227.663),
    ('2012-03-01', 229.392),
    ('2012-04-01', 230.085),
    ('2012-05-01', 229.815),
    ('2012-06-01', 229.478),
    ('2012-07-01', 229.104),
    ('2012-08-01', 230.379),
    ('2012-09-01', 231.407),
    ('2012-10-01', 231.317),
    ('2012-11-01', 230.221),
    ('2012-12-01', 229.601),
    -- 2013
    ('2013-01-01', 230.280),
    ('2013-02-01', 232.166),
    ('2013-03-01', 232.773),
    ('2013-04-01', 232.531),
    ('2013-05-01', 232.945),
    ('2013-06-01', 233.504),
    ('2013-07-01', 233.596),
    ('2013-08-01', 233.877),
    ('2013-09-01', 234.149),
    ('2013-10-01', 233.546),
    ('2013-11-01', 233.069),
    ('2013-12-01', 233.049),
    -- 2014
    ('2014-01-01', 233.916),
    ('2014-02-01', 234.781),
    ('2014-03-01', 236.293),
    ('2014-04-01', 237.072),
    ('2014-05-01', 237.900),
    ('2014-06-01', 238.343),
    ('2014-07-01', 238.250),
    ('2014-08-01', 237.852),
    ('2014-09-01', 238.031),
    ('2014-10-01', 237.433),
    ('2014-11-01', 236.151),
    ('2014-12-01', 234.812),
    -- 2015
    ('2015-01-01', 233.707),
    ('2015-02-01', 234.722),
    ('2015-03-01', 236.119),
    ('2015-04-01', 236.599),
    ('2015-05-01', 237.805),
    ('2015-06-01', 238.638),
    ('2015-07-01', 238.654),
    ('2015-08-01', 238.316),
    ('2015-09-01', 237.945),
    ('2015-10-01', 237.838),
    ('2015-11-01', 237.336),
    ('2015-12-01', 236.525),
    -- 2016
    ('2016-01-01', 236.916),
    ('2016-02-01', 237.111),
    ('2016-03-01', 238.132),
    ('2016-04-01', 239.261),
    ('2016-05-01', 240.229),
    ('2016-06-01', 241.018),
    ('2016-07-01', 240.628),
    ('2016-08-01', 240.849),
    ('2016-09-01', 241.428),
    ('2016-10-01', 241.729),
    ('2016-11-01', 241.353),
    ('2016-12-01', 241.432),
    -- 2017
    ('2017-01-01', 242.839),
    ('2017-02-01', 243.603),
    ('2017-03-01', 243.801),
    ('2017-04-01', 244.524),
    ('2017-05-01', 244.733),
    ('2017-06-01', 244.955),
    ('2017-07-01', 244.786),
    ('2017-08-01', 245.519),
    ('2017-09-01', 246.819),
    ('2017-10-01', 246.663),
    ('2017-11-01', 246.669),
    ('2017-12-01', 246.524),
    -- 2018
    ('2018-01-01', 247.867),
    ('2018-02-01', 248.991),
    ('2018-03-01', 249.554),
    ('2018-04-01', 250.546),
    ('2018-05-01', 251.588),
    ('2018-06-01', 251.989),
    ('2018-07-01', 252.006),
    ('2018-08-01', 252.146),
    ('2018-09-01', 252.439),
    ('2018-10-01', 252.885),
    ('2018-11-01', 252.038),
    ('2018-12-01', 251.233),
    -- 2019
    ('2019-01-01', 251.712),
    ('2019-02-01', 252.776),
    ('2019-03-01', 254.202),
    ('2019-04-01', 255.548),
    ('2019-05-01', 256.092),
    ('2019-06-01', 256.143),
    ('2019-07-01', 256.571),
    ('2019-08-01', 256.558),
    ('2019-09-01', 256.759),
    ('2019-10-01', 257.346),
    ('2019-11-01', 257.208),
    ('2019-12-01', 256.974),
    -- 2020
    ('2020-01-01', 257.971),
    ('2020-02-01', 258.678),
    ('2020-03-01', 258.115),
    ('2020-04-01', 256.389),
    ('2020-05-01', 256.394),
    ('2020-06-01', 257.797),
    ('2020-07-01', 259.101),
    ('2020-08-01', 259.918),
    ('2020-09-01', 260.280),
    ('2020-10-01', 260.388),
    ('2020-11-01', 260.229),
    ('2020-12-01', 260.474),
    -- 2021
    ('2021-01-01', 261.582),
    ('2021-02-01', 263.014),
    ('2021-03-01', 264.877),
    ('2021-04-01', 267.054),
    ('2021-05-01', 269.195),
    ('2021-06-01', 271.696),
    ('2021-07-01', 273.003),
    ('2021-08-01', 273.567),
    ('2021-09-01', 274.310),
    ('2021-10-01', 276.589),
    ('2021-11-01', 277.948),
    ('2021-12-01', 278.802),
    -- 2022
    ('2022-01-01', 281.148),
    ('2022-02-01', 283.716),
    ('2022-03-01', 287.504),
    ('2022-04-01', 289.109),
    ('2022-05-01', 292.296),
    ('2022-06-01', 296.311),
    ('2022-07-01', 296.276),
    ('2022-08-01', 296.171),
    ('2022-09-01', 296.808),
    ('2022-10-01', 298.012),
    ('2022-11-01', 297.711),
    ('2022-12-01', 296.797),
    -- 2023
    ('2023-01-01', 299.170),
    ('2023-02-01', 300.840),
    ('2023-03-01', 301.836),
    ('2023-04-01', 303.363),
    ('2023-05-01', 304.127),
    ('2023-06-01', 305.109),
    ('2023-07-01', 305.691),
    ('2023-08-01', 307.026),
    ('2023-09-01', 307.789),
    ('2023-10-01', 307.671),
    ('2023-11-01', 307.051),
    ('2023-12-01', 306.746),
    -- 2024
    ('2024-01-01', 308.417),
    ('2024-02-01', 310.326),
    ('2024-03-01', 312.332),
    ('2024-04-01', 313.548),
    ('2024-05-01', 314.069),
    ('2024-06-01', 314.175),
    ('2024-07-01', 314.540),
    ('2024-08-01', 314.796),
    ('2024-09-01', 315.301),
    ('2024-10-01', 315.664),
    ('2024-11-01', 315.493),
    ('2024-12-01', 315.605),
    -- 2025
    ('2025-01-01', 317.671),
    ('2025-02-01', 319.082),
    ('2025-03-01', 319.799),
    ('2025-04-01', 320.795),
    ('2025-05-01', 321.465),
    ('2025-06-01', 322.561),
    ('2025-07-01', 323.048),
    ('2025-08-01', 323.976),
    ('2025-09-01', 324.800),
    ('2025-11-01', 324.122),
    ('2025-12-01', 324.054),
    -- 2026
    ('2026-01-01', 325.252),
    ('2026-02-01', 326.785),
    ('2026-03-01', 330.213),
    ('2026-04-01', 333.020),
    ('2026-05-01', 335.123),
    ('2026-06-01', 333.952);

-- +goose Down
DROP TABLE IF EXISTS cpi_series;
