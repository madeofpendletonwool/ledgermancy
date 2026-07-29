-- name: CreateHousehold :one
INSERT INTO households (name)
VALUES ($1)
RETURNING *;

-- name: GetHousehold :one
SELECT * FROM households WHERE id = $1;

-- name: RenameHousehold :one
UPDATE households SET name = $2 WHERE id = $1 RETURNING *;

-- name: ListHouseholdMembers :many
SELECT id, household_id, email, display_name, role, created_at
FROM users
WHERE household_id = $1
ORDER BY created_at;

-- name: CreateInvite :one
-- `role` is the permission level the invite grants; `person_id` attaches the
-- new login to an EXISTING person rather than creating a second one. That is
-- how "enable a login for Ellie" works — without it, accepting the invite would
-- produce a duplicate Ellie with no 529 attached.
INSERT INTO household_invites (
    household_id, email, token_hash, invited_by, expires_at, role, person_id
)
VALUES ($1, $2, $3, $4, $5, $6, $7)
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
SELECT i.id, i.household_id, i.email, i.invited_by, i.expires_at, i.created_at,
       i.role, i.person_id, p.display_name AS person_name
FROM household_invites i
LEFT JOIN household_people p ON p.id = i.person_id
WHERE i.household_id = $1 AND i.accepted_at IS NULL AND i.expires_at > now()
ORDER BY i.created_at DESC;

-- name: DeleteInvite :exec
DELETE FROM household_invites WHERE id = $1 AND household_id = $2;
