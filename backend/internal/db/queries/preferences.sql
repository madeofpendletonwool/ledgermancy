-- name: UpsertUserPreference :one
-- Set one user-scoped preference; re-setting the same key updates it. The
-- partial index preferences_user_key is the conflict target.
INSERT INTO preferences (scope, user_id, key, value)
VALUES ('user', $1, $2, $3)
ON CONFLICT (user_id, key) WHERE scope = 'user'
DO UPDATE SET value = EXCLUDED.value, updated_at = now()
RETURNING *;

-- name: UpsertHouseholdPreference :one
-- Set one household-scoped preference; re-setting the same key updates it.
INSERT INTO preferences (scope, household_id, key, value)
VALUES ('household', $1, $2, $3)
ON CONFLICT (household_id, key) WHERE scope = 'household'
DO UPDATE SET value = EXCLUDED.value, updated_at = now()
RETURNING *;

-- name: ListUserPreferences :many
SELECT key, value FROM preferences
WHERE scope = 'user' AND user_id = $1
ORDER BY key;

-- name: ListHouseholdPreferences :many
SELECT key, value FROM preferences
WHERE scope = 'household' AND household_id = $1
ORDER BY key;

-- name: GetUserPreference :one
-- Single-key lookup used by the notifier (03) and digest (10) per user.
SELECT value FROM preferences
WHERE scope = 'user' AND user_id = $1 AND key = $2;
