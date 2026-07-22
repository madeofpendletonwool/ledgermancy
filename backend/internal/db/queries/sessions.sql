-- name: CreateSession :one
INSERT INTO sessions (user_id, token_hash, user_agent, expires_at)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetSessionUser :one
-- Resolves a cookie token straight to the authenticated user in one round
-- trip. Expired sessions are filtered out here rather than in Go so there is
-- no window where a stale session authenticates a request.
SELECT
    s.id           AS session_id,
    s.expires_at   AS session_expires_at,
    u.id           AS user_id,
    u.household_id,
    u.email,
    u.display_name
FROM sessions s
JOIN users u ON u.id = s.user_id
WHERE s.token_hash = $1 AND s.expires_at > now();

-- name: DeleteSession :exec
DELETE FROM sessions WHERE token_hash = $1;

-- name: DeleteUserSessions :exec
-- Logs a user out everywhere, e.g. after a password change.
DELETE FROM sessions WHERE user_id = $1;

-- name: DeleteExpiredSessions :execrows
DELETE FROM sessions WHERE expires_at <= now();
