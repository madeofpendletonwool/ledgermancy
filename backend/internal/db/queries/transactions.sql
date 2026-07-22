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
SELECT t.*, a.name AS account_name, i.institution_name
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
ORDER BY t.date DESC, t.created_at DESC
LIMIT $5 OFFSET $6;
