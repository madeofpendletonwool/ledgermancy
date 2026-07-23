-- Goals: savings/target goals and the derived-progress lookups behind them.
-- Every read is scoped so a caller sees their household's shared goals plus
-- their own personal ones, never another member's private goal.

-- name: CreateGoal :one
INSERT INTO goals (
    household_id, scope, user_id, kind, name, target_amount, target_date, account_id, category_id
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: ListGoals :many
-- Active goals visible to the caller: household-shared plus their own.
SELECT * FROM goals
WHERE household_id = $1
  AND archived_at IS NULL
  AND (scope = 'household' OR user_id = $2)
ORDER BY created_at DESC;

-- name: GetGoal :one
SELECT * FROM goals
WHERE id = $1 AND household_id = $2
  AND (scope = 'household' OR user_id = $3);

-- name: UpdateGoal :one
UPDATE goals
SET name = $4, target_amount = $5, target_date = $6, account_id = $7, category_id = $8
WHERE id = $1 AND household_id = $2
  AND (scope = 'household' OR user_id = $3)
RETURNING *;

-- name: ArchiveGoal :exec
UPDATE goals
SET archived_at = now()
WHERE id = $1 AND household_id = $2
  AND (scope = 'household' OR user_id = $3);

-- name: MarkGoalAchieved :exec
-- Stamps the first time progress reaches the target; the guard keeps the
-- original achievement time even if the producer re-runs.
UPDATE goals
SET achieved_at = now()
WHERE id = $1 AND household_id = $2 AND achieved_at IS NULL;

-- name: ListActiveHouseholdGoals :many
-- Household-scoped active goals only. The insight feed is household-shared and
-- has no per-user visibility, so the coaching producer coaches shared goals
-- exclusively — a personal goal never leaks into a feed the whole household
-- reads. Personal goals still work everywhere else (CRUD + feasibility on the
-- Goals page).
SELECT * FROM goals
WHERE household_id = $1 AND scope = 'household' AND archived_at IS NULL;

-- name: GetGoalAccountBalance :one
-- Current balance of a goal's linked account, scoped to the household so a goal
-- can never read another household's account. Coalesced to 0 when unknown.
SELECT COALESCE(a.current_balance, 0)::numeric AS balance
FROM accounts a
JOIN plaid_items i ON i.id = a.plaid_item_id
JOIN users u       ON u.id = i.user_id
WHERE a.id = $1 AND u.household_id = $2;
