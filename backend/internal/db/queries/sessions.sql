-- name: CreateSession :one
INSERT INTO sessions (user_id, token_hash, user_agent, client_ip, expires_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetSessionUser :one
-- Resolves a cookie token straight to the authenticated user in one round
-- trip. Both expiry checks live in the SQL rather than in Go so there is no
-- window where a stale session authenticates a request:
--   expires_at   — the absolute cap, set at login (30 days)
--   last_used_at — the idle cap, so an abandoned session dies sooner
SELECT
    s.id           AS session_id,
    s.expires_at   AS session_expires_at,
    s.last_used_at AS session_last_used_at,
    u.id           AS user_id,
    u.household_id,
    u.email,
    u.display_name
FROM sessions s
JOIN users u ON u.id = s.user_id
WHERE s.token_hash = $1
  AND s.expires_at > now()
  AND s.last_used_at > now() - sqlc.arg(idle_ttl)::interval;

-- name: TouchSession :exec
-- Records activity on a session. The predicate makes this a no-op unless the
-- stored value is already stale, so an active browser does not turn every read
-- request into a database write.
UPDATE sessions
SET last_used_at = now()
WHERE id = $1
  AND last_used_at < now() - sqlc.arg(min_age)::interval;

-- name: ListUserSessions :many
-- Powers the "active sessions" list. Ordered most-recently-used first, which
-- is the order someone scanning for an unfamiliar device wants.
SELECT id, user_agent, client_ip, last_used_at, expires_at, created_at
FROM sessions
WHERE user_id = $1 AND expires_at > now()
ORDER BY last_used_at DESC;

-- name: DeleteSession :exec
DELETE FROM sessions WHERE token_hash = $1;

-- name: DeleteSessionByID :execrows
-- user_id is part of the predicate, so a caller cannot revoke someone else's
-- session even with a valid id. execrows lets the handler tell "not yours"
-- from "deleted" without a second query.
DELETE FROM sessions WHERE id = $1 AND user_id = $2;

-- name: DeleteUserSessions :exec
-- Logs a user out everywhere, e.g. after a password change.
DELETE FROM sessions WHERE user_id = $1;

-- name: DeleteOtherUserSessions :execrows
-- Logs a user out everywhere except the browser they are currently using.
-- Used after enabling MFA and after a password change, where the point is to
-- evict anyone else without making the user sign in again themselves.
DELETE FROM sessions WHERE user_id = $1 AND token_hash <> $2;

-- name: DeleteExpiredSessions :execrows
-- Covers both caps, so idle-expired rows are collected too.
DELETE FROM sessions
WHERE expires_at <= now()
   OR last_used_at < now() - sqlc.arg(idle_ttl)::interval;
