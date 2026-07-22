-- name: CreateUser :one
INSERT INTO users (household_id, email, password_hash, display_name)
VALUES ($1, lower($2), $3, $4)
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

-- name: UpdateUserProfile :one
UPDATE users SET display_name = $2 WHERE id = $1 RETURNING *;
