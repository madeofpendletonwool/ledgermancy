-- The allocation planner (doc 32): saved plans, the cash-drag inputs, and the
-- college-goal beneficiaries.
--
-- Every read here carries the same visibility scoping as the rest of the
-- reporting layer, resolved through the account_access view: household
-- membership plus `v.user_id = $2 OR v.is_shared`. A plan must never put money
-- into a bucket the caller cannot see, and a spouse's private account must never
-- appear in one.

-- --------------------------------------------------------------------------
-- Saved plans
-- --------------------------------------------------------------------------

-- name: CreateAllocationPlan :one
-- inputs and assumptions are JSONB, and every money field inside them is a
-- decimal STRING. See the migration's "Money in JSONB" note — export.go casts
-- numeric COLUMNS to text but cannot reach inside a jsonb value, so the
-- continuity guarantee only holds because the writer never puts a JSON number
-- there in the first place.
INSERT INTO allocation_plans (household_id, created_by, name, inputs, input_version, assumptions)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: ListAllocationPlans :many
SELECT * FROM allocation_plans
WHERE household_id = $1
ORDER BY updated_at DESC;

-- name: GetAllocationPlan :one
-- Household-scoped in the WHERE clause rather than checked afterwards: a plan
-- id from another household yields no row, which the handler turns into a 404.
SELECT * FROM allocation_plans WHERE id = $1 AND household_id = $2;

-- name: UpdateAllocationPlan :one
UPDATE allocation_plans
SET name          = $3,
    inputs        = $4,
    input_version = $5,
    assumptions   = $6,
    updated_at    = now()
WHERE id = $1 AND household_id = $2
RETURNING *;

-- name: DeleteAllocationPlan :execrows
-- execrows so the handler can distinguish "deleted" from "there was nothing of
-- yours by that id" and answer 404 rather than a cheerful 204.
DELETE FROM allocation_plans WHERE id = $1 AND household_id = $2;

-- --------------------------------------------------------------------------
-- Cash drag
-- --------------------------------------------------------------------------

-- name: ListVisibleDepositAccounts :many
-- Depository accounts with whatever yield the household has told us.
--
-- deposit_apy comes through as-is, NULL included. NULL means UNKNOWN and the
-- detector stays silent on it: the column exists precisely because Plaid does
-- not serve deposit yields reliably, so an empty field is "nobody has said",
-- never "this account earns nothing". Reading it as zero would report a
-- high-yield savings account as the household's worst cash drag.
SELECT
    a.id,
    a.name,
    a.subtype,
    a.current_balance,
    a.deposit_apy,
    v.institution_name
FROM accounts a
JOIN account_access v ON v.account_id = a.id
WHERE v.household_id = $1
  AND (v.user_id = $2 OR v.is_shared)
  AND a.is_active
  AND a.type = 'depository'
ORDER BY v.institution_name NULLS LAST, a.name;

-- name: SetAccountDepositAPY :one
-- Ownership is enforced inside the statement, the same way the tax-treatment
-- and contribution writes are: the join yields no row when the caller cannot
-- see the account, so nothing is written and the handler sees pgx.ErrNoRows.
UPDATE accounts a
SET deposit_apy = sqlc.narg('deposit_apy')::numeric
FROM account_access v
WHERE a.id = sqlc.arg('account_id')
  AND v.account_id = a.id
  AND v.household_id = sqlc.arg('household_id')
  AND (v.user_id = sqlc.arg('user_id') OR v.is_shared)
RETURNING a.id, a.name, a.deposit_apy;

-- --------------------------------------------------------------------------
-- College goals
-- --------------------------------------------------------------------------

-- name: ListCollegeGoals :many
-- Active college goals with everything the drawdown needs to resolve an
-- enrollment year.
--
-- The beneficiary's BIRTHDATE is preferred over any stored integer, for the
-- reason networth.ResolveAge exists: a stored age is correct the day it is
-- typed and wrong every year after, and a college projection is the longest
-- horizon in the app. beneficiary_current_age/beneficiary_target_age come from
-- the linked account's contribution plan, which is where doc 15 already put the
-- 529 horizon — this reads that rather than adding a second place to set it.
--
-- The birthdate is resolved through TWO links, in order: the goal's own
-- person_id, then the linked account's beneficiary_person_id. The goal link is
-- the explicit one, but a goal scoped 'household' cannot carry a person_id
-- (the goals_scope_target CHECK forces it NULL), and for such a goal the 529's
-- beneficiary is the only record of whose money this is. That is the same join
-- ListProjectableAccounts makes (a.beneficiary_person_id -> household_people),
-- so this query and the net-worth projection agree about the horizon — without
-- it, a household that correctly set the beneficiary on the 529 but never
-- opened the goal's person scope would be told the age "is not on file" while
-- every other surface in the app had it.
SELECT
    g.id,
    g.name,
    g.target_amount,
    g.target_date,
    g.college_years,
    g.account_id,
    COALESCE(gp.birthdate, ap.birthdate) AS beneficiary_birthdate,
    c.beneficiary_current_age,
    c.beneficiary_target_age
FROM goals g
LEFT JOIN household_people      gp ON gp.id = g.person_id
LEFT JOIN accounts              a  ON a.id  = g.account_id
LEFT JOIN household_people      ap ON ap.id = a.beneficiary_person_id
LEFT JOIN account_contributions c  ON c.account_id = g.account_id
WHERE g.household_id = $1
  AND g.kind = 'college'
  AND g.archived_at IS NULL
ORDER BY g.name;
