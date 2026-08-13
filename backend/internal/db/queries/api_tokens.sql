-- name: CreateAPIToken :one
INSERT INTO api_tokens (user_id, name, token_hash, scopes, expires_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, name, scopes, last_used_at, expires_at, created_at;

-- name: GetAPITokenUser :one
-- Resolves a bearer token straight to the authenticated user in one round trip,
-- returning the SAME columns GetSessionUser does. That symmetry is the point:
-- the middleware builds one Identity from either row, so every handler's
-- visibility scoping is unchanged by which credential arrived.
--
-- Expiry lives in the SQL rather than in Go, exactly as it does for sessions, so
-- there is no window where a lapsed token authenticates a request. There is no
-- idle cap here — a token that has sat unused for a month is an integration that
-- runs monthly, not an abandoned browser.
--
-- The LEFT JOIN on household_people matches GetSessionUser: a login whose person
-- row was removed still authenticates, it simply has no person-scoped data.
SELECT
    t.id           AS token_id,
    t.scopes       AS token_scopes,
    t.last_used_at AS token_last_used_at,
    u.id           AS user_id,
    u.household_id,
    u.email,
    u.display_name,
    u.role,
    p.id           AS person_id
FROM api_tokens t
JOIN users u ON u.id = t.user_id
LEFT JOIN household_people p ON p.user_id = u.id
WHERE t.token_hash = $1
  AND (t.expires_at IS NULL OR t.expires_at > now());

-- name: TouchAPIToken :exec
-- Records activity on a token. The predicate makes this a no-op unless the
-- stored value is already stale, so an integration polling on a timer does not
-- turn every read into a database write. A token that has never been used has a
-- NULL last_used_at and is always written, so the first call is visible
-- immediately rather than after the coalescing window.
UPDATE api_tokens
SET last_used_at = now()
WHERE id = $1
  AND (last_used_at IS NULL OR last_used_at < now() - sqlc.arg(min_age)::interval);

-- name: ListUserAPITokens :many
-- Powers the revoke list. The hash is deliberately absent: nothing outside
-- authentication has any use for it, and a credential digest has no business in
-- a JSON response.
SELECT id, name, scopes, last_used_at, expires_at, created_at
FROM api_tokens
WHERE user_id = $1
ORDER BY created_at DESC;

-- name: DeleteAPIToken :execrows
-- user_id is part of the predicate, so a caller cannot revoke someone else's
-- token even with a valid id — the same shape as DeleteSessionByID. execrows
-- lets the handler tell "not yours" from "deleted" without a second query.
DELETE FROM api_tokens WHERE id = $1 AND user_id = $2;

-- name: DeleteExpiredAPITokens :execrows
-- Housekeeping only. GetAPITokenUser already refuses an expired token, so a row
-- left here is never honoured; this just stops them accumulating.
DELETE FROM api_tokens WHERE expires_at IS NOT NULL AND expires_at <= now();
