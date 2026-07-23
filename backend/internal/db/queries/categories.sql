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

-- name: UpdateCategory :one
-- Custom categories only: the household_id guard means a system default
-- (household_id NULL) never matches, so a shared default can't be edited.
UPDATE categories
SET name = $3, color = $4, is_fixed = $5, updated_at = now()
WHERE id = $1 AND household_id = $2
RETURNING *;

-- name: DeleteCategory :exec
-- Custom only (household_id guard). transactions.category_id is ON DELETE SET
-- NULL, so a deleted category's charges simply fall back to uncategorised.
DELETE FROM categories WHERE id = $1 AND household_id = $2;

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
-- Transactions still needing a category, scoped to one household. merchant_name
-- and name are selected because household rules (match step 2) match against
-- them — without them the deterministic pass could never fire a rule.
SELECT t.id, t.merchant_key, t.merchant_name, t.name,
       t.plaid_pfc_primary, t.plaid_pfc_detailed
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

-- name: ListLLMCandidates :many
-- Transactions the deterministic pass left in the fallback category ($2, the
-- household's "uncategorised" category) and that the LLM has not been asked
-- about yet. A merchant already in merchant_category_map is excluded: caching
-- every answer is what makes a merchant cost at most one model call, ever.
-- Rows without a merchant_key are skipped — there is nothing to cache them by.
SELECT t.id, t.merchant_key, t.merchant_name, t.name,
       t.plaid_pfc_primary, t.plaid_pfc_detailed
FROM transactions t
JOIN accounts a    ON a.id = t.account_id
JOIN plaid_items i ON i.id = a.plaid_item_id
JOIN users u       ON u.id = i.user_id
WHERE u.household_id = $1
  AND t.category_id = $2
  AND t.category_source IS DISTINCT FROM 'manual'
  AND t.merchant_key IS NOT NULL
  AND NOT EXISTS (
      SELECT 1 FROM merchant_category_map m
      WHERE m.household_id = $1 AND m.merchant_key = t.merchant_key
  )
ORDER BY t.merchant_key
LIMIT $3;

-- name: ApplyMerchantCategory :exec
-- Applies an LLM-resolved category to every one of a merchant's transactions in
-- the household, not just the sampled row — so transactions already sitting in
-- the fallback category are lifted out in one statement. Never touches a manual
-- choice.
UPDATE transactions t
SET category_id = $3, category_source = 'llm'
FROM accounts a, plaid_items i, users u
WHERE t.account_id = a.id
  AND a.plaid_item_id = i.id
  AND i.user_id = u.id
  AND u.household_id = $1
  AND t.merchant_key = $2
  AND t.category_source IS DISTINCT FROM 'manual';

-- name: ApplyMerchantCategoryRewritable :exec
-- Like ApplyMerchantCategory, but marks rows with the rewritable 'cache' source
-- instead of 'llm', so a later manual re-edit of the same merchant re-applies
-- cleanly across all its rows. Used by the manual "apply to all from this
-- merchant" path; never touches a manually-pinned row. The durable rule lives
-- in merchant_category_map (source 'manual'), which also catches future syncs.
UPDATE transactions t
SET category_id = $3, category_source = 'cache'
FROM accounts a, plaid_items i, users u
WHERE t.account_id = a.id
  AND a.plaid_item_id = i.id
  AND i.user_id = u.id
  AND u.household_id = $1
  AND t.merchant_key = $2
  AND t.category_source IS DISTINCT FROM 'manual';

-- name: ListHouseholdIDs :many
SELECT id FROM households ORDER BY created_at;
