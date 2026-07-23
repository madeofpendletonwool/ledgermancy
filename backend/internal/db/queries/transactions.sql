-- name: UpsertTransaction :one
-- The idempotency guarantee for syncing.
--
-- Plaid's /transactions/sync can legitimately redeliver a transaction (a
-- replayed page after a crash, or a genuine `modified` event), so this is
-- keyed on plaid_transaction_id and updates in place. Re-running an entire
-- sync therefore converges rather than duplicating.
--
-- Two fields are deliberately preserved across updates:
--   * a manual category, because Plaid must never overwrite a human decision
--   * excluded_from_reports and notes, which are purely user state
INSERT INTO transactions (
    account_id, plaid_transaction_id, amount, currency, date, authorized_date,
    name, merchant_name, merchant_key, pending, pending_transaction_id,
    plaid_pfc_primary, plaid_pfc_detailed, source, raw
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, 'plaid', $14)
ON CONFLICT (plaid_transaction_id) DO UPDATE SET
    account_id             = EXCLUDED.account_id,
    amount                 = EXCLUDED.amount,
    currency               = EXCLUDED.currency,
    date                   = EXCLUDED.date,
    authorized_date        = EXCLUDED.authorized_date,
    name                   = EXCLUDED.name,
    merchant_name          = EXCLUDED.merchant_name,
    merchant_key           = EXCLUDED.merchant_key,
    pending                = EXCLUDED.pending,
    pending_transaction_id = EXCLUDED.pending_transaction_id,
    plaid_pfc_primary      = EXCLUDED.plaid_pfc_primary,
    plaid_pfc_detailed     = EXCLUDED.plaid_pfc_detailed,
    raw                    = EXCLUDED.raw,
    category_id     = CASE WHEN transactions.category_source = 'manual'
                           THEN transactions.category_id ELSE NULL END,
    category_source = CASE WHEN transactions.category_source = 'manual'
                           THEN transactions.category_source ELSE NULL END
RETURNING *;

-- name: DeleteTransactionByPlaidID :execrows
DELETE FROM transactions WHERE plaid_transaction_id = $1;

-- name: DeletePendingSupersededBy :execrows
-- When a pending charge posts, Plaid issues a new transaction that names the
-- pending one it replaces. Removing the superseded row keeps a single
-- authoritative record and stops the amount being counted twice.
DELETE FROM transactions WHERE plaid_transaction_id = $1;

-- name: CountTransactionsForItem :one
SELECT count(*)
FROM transactions t
JOIN accounts a ON a.id = t.account_id
WHERE a.plaid_item_id = $1;

-- name: ListVisibleTransactions :many
SELECT t.*, a.name AS account_name, i.institution_name,
    -- A manual row is a "possible duplicate" when a Plaid-synced row exists on
    -- the same account for the same amount within four days — the issuer having
    -- finally delivered a charge the user already entered by hand. Computed at
    -- read time (never in the sync hot path) and only ever true for manual rows.
    (t.source = 'manual' AND EXISTS (
        SELECT 1 FROM transactions p
        WHERE p.source = 'plaid'
          AND p.account_id = t.account_id
          AND p.amount = t.amount
          AND abs(p.date - t.date) <= 4
    )) AS is_possible_duplicate
FROM transactions t
JOIN accounts a    ON a.id = t.account_id
JOIN plaid_items i ON i.id = a.plaid_item_id
JOIN users u       ON u.id = i.user_id
WHERE u.household_id = $1
  AND (i.user_id = $2 OR i.is_shared)
  AND a.is_active
  AND NOT t.excluded_from_reports
  AND t.date >= $3
  AND t.date <= $4
  AND (sqlc.narg('account_id')::uuid IS NULL OR t.account_id = sqlc.narg('account_id')::uuid)
  -- Optional "needs a category" filter for draining the backlog: a row is
  -- uncategorised when it has no category or sits in the fallback 'uncategorised'
  -- category. NULL/false narg passes everything.
  AND (
    sqlc.narg('uncategorised')::bool IS NOT TRUE
    OR t.category_id IS NULL
    OR t.category_id IN (SELECT id FROM categories WHERE slug = 'uncategorised')
  )
ORDER BY t.date DESC, t.created_at DESC
LIMIT $5 OFFSET $6;

-- name: SumFilteredTransactions :one
-- Exact count and total of spending transactions in a period, optionally
-- narrowed to a category and/or merchant. Backs the assistant's breakdown tool
-- so the model quotes SQL-computed figures rather than summing rows itself.
--
-- Filters mirror GetSpendingByCategory exactly (money out, non-income,
-- non-transfer, categorised) so a category count here reconciles with that
-- report. Category matches on exact slug or a name substring; merchant matches
-- on a name substring — both case-insensitive, both optional.
SELECT
    COUNT(*)::bigint                    AS transaction_count,
    COALESCE(SUM(t.amount), 0)::numeric AS total
FROM transactions t
JOIN accounts a    ON a.id = t.account_id
JOIN plaid_items i ON i.id = a.plaid_item_id
JOIN users u       ON u.id = i.user_id
JOIN categories c  ON c.id = t.category_id
WHERE u.household_id = $1
  AND (i.user_id = $2 OR i.is_shared)
  AND a.is_active
  AND NOT t.excluded_from_reports
  AND NOT t.pending
  AND t.date >= $3 AND t.date <= $4
  AND NOT c.is_income
  AND NOT c.is_transfer
  AND t.amount > 0
  AND (sqlc.narg('category')::text IS NULL
       OR c.slug = lower(sqlc.narg('category')::text)
       OR c.name ILIKE '%' || sqlc.narg('category')::text || '%')
  AND (sqlc.narg('merchant')::text IS NULL
       OR COALESCE(t.merchant_name, t.name) ILIKE '%' || sqlc.narg('merchant')::text || '%');

-- name: ListFilteredTransactions :many
-- The per-transaction breakdown behind the same tool. Same filters as
-- SumFilteredTransactions; the list may be capped by the limit while the sum
-- above stays exact over every match.
SELECT
    t.date,
    COALESCE(t.merchant_name, t.name) AS merchant,
    t.amount,
    c.name AS category_name
FROM transactions t
JOIN accounts a    ON a.id = t.account_id
JOIN plaid_items i ON i.id = a.plaid_item_id
JOIN users u       ON u.id = i.user_id
JOIN categories c  ON c.id = t.category_id
WHERE u.household_id = $1
  AND (i.user_id = $2 OR i.is_shared)
  AND a.is_active
  AND NOT t.excluded_from_reports
  AND NOT t.pending
  AND t.date >= $3 AND t.date <= $4
  AND NOT c.is_income
  AND NOT c.is_transfer
  AND t.amount > 0
  AND (sqlc.narg('category')::text IS NULL
       OR c.slug = lower(sqlc.narg('category')::text)
       OR c.name ILIKE '%' || sqlc.narg('category')::text || '%')
  AND (sqlc.narg('merchant')::text IS NULL
       OR COALESCE(t.merchant_name, t.name) ILIKE '%' || sqlc.narg('merchant')::text || '%')
ORDER BY t.date DESC, t.amount DESC
LIMIT sqlc.arg('lim');

-- name: CreateManualTransaction :one
-- Inserts a hand-entered transaction. Household scoping is enforced in the
-- SELECT: the row is created only when the target account belongs to the
-- caller's household and is visible to them (own or shared), so a foreign or
-- invisible account_id inserts nothing and the handler reads pgx.ErrNoRows.
--
-- source='manual' and a NULL plaid_transaction_id keep it clear of the Plaid
-- sync upsert (which keys ON CONFLICT (plaid_transaction_id)) forever, and the
-- balance is never touched — manual rows correct spending math only.
INSERT INTO transactions (
    account_id, amount, currency, date, name, merchant_name, merchant_key,
    category_id, category_source, notes, source, pending
)
SELECT
    a.id,
    sqlc.arg('amount'),
    a.currency,
    sqlc.arg('date'),
    sqlc.arg('name'),
    sqlc.narg('merchant_name'),
    sqlc.narg('merchant_key'),
    sqlc.narg('category_id'),
    CASE WHEN sqlc.narg('category_id')::uuid IS NULL THEN NULL ELSE 'manual' END,
    sqlc.narg('notes'),
    'manual',
    false
FROM accounts a
JOIN plaid_items i ON i.id = a.plaid_item_id
JOIN users u       ON u.id = i.user_id
WHERE a.id = sqlc.arg('account_id')
  AND u.household_id = sqlc.arg('household_id')
  AND (i.user_id = sqlc.arg('user_id') OR i.is_shared)
  AND a.is_active
RETURNING *;

-- name: UpdateManualTransaction :one
-- Edits a manual row in place. The source='manual' guard means this can never
-- mutate a Plaid-synced transaction even with a valid id, and the household
-- join means it can never touch another household's row.
UPDATE transactions t
SET amount          = sqlc.arg('amount'),
    date            = sqlc.arg('date'),
    name            = sqlc.arg('name'),
    merchant_name   = sqlc.narg('merchant_name'),
    merchant_key    = sqlc.narg('merchant_key'),
    category_id     = sqlc.narg('category_id'),
    category_source = CASE WHEN sqlc.narg('category_id')::uuid IS NULL THEN NULL ELSE 'manual' END,
    notes           = sqlc.narg('notes'),
    updated_at      = now()
FROM accounts a, plaid_items i, users u
WHERE t.id = sqlc.arg('id')
  AND t.source = 'manual'
  AND a.id = t.account_id
  AND i.id = a.plaid_item_id
  AND u.id = i.user_id
  AND u.household_id = sqlc.arg('household_id')
RETURNING t.*;

-- name: DeleteManualTransaction :execrows
-- Same source='manual' + household guard as the update. :execrows returns 0
-- when nothing matched (wrong household, or a Plaid id), which the handler maps
-- to 404.
DELETE FROM transactions t
USING accounts a, plaid_items i, users u
WHERE t.id = sqlc.arg('id')
  AND t.source = 'manual'
  AND a.id = t.account_id
  AND i.id = a.plaid_item_id
  AND u.id = i.user_id
  AND u.household_id = sqlc.arg('household_id');
