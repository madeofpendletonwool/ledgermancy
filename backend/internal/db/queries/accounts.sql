-- name: UpsertAccount :one
-- Plaid is authoritative for account identity and balances, so re-syncing an
-- existing account refreshes it in place rather than creating a duplicate.
-- is_active is deliberately not touched: excluding an account from reports is
-- a user decision that must survive every sync.
--
-- The WHERE on the conflict target is load-bearing: 00053 made this index
-- PARTIAL (manual rows have no Plaid id and are not in it), and Postgres only
-- accepts a partial index as an ON CONFLICT arbiter when the statement repeats
-- its predicate.
INSERT INTO accounts (
    plaid_item_id, plaid_account_id, name, official_name, mask,
    type, subtype, current_balance, available_balance, credit_limit, currency
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (plaid_account_id) WHERE plaid_account_id IS NOT NULL DO UPDATE SET
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
-- Every account the caller can see, of either source. institution_name is NULL
-- for a manual account — there is no institution — and the UI labels those from
-- accounts.source rather than inventing one here.
--
-- Columns are listed rather than `a.*` on purpose. accounts now carries its own
-- user_id and is_shared, which are NULL on a Plaid row and meaningful only on a
-- manual one; `a.*` alongside the view's resolved columns yields two fields
-- with the same name, and the handler that picks the raw one silently reports a
-- private institution as shared. owner_id/shared below are the RESOLVED values
-- and the only ones a caller should read.
SELECT
    a.id,
    a.plaid_item_id,
    a.name,
    a.official_name,
    a.mask,
    a.type,
    a.subtype,
    a.current_balance,
    a.available_balance,
    a.credit_limit,
    a.currency,
    a.tax_treatment,
    -- User-entered deposit yield as a PERCENT; NULL means nobody has said, and
    -- doc 32's cash-drag detector stays silent on it rather than reading it as
    -- zero. Carried here so the Accounts page can show and edit it.
    a.deposit_apy,
    a.is_managed,
    a.beneficiary_person_id,
    a.source,
    v.institution_name,
    v.user_id  AS owner_id,
    v.is_shared AS shared
FROM accounts a
JOIN account_access v ON v.account_id = a.id
WHERE v.household_id = $1
  AND (v.user_id = $2 OR v.is_shared)
  AND a.is_active
ORDER BY v.institution_name NULLS LAST, a.name;

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
FROM account_access v
WHERE a.id = sqlc.arg('id')
  AND v.account_id = a.id
  AND v.household_id = sqlc.arg('household_id')
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
SELECT a.*, v.institution_name
FROM accounts a
JOIN account_access v ON v.account_id = a.id
WHERE v.household_id = $1
  AND a.beneficiary_person_id = $2
  AND a.is_active
ORDER BY v.institution_name, a.name;

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
