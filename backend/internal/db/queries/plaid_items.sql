-- name: CreatePlaidItem :one
INSERT INTO plaid_items (
    user_id, plaid_item_id, access_token_encrypted,
    institution_id, institution_name, products
) VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetPlaidItem :one
SELECT * FROM plaid_items WHERE id = $1;

-- name: GetPlaidItemByPlaidID :one
SELECT * FROM plaid_items WHERE plaid_item_id = $1;

-- name: ListPlaidItemsForUser :many
SELECT * FROM plaid_items WHERE user_id = $1 ORDER BY created_at;

-- name: ListVisiblePlaidItems :many
-- Items the caller may see: their own, plus household members' shared items.
-- This is the single definition of item visibility; every report scopes
-- through it so a private item can never leak into a household total.
--
-- History spans are fetched separately (see itemHistorySpans in the api
-- package): sqlc infers min()/max() over a NOT NULL column as NOT NULL, but
-- both are NULL for an item with no transactions yet, which fails to scan.
SELECT i.*
FROM plaid_items i
JOIN users u ON u.id = i.user_id
WHERE u.household_id = $1
  AND (i.user_id = $2 OR i.is_shared)
ORDER BY i.created_at;

-- name: ListItemsDueForSync :many
-- Active items whose last sync is older than the supplied cutoff, plus any
-- that have never synced.
SELECT * FROM plaid_items
WHERE status = 'active'
  AND (last_synced_at IS NULL OR last_synced_at < $1)
ORDER BY last_synced_at NULLS FIRST;

-- name: UpdateItemCursor :exec
-- Advances the sync cursor. Written only after the page's rows are committed,
-- so a crash mid-sync replays that page rather than skipping it.
--
-- A successful sync also clears the whole error state, status included. Only
-- clearing error_code left a recovered item stuck displaying 'login_required'
-- forever, prompting the user to reconnect an account that already works.
UPDATE plaid_items
SET sync_cursor    = $2,
    last_synced_at = now(),
    status         = 'active',
    error_code     = NULL
WHERE id = $1;

-- name: MarkItemRefreshed :exec
-- Records that /transactions/refresh was requested, so the per-item rate limit
-- is respected. Written on request rather than on completion: Plaid's pull is
-- asynchronous, and it is the *request* that consumes the quota.
UPDATE plaid_items SET last_refresh_at = now() WHERE id = $1;

-- name: MarkBackfillComplete :exec
UPDATE plaid_items SET backfill_complete = TRUE WHERE id = $1;

-- name: SetItemStatus :exec
UPDATE plaid_items SET status = $2, error_code = $3 WHERE id = $1;

-- name: SetItemShared :one
UPDATE plaid_items SET is_shared = $3
WHERE id = $1 AND user_id = $2
RETURNING *;

-- name: DeletePlaidItem :exec
DELETE FROM plaid_items WHERE id = $1 AND user_id = $2;

-- name: GetHouseholdForItem :one
SELECT u.household_id
FROM plaid_items i
JOIN users u ON u.id = i.user_id
WHERE i.id = $1;
