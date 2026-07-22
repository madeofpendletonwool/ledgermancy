-- name: CreateHousehold :one
INSERT INTO households (name)
VALUES ($1)
RETURNING *;

-- name: GetHousehold :one
SELECT * FROM households WHERE id = $1;

-- name: RenameHousehold :one
UPDATE households SET name = $2 WHERE id = $1 RETURNING *;

-- name: ListHouseholdMembers :many
SELECT id, household_id, email, display_name, created_at
FROM users
WHERE household_id = $1
ORDER BY created_at;

-- name: CreateInvite :one
INSERT INTO household_invites (household_id, email, token_hash, invited_by, expires_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetInviteByTokenHash :one
-- Only returns invites that are still usable, so callers cannot accidentally
-- accept an expired or already-redeemed invitation.
SELECT * FROM household_invites
WHERE token_hash = $1
  AND accepted_at IS NULL
  AND expires_at > now();

-- name: MarkInviteAccepted :exec
UPDATE household_invites SET accepted_at = now() WHERE id = $1;

-- name: ListPendingInvites :many
SELECT id, household_id, email, invited_by, expires_at, created_at
FROM household_invites
WHERE household_id = $1 AND accepted_at IS NULL AND expires_at > now()
ORDER BY created_at DESC;

-- name: DeleteInvite :exec
DELETE FROM household_invites WHERE id = $1 AND household_id = $2;
