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

-- name: SetAccountBeneficiary :one
-- Whose money this account is: a 529's beneficiary, the minor on a UTMA, the
-- child on a custodial Roth. The 529 sense of "beneficiary", NOT payable-on-
-- death, and NOT joint ownership.
--
-- Both the account and the person are re-resolved through the household guard,
-- so neither id is trusted from the request. A NULL person clears it.
UPDATE accounts a
SET beneficiary_person_id = sqlc.narg('person_id')
FROM plaid_items i, users u
WHERE a.id = sqlc.arg('id')
  AND i.id = a.plaid_item_id
  AND u.id = i.user_id
  AND u.household_id = sqlc.arg('household_id')
  AND (
        sqlc.narg('person_id')::uuid IS NULL
     OR EXISTS (
            SELECT 1 FROM household_people p
            WHERE p.id = sqlc.narg('person_id')
              AND p.household_id = sqlc.arg('household_id')
        )
  )
RETURNING a.*;

-- name: ListAccountsForPerson :many
-- Accounts held FOR one person. Read-only surface for a child, and the
-- per-person net-worth lens for a parent.
SELECT a.*, i.institution_name
FROM accounts a
JOIN plaid_items i ON i.id = a.plaid_item_id
JOIN users u       ON u.id = i.user_id
WHERE u.household_id = $1
  AND a.beneficiary_person_id = $2
  AND a.is_active
ORDER BY i.institution_name, a.name;

-- name: ListPersonAssetTotals :many
-- Per-person asset breakdown for the net-worth lens. This is a BREAKDOWN of a
-- total that already exists, not a new sum: a child's 529 was already in the
-- household's assets and stays there. Nothing here changes ComputeNetWorth.
SELECT
    p.id   AS person_id,
    p.display_name,
    p.is_dependent,
    COALESCE(SUM(a.current_balance), 0)::numeric AS account_total,
    COALESCE(SUM(a.current_balance) FILTER (
        WHERE a.tax_treatment IN ('529','utma_ugma','coverdell','custodial_roth','trump')
    ), 0)::numeric AS custodial_total
FROM household_people p
LEFT JOIN accounts a
       ON a.beneficiary_person_id = p.id AND a.is_active
WHERE p.household_id = $1
GROUP BY p.id, p.display_name, p.is_dependent
ORDER BY p.is_dependent, p.display_name;
