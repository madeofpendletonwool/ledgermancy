-- Real-asset revaluation, depreciation and directly-held bonds (doc 26).
--
-- manual_assets.value stays authoritative for CURRENT value — every existing
-- net-worth query reads it, and making them join to find the latest valuation
-- would be a wide blast radius for no benefit. asset_valuations is the trend
-- behind it, and the two are written together in one transaction.

-- --------------------------------------------------------------------------
-- Class-specific detail
-- --------------------------------------------------------------------------

-- name: GetAssetDetail :one
-- Household-scoped through the asset, so a detail row is never reachable by id
-- alone.
SELECT d.* FROM asset_details d
JOIN manual_assets m ON m.id = d.manual_asset_id
WHERE d.manual_asset_id = sqlc.arg('manual_asset_id')
  AND m.household_id = sqlc.arg('household_id');

-- name: ListAssetDetails :many
SELECT d.* FROM asset_details d
JOIN manual_assets m ON m.id = d.manual_asset_id
WHERE m.household_id = $1;

-- name: UpsertAssetDetail :one
-- One detail row per asset, so this is an upsert rather than an insert. Every
-- column is nullable: a 'cash' asset legitimately carries none of them, and a
-- bond carries none of the vehicle ones.
INSERT INTO asset_details (
    manual_asset_id,
    address, beds, baths, sqft, lot_sqft,
    year, make, model, trim, mileage, annual_mileage,
    bond_series, issue_date, purchase_price, face_value,
    coupon_rate, maturity_date, tax_exempt,
    condition
) VALUES (
    sqlc.arg('manual_asset_id'),
    sqlc.narg('address'), sqlc.narg('beds'), sqlc.narg('baths'),
    sqlc.narg('sqft'), sqlc.narg('lot_sqft'),
    sqlc.narg('year'), sqlc.narg('make'), sqlc.narg('model'), sqlc.narg('trim'),
    sqlc.narg('mileage'), sqlc.narg('annual_mileage'),
    sqlc.narg('bond_series'), sqlc.narg('issue_date'),
    sqlc.narg('purchase_price'), sqlc.narg('face_value'),
    sqlc.narg('coupon_rate'), sqlc.narg('maturity_date'), sqlc.narg('tax_exempt'),
    sqlc.narg('condition')
)
ON CONFLICT (manual_asset_id) DO UPDATE SET
    address        = EXCLUDED.address,
    beds           = EXCLUDED.beds,
    baths          = EXCLUDED.baths,
    sqft           = EXCLUDED.sqft,
    lot_sqft       = EXCLUDED.lot_sqft,
    year           = EXCLUDED.year,
    make           = EXCLUDED.make,
    model          = EXCLUDED.model,
    trim           = EXCLUDED.trim,
    mileage        = EXCLUDED.mileage,
    annual_mileage = EXCLUDED.annual_mileage,
    bond_series    = EXCLUDED.bond_series,
    issue_date     = EXCLUDED.issue_date,
    purchase_price = EXCLUDED.purchase_price,
    face_value     = EXCLUDED.face_value,
    coupon_rate    = EXCLUDED.coupon_rate,
    maturity_date  = EXCLUDED.maturity_date,
    tax_exempt     = EXCLUDED.tax_exempt,
    condition      = EXCLUDED.condition,
    updated_at     = now()
RETURNING *;

-- --------------------------------------------------------------------------
-- Valuation history
-- --------------------------------------------------------------------------

-- name: ListAssetValuations :many
-- The trend behind a single asset. Ascending, because it is drawn as a line.
SELECT v.* FROM asset_valuations v
JOIN manual_assets m ON m.id = v.manual_asset_id
WHERE v.manual_asset_id = sqlc.arg('manual_asset_id')
  AND m.household_id = sqlc.arg('household_id')
ORDER BY v.as_of;

-- name: InsertAssetValuation :one
-- One valuation per asset per day. A second write on the same day replaces it
-- rather than failing: re-running the bond job must be idempotent, and a user
-- correcting a figure they entered an hour ago is not a conflict.
INSERT INTO asset_valuations (manual_asset_id, value, as_of, source, note)
VALUES ($1, $2, $3, $4, sqlc.narg('note'))
ON CONFLICT (manual_asset_id, as_of) DO UPDATE SET
    value  = EXCLUDED.value,
    source = EXCLUDED.source,
    note   = EXCLUDED.note
RETURNING *;

-- name: SetManualAssetValue :one
-- The other half of a revaluation. Callers MUST run this in the same
-- transaction as InsertAssetValuation: a failure has to leave neither, or the
-- headline value and the history it is supposed to explain disagree.
UPDATE manual_assets
SET value = $3, as_of = $4, updated_at = now()
WHERE id = $1 AND household_id = $2
RETURNING *;

-- name: GetManualAsset :one
SELECT * FROM manual_assets WHERE id = $1 AND household_id = $2;

-- name: SetManualAssetLoan :one
-- Link the loan secured against an asset. Equity = value − balance, and it is a
-- DISPLAY figure: net worth already counts the asset and the loan separately,
-- so equity must never be added to the sum.
UPDATE manual_assets
SET loan_account_id = sqlc.narg('loan_account_id'), updated_at = now()
WHERE id = $1 AND household_id = $2
RETURNING *;

-- name: ListManualAssetsWithLoans :many
-- Assets alongside the balance of any loan linked to them, for the equity
-- display. LEFT JOIN because most assets have no loan, and the visibility rule
-- matches every other account read: the household's own items, shared or the
-- caller's.
SELECT
    m.*,
    a.name           AS loan_name,
    a.current_balance AS loan_balance,
    d.year           AS detail_year,
    d.mileage        AS detail_mileage,
    d.annual_mileage AS detail_annual_mileage,
    d.bond_series    AS detail_bond_series
FROM manual_assets m
LEFT JOIN accounts a
       ON a.id = m.loan_account_id
      AND EXISTS (
          SELECT 1 FROM plaid_items i
          JOIN users u ON u.id = i.user_id
          WHERE i.id = a.plaid_item_id
            AND u.household_id = m.household_id
            AND (i.user_id = sqlc.arg('user_id') OR i.is_shared)
      )
LEFT JOIN asset_details d ON d.manual_asset_id = m.id
WHERE m.household_id = sqlc.arg('household_id')
ORDER BY m.is_liability, m.value DESC;

-- --------------------------------------------------------------------------
-- Published savings-bond rates
-- --------------------------------------------------------------------------

-- name: ListSavingsBondRates :many
-- The whole table. It is ~100 rows and every bond valuation needs all of the
-- periods it spans, so paging it would only add round trips.
SELECT * FROM savings_bond_rates ORDER BY series, period_start;

-- name: UpsertSavingsBondRate :one
-- Editable, deliberately. A household that distrusts the seeded table can
-- verify it against treasurydirect.gov and replace any row — which is the
-- answer to "a transcribed table of numbers nobody can verify".
INSERT INTO savings_bond_rates (series, period_start, fixed_rate, inflation_rate, source_url)
VALUES ($1, $2, $3, sqlc.narg('inflation_rate'), $4)
ON CONFLICT (series, period_start) DO UPDATE SET
    fixed_rate     = EXCLUDED.fixed_rate,
    inflation_rate = EXCLUDED.inflation_rate,
    source_url     = EXCLUDED.source_url
RETURNING *;

-- name: DeleteSavingsBondRate :exec
DELETE FROM savings_bond_rates WHERE series = $1 AND period_start = $2;

-- --------------------------------------------------------------------------
-- Jobs and nudges
-- --------------------------------------------------------------------------

-- name: ListBondAssets :many
-- Every asset that can be valued from published data, across all households —
-- the monthly bond revaluation job's work list. Scoped by household so the job
-- can run one household at a time.
SELECT m.id, m.household_id, m.name, m.value, m.as_of,
       d.bond_series, d.issue_date, d.purchase_price, d.face_value,
       d.coupon_rate, d.maturity_date
FROM manual_assets m
JOIN asset_details d ON d.manual_asset_id = m.id
WHERE m.household_id = $1
  AND d.bond_series IS NOT NULL
  AND d.issue_date IS NOT NULL
  AND d.purchase_price IS NOT NULL
ORDER BY m.name;

-- name: ListStaleAssets :many
-- Assets whose recorded value has gone stale, for the revaluation nudge.
--
-- Bonds are excluded: they revalue themselves automatically every month, so
-- nudging about one would be asking the user to confirm arithmetic the app has
-- already done. Liabilities are excluded too — a manually tracked debt does not
-- drift the way a house does.
SELECT m.id, m.name, m.kind, m.value, m.as_of
FROM manual_assets m
LEFT JOIN asset_details d ON d.manual_asset_id = m.id
WHERE m.household_id = $1
  AND NOT m.is_liability
  AND m.kind <> 'bond'
  AND d.bond_series IS NULL
  AND m.as_of <= sqlc.arg('stale_before')::date
ORDER BY m.as_of;
