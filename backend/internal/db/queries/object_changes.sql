-- Per-object change history. One row per changed field, written in the same
-- transaction as the mutation it records. See migration 00062_object_changes.sql
-- for the append-only contract and the visibility model.

-- name: InsertObjectChange :exec
-- One field-level diff row. Called inside the caller's transaction (handlers
-- pass a tx-backed *Queries), so a rolled-back mutation writes nothing — the
-- load-bearing "same transaction" invariant. old/new are JSONB: NULL old means
-- a field was set on create, NULL new means it was cleared.
INSERT INTO object_changes (
    household_id, object_kind, object_id, actor_user_id, field, old_value, new_value
) VALUES (
    $1, $2, $3, $4, $5, $6, $7
);

-- --------------------------------------------------------------------------
-- Read paths. The History panel lists one object's changes newest-first, and
-- each query re-resolves visibility by joining back to the source table — so a
-- private account/transaction's history is invisible to the other member of the
-- household, exactly as the object itself is. Mirrors the scoping each detail
-- view already applies.
-- --------------------------------------------------------------------------

-- name: ListTransactionChanges :many
-- Transaction visibility is inherited from the account it posts to, resolved
-- through account_access: own items ∪ household items where is_shared.
SELECT oc.field, oc.old_value, oc.new_value, oc.actor_user_id,
       u.display_name AS actor_display_name, oc.created_at
FROM object_changes oc
JOIN transactions t ON t.id = oc.object_id
JOIN accounts a     ON a.id = t.account_id
JOIN account_access v ON v.account_id = a.id
LEFT JOIN users u   ON u.id = oc.actor_user_id
WHERE oc.household_id = sqlc.arg('household_id')
  AND oc.object_kind = 'transaction'
  AND oc.object_id = sqlc.arg('object_id')
  AND (v.user_id = sqlc.narg('viewer_user_id')::uuid OR v.is_shared)
ORDER BY oc.created_at DESC
LIMIT sqlc.arg('limit_count');

-- name: ListBudgetChanges :many
-- A household budget is visible to everyone; a personal budget to its owner.
SELECT oc.field, oc.old_value, oc.new_value, oc.actor_user_id,
       u.display_name AS actor_display_name, oc.created_at
FROM object_changes oc
JOIN budgets b   ON b.id = oc.object_id
LEFT JOIN users u ON u.id = oc.actor_user_id
WHERE oc.household_id = sqlc.arg('household_id')
  AND oc.object_kind = 'budget'
  AND oc.object_id = sqlc.arg('object_id')
  AND (b.owner_scope = 'household' OR b.user_id = sqlc.narg('viewer_user_id')::uuid)
ORDER BY oc.created_at DESC
LIMIT sqlc.arg('limit_count');

-- name: ListGoalChanges :many
-- Goal visibility is the three-way scope: household (everyone), user (owner),
-- person (the person it belongs to, plus any adult). all_person_goals carries
-- the adult/child decision into the SQL the way ListGoals does.
SELECT oc.field, oc.old_value, oc.new_value, oc.actor_user_id,
       u.display_name AS actor_display_name, oc.created_at
FROM object_changes oc
JOIN goals g     ON g.id = oc.object_id
LEFT JOIN users u ON u.id = oc.actor_user_id
WHERE oc.household_id = sqlc.arg('household_id')
  AND oc.object_kind = 'goal'
  AND oc.object_id = sqlc.arg('object_id')
  AND (
        g.scope = 'household'
     OR (g.scope = 'user'   AND g.user_id = sqlc.narg('viewer_user_id')::uuid)
     OR (g.scope = 'person' AND (sqlc.arg('all_person_goals')::boolean
                                 OR g.person_id = sqlc.narg('person_id')::uuid))
  )
ORDER BY oc.created_at DESC
LIMIT sqlc.arg('limit_count');
