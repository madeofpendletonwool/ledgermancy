-- name: ListCategories :many
-- System categories plus this household's own.
SELECT * FROM categories
WHERE household_id IS NULL OR household_id = $1
ORDER BY sort_order, name;

-- name: GetCategoryBySlug :one
SELECT * FROM categories
WHERE slug = $1 AND (household_id IS NULL OR household_id = $2)
ORDER BY household_id NULLS LAST
LIMIT 1;

-- name: CreateCategory :one
INSERT INTO categories (household_id, name, slug, parent_id, icon, color, is_fixed, sort_order)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: ResolvePFCCategory :one
-- Maps a Plaid category onto ours, preferring a detailed-level match so
-- specific overrides (credit-card payments) beat the broad primary mapping.
SELECT c.*
FROM pfc_category_map m
JOIN categories c ON c.slug = m.category_slug AND c.household_id IS NULL
WHERE m.pfc_primary = $1
  AND (m.pfc_detailed = sqlc.narg('pfc_detailed') OR m.pfc_detailed IS NULL)
ORDER BY m.pfc_detailed NULLS LAST
LIMIT 1;

-- name: ListCategoryRules :many
SELECT * FROM category_rules
WHERE household_id = $1 AND enabled
ORDER BY priority DESC, created_at;

-- name: CreateCategoryRule :one
INSERT INTO category_rules (household_id, match_type, pattern, category_id, priority)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: DeleteCategoryRule :exec
DELETE FROM category_rules WHERE id = $1 AND household_id = $2;

-- name: LookupMerchantCategory :one
SELECT category_id FROM merchant_category_map
WHERE household_id = $1 AND merchant_key = $2;

-- name: UpsertMerchantCategory :exec
-- Caches a resolved merchant so the same decision is never recomputed. A
-- manual choice outranks anything previously learned.
INSERT INTO merchant_category_map (household_id, merchant_key, category_id, source)
VALUES ($1, $2, $3, $4)
ON CONFLICT (household_id, merchant_key) DO UPDATE SET
    category_id = EXCLUDED.category_id,
    source      = EXCLUDED.source
WHERE merchant_category_map.source <> 'manual' OR EXCLUDED.source = 'manual';

-- name: SetTransactionCategory :one
-- Manual recategorisation. category_source = 'manual' makes the choice sticky:
-- the sync upsert preserves it, so Plaid can never overwrite it.
UPDATE transactions t
SET category_id = $3, category_source = 'manual'
FROM accounts a, plaid_items i, users u
WHERE t.id = $1
  AND a.id = t.account_id
  AND i.id = a.plaid_item_id
  AND u.id = i.user_id
  AND u.household_id = $2
RETURNING t.*;

-- name: ListUncategorisedTransactions :many
-- Transactions still needing a category, scoped to one household.
SELECT t.id, t.merchant_key, t.plaid_pfc_primary, t.plaid_pfc_detailed
FROM transactions t
JOIN accounts a    ON a.id = t.account_id
JOIN plaid_items i ON i.id = a.plaid_item_id
JOIN users u       ON u.id = i.user_id
WHERE u.household_id = $1
  AND t.category_id IS NULL
LIMIT $2;

-- name: ApplyCategory :exec
-- Applies a resolved category without disturbing a manual choice.
UPDATE transactions
SET category_id = $2, category_source = $3
WHERE id = $1 AND category_source IS DISTINCT FROM 'manual';

-- name: ListHouseholdIDs :many
SELECT id FROM households ORDER BY created_at;
