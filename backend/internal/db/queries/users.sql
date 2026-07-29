-- name: CreateUser :one
-- `role` is passed explicitly rather than left to the column default: the
-- bootstrap user owns the household, and an invited user gets whatever role the
-- invite granted. Defaulting here would quietly make every invited child a
-- full member.
INSERT INTO users (household_id, email, password_hash, display_name, role)
VALUES ($1, lower($2), $3, $4, $5)
RETURNING *;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = $1;

-- name: GetUserByEmail :one
-- Matches the lower(email) unique index, so lookups are case-insensitive.
SELECT * FROM users WHERE lower(email) = lower($1);

-- name: CountUsers :one
-- Used to decide whether open registration is still allowed: the very first
-- user bootstraps a household, everyone after that needs an invite.
SELECT count(*) FROM users;

-- name: UpdateUserPassword :exec
UPDATE users SET password_hash = $2 WHERE id = $1;

-- name: RecordFailedLogin :one
-- Bumps the durable failure counter and returns it so the caller can decide
-- how long to lock for. Counting in the database rather than in process memory
-- means a restart does not hand an attacker a fresh budget.
UPDATE users
SET failed_login_count = failed_login_count + 1
WHERE id = $1
RETURNING failed_login_count;

-- name: LockUser :exec
UPDATE users SET locked_until = $2 WHERE id = $1;

-- name: ClearFailedLogins :exec
-- Called after any successful authentication.
UPDATE users SET failed_login_count = 0, locked_until = NULL WHERE id = $1;

-- name: UpdateUserProfile :one
UPDATE users SET display_name = $2 WHERE id = $1 RETURNING *;
