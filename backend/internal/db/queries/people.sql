-- Household people: everyone the household's money can be ABOUT, whether or not
-- they can sign in. Every read here is household-scoped, so one household can
-- never enumerate or address another's people.
--
-- The join to `users` is a LEFT JOIN everywhere on purpose. A person with no
-- login is the normal case for a child, and an INNER JOIN would silently drop
-- exactly the rows this table was added for.

-- name: CreatePerson :one
INSERT INTO household_people (household_id, user_id, display_name, birthdate, is_dependent)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: ListPeople :many
-- Adults first, then dependents, each alphabetically — the order the Household
-- page renders and the order a "whose money is this" picker wants.
SELECT
    p.*,
    u.email,
    u.role
FROM household_people p
LEFT JOIN users u ON u.id = p.user_id
WHERE p.household_id = $1
ORDER BY p.is_dependent, p.display_name;

-- name: GetPerson :one
SELECT
    p.*,
    u.email,
    u.role
FROM household_people p
LEFT JOIN users u ON u.id = p.user_id
WHERE p.id = $1 AND p.household_id = $2;

-- name: GetPersonByUserID :one
-- The caller's own person row. Used to resolve "my birthdate" without the
-- client having to know its own person id.
SELECT * FROM household_people WHERE user_id = $1;

-- name: UpdatePerson :one
-- household_id is in the predicate, so a caller cannot edit another
-- household's person even with a valid id.
UPDATE household_people
SET display_name = $3,
    birthdate    = $4,
    is_dependent = $5
WHERE id = $1 AND household_id = $2
RETURNING *;

-- name: LinkPersonToUser :one
-- Attaches a freshly created login to an existing person. Guarded on user_id
-- IS NULL so accepting a second invite cannot steal a person who already has
-- a login.
UPDATE household_people
SET user_id = $3
WHERE id = $1 AND household_id = $2 AND user_id IS NULL
RETURNING *;

-- name: DeletePerson :execrows
-- Refuses while a login still exists: unlink or delete the user first. Deleting
-- a person cascades their allowance, contributions and splits, and that is too
-- much to do implicitly to somebody who can still sign in.
DELETE FROM household_people
WHERE id = $1 AND household_id = $2 AND user_id IS NULL;

-- name: CountAdultsInHousehold :one
-- Adults = logins that are not children. Guards the last-adult cases: an owner
-- cannot demote or delete themselves into a household nobody can administer.
SELECT count(*) FROM users
WHERE household_id = $1 AND role IN ('owner', 'member');

-- name: SetUserRole :one
-- household_id is in the predicate for the same reason as everywhere else.
UPDATE users SET role = $3
WHERE id = $1 AND household_id = $2
RETURNING id, household_id, email, display_name, role;

-- name: GetUserRole :one
SELECT role FROM users WHERE id = $1;

-- name: DeleteUser :exec
-- Removes a login. household_people.user_id is ON DELETE SET NULL, so the
-- person survives with their accounts, goals, allowance and history intact —
-- revoking a teenager's login must not erase the child their 529 points at.
DELETE FROM users WHERE id = $1 AND household_id = $2;
