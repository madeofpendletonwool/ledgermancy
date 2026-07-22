-- name: UpsertAccount :one
-- Plaid is authoritative for account identity and balances, so re-syncing an
-- existing account refreshes it in place rather than creating a duplicate.
-- is_active is deliberately not touched: excluding an account from reports is
-- a user decision that must survive every sync.
INSERT INTO accounts (
    plaid_item_id, plaid_account_id, name, official_name, mask,
    type, subtype, current_balance, available_balance, credit_limit, currency
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (plaid_account_id) DO UPDATE SET
    name              = EXCLUDED.name,
    official_name     = EXCLUDED.official_name,
    mask              = EXCLUDED.mask,
    type              = EXCLUDED.type,
    subtype           = EXCLUDED.subtype,
    current_balance   = EXCLUDED.current_balance,
    available_balance = EXCLUDED.available_balance,
    credit_limit      = EXCLUDED.credit_limit,
    currency          = EXCLUDED.currency
RETURNING *;

-- name: GetAccountByPlaidID :one
SELECT * FROM accounts WHERE plaid_account_id = $1;

-- name: ListAccountsForItem :many
SELECT * FROM accounts WHERE plaid_item_id = $1 ORDER BY name;

-- name: ListVisibleAccounts :many
-- Accounts belonging to items the caller can see. Mirrors ListVisiblePlaidItems.
SELECT a.*, i.institution_name, i.user_id AS owner_id
FROM accounts a
JOIN plaid_items i ON i.id = a.plaid_item_id
JOIN users u       ON u.id = i.user_id
WHERE u.household_id = $1
  AND (i.user_id = $2 OR i.is_shared)
  AND a.is_active
ORDER BY i.institution_name, a.name;

-- name: SetAccountActive :one
UPDATE accounts SET is_active = $2 WHERE id = $1 RETURNING *;
