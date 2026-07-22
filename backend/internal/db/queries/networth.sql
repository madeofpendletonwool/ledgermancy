-- name: UpsertSecurity :one
INSERT INTO securities (
    plaid_security_id, name, ticker, type, cusip, isin,
    close_price, close_price_as_of, currency, is_cash_equivalent
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (plaid_security_id) DO UPDATE SET
    name              = EXCLUDED.name,
    ticker            = EXCLUDED.ticker,
    type              = EXCLUDED.type,
    close_price       = EXCLUDED.close_price,
    close_price_as_of = EXCLUDED.close_price_as_of,
    currency          = EXCLUDED.currency,
    is_cash_equivalent = EXCLUDED.is_cash_equivalent
RETURNING *;

-- name: UpsertHolding :exec
INSERT INTO holdings (
    account_id, security_id, quantity, cost_basis,
    institution_price, institution_value, currency, as_of
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (account_id, security_id) DO UPDATE SET
    quantity          = EXCLUDED.quantity,
    cost_basis        = EXCLUDED.cost_basis,
    institution_price = EXCLUDED.institution_price,
    institution_value = EXCLUDED.institution_value,
    currency          = EXCLUDED.currency,
    as_of             = EXCLUDED.as_of;

-- name: DeleteHoldingsNotIn :exec
-- Removes positions the institution no longer reports, so a fully sold holding
-- disappears instead of lingering at its last known value.
DELETE FROM holdings
WHERE account_id = $1 AND NOT (security_id = ANY($2::uuid[]));

-- name: ListVisibleHoldings :many
SELECT
    h.id,
    h.quantity,
    h.cost_basis,
    h.institution_value,
    h.currency,
    s.name    AS security_name,
    s.ticker,
    s.type    AS security_type,
    s.is_cash_equivalent,
    a.name    AS account_name,
    i.institution_name
FROM holdings h
JOIN securities s  ON s.id = h.security_id
JOIN accounts a    ON a.id = h.account_id
JOIN plaid_items i ON i.id = a.plaid_item_id
JOIN users u       ON u.id = i.user_id
WHERE u.household_id = $1
  AND (i.user_id = $2 OR i.is_shared)
  AND a.is_active
ORDER BY h.institution_value DESC NULLS LAST;

-- name: UpsertLiability :exec
INSERT INTO liabilities (
    account_id, kind, apr, apr_type, balance, minimum_payment,
    last_payment_amount, last_payment_date, next_payment_due_date,
    origination_date, origination_principal, interest_rate_percentage,
    is_overdue, raw
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
ON CONFLICT (account_id) DO UPDATE SET
    kind                     = EXCLUDED.kind,
    apr                      = EXCLUDED.apr,
    apr_type                 = EXCLUDED.apr_type,
    balance                  = EXCLUDED.balance,
    minimum_payment          = EXCLUDED.minimum_payment,
    last_payment_amount      = EXCLUDED.last_payment_amount,
    last_payment_date        = EXCLUDED.last_payment_date,
    next_payment_due_date    = EXCLUDED.next_payment_due_date,
    origination_date         = EXCLUDED.origination_date,
    origination_principal    = EXCLUDED.origination_principal,
    interest_rate_percentage = EXCLUDED.interest_rate_percentage,
    is_overdue               = EXCLUDED.is_overdue,
    raw                      = EXCLUDED.raw;

-- name: ListVisibleLiabilities :many
SELECT
    l.*,
    a.name AS account_name,
    a.mask,
    i.institution_name
FROM liabilities l
JOIN accounts a    ON a.id = l.account_id
JOIN plaid_items i ON i.id = a.plaid_item_id
JOIN users u       ON u.id = i.user_id
WHERE u.household_id = $1
  AND (i.user_id = $2 OR i.is_shared)
  AND a.is_active
ORDER BY l.balance DESC NULLS LAST;

-- name: ListManualAssets :many
SELECT * FROM manual_assets WHERE household_id = $1 ORDER BY is_liability, value DESC;

-- name: CreateManualAsset :one
INSERT INTO manual_assets (household_id, created_by, name, kind, value, is_liability, as_of, notes)
VALUES ($1, $2, $3, $4, $5, $6, COALESCE(sqlc.narg('as_of')::date, CURRENT_DATE), $7)
RETURNING *;

-- name: UpdateManualAsset :one
UPDATE manual_assets
SET name = $3, kind = $4, value = $5, is_liability = $6, notes = $7, as_of = CURRENT_DATE
WHERE id = $1 AND household_id = $2
RETURNING *;

-- name: DeleteManualAsset :exec
DELETE FROM manual_assets WHERE id = $1 AND household_id = $2;

-- name: ComputeNetWorth :one
-- The current whole picture, in one pass.
--
-- Account *type* decides which side of the ledger a balance falls on: credit
-- and loan balances are amounts owed, everything else is held. Investment
-- value comes from the account balance rather than summing holdings, because
-- the balance includes uninvested cash that holdings alone would miss.
WITH visible_accounts AS (
    SELECT a.type, a.current_balance
    FROM accounts a
    JOIN plaid_items i ON i.id = a.plaid_item_id
    JOIN users u       ON u.id = i.user_id
    WHERE u.household_id = $1
      AND a.is_active
      AND a.current_balance IS NOT NULL
),
manual AS (
    SELECT is_liability, value FROM manual_assets WHERE household_id = $1
)
SELECT
    COALESCE((SELECT SUM(current_balance) FROM visible_accounts
              WHERE type = 'depository'), 0)::numeric AS cash,
    COALESCE((SELECT SUM(current_balance) FROM visible_accounts
              WHERE type IN ('investment', 'brokerage')), 0)::numeric AS investments,
    COALESCE((SELECT SUM(current_balance) FROM visible_accounts
              WHERE type NOT IN ('depository', 'investment', 'brokerage', 'credit', 'loan')), 0)::numeric AS other_assets,
    COALESCE((SELECT SUM(value) FROM manual WHERE NOT is_liability), 0)::numeric AS manual_assets,
    COALESCE((SELECT SUM(current_balance) FROM visible_accounts
              WHERE type = 'credit'), 0)::numeric AS credit_debt,
    COALESCE((SELECT SUM(current_balance) FROM visible_accounts
              WHERE type = 'loan'), 0)::numeric AS loan_debt,
    COALESCE((SELECT SUM(value) FROM manual WHERE is_liability), 0)::numeric AS manual_debt;

-- name: UpsertNetWorthSnapshot :one
INSERT INTO net_worth_snapshots (
    household_id, as_of, assets_total, liabilities_total, net_worth, breakdown
) VALUES ($1, COALESCE(sqlc.narg('as_of')::date, CURRENT_DATE), $2, $3, $4, $5)
ON CONFLICT (household_id, as_of) DO UPDATE SET
    assets_total      = EXCLUDED.assets_total,
    liabilities_total = EXCLUDED.liabilities_total,
    net_worth         = EXCLUDED.net_worth,
    breakdown         = EXCLUDED.breakdown
RETURNING *;

-- name: ListNetWorthSnapshots :many
SELECT * FROM net_worth_snapshots
WHERE household_id = $1 AND as_of >= $2 AND as_of <= $3
ORDER BY as_of;

-- name: GetLatestNetWorthSnapshot :one
SELECT * FROM net_worth_snapshots
WHERE household_id = $1
ORDER BY as_of DESC
LIMIT 1;
