-- Allowance: a schedule, and a ledger of what actually happened.
--
-- SIGN CONVENTION: allowance_entries.amount is POSITIVE for money INTO the
-- child's balance and NEGATIVE for money out — the OPPOSITE of
-- transactions.amount. See the migration comment for why. Nothing in this file
-- touches `transactions`, and nothing should: allowance entries are not
-- transactions and must never reach household spending totals.
--
-- Every query is scoped through household_people.household_id, so a caller
-- cannot read or write another household's ledger.

-- name: UpsertAllowance :one
INSERT INTO allowances (person_id, amount, cadence, monthly_limit, auto_post)
SELECT p.id, sqlc.narg('amount')::numeric, sqlc.narg('cadence')::text,
       sqlc.narg('monthly_limit')::numeric, sqlc.arg('auto_post')
FROM household_people p
WHERE p.id = sqlc.arg('person_id') AND p.household_id = sqlc.arg('household_id')
ON CONFLICT (person_id) DO UPDATE SET
    amount        = EXCLUDED.amount,
    cadence       = EXCLUDED.cadence,
    monthly_limit = EXCLUDED.monthly_limit,
    auto_post     = EXCLUDED.auto_post
RETURNING *;

-- name: GetAllowance :one
SELECT a.* FROM allowances a
JOIN household_people p ON p.id = a.person_id
WHERE a.person_id = $1 AND p.household_id = $2;

-- name: ListAllowances :many
SELECT a.*, p.display_name
FROM allowances a
JOIN household_people p ON p.id = a.person_id
WHERE p.household_id = $1
ORDER BY p.display_name;

-- name: DeleteAllowance :execrows
DELETE FROM allowances a
USING household_people p
WHERE a.person_id = sqlc.arg('person_id')
  AND p.id = a.person_id
  AND p.household_id = sqlc.arg('household_id');

-- name: CreateAllowanceEntry :one
-- The person_id is resolved through the household guard rather than trusted
-- from the request, so a valid person id from another household inserts
-- nothing instead of writing across the boundary.
INSERT INTO allowance_entries (person_id, kind, amount, occurred_on, note, created_by)
SELECT p.id, sqlc.arg('kind'), sqlc.arg('amount')::numeric,
       sqlc.arg('occurred_on')::date, sqlc.narg('note'), sqlc.narg('created_by')
FROM household_people p
WHERE p.id = sqlc.arg('person_id') AND p.household_id = sqlc.arg('household_id')
RETURNING *;

-- name: ListAllowanceEntries :many
SELECT e.* FROM allowance_entries e
JOIN household_people p ON p.id = e.person_id
WHERE e.person_id = $1 AND p.household_id = $2
ORDER BY e.occurred_on DESC, e.created_at DESC
LIMIT $3;

-- name: GetAllowanceBalance :one
-- DERIVED, never stored — the same rule goals follow. Summing signed amounts is
-- the whole computation; a stored balance would be one more thing that can
-- disagree with its own ledger.
SELECT COALESCE(SUM(e.amount), 0)::numeric AS balance
FROM allowance_entries e
JOIN household_people p ON p.id = e.person_id
WHERE e.person_id = $1 AND p.household_id = $2;

-- name: GetAllowanceSpentInMonth :one
-- Spending only (negative entries), returned as a positive magnitude, for
-- comparison against allowances.monthly_limit.
SELECT COALESCE(-SUM(e.amount), 0)::numeric AS spent
FROM allowance_entries e
JOIN household_people p ON p.id = e.person_id
WHERE e.person_id = $1
  AND p.household_id = $2
  AND e.amount < 0
  AND e.occurred_on >= sqlc.arg('month_start')::date
  AND e.occurred_on <  sqlc.arg('month_end')::date;

-- name: DeleteAllowanceEntry :execrows
DELETE FROM allowance_entries e
USING household_people p
WHERE e.id = sqlc.arg('id')
  AND p.id = e.person_id
  AND p.household_id = sqlc.arg('household_id');

-- name: ListAutoPostAllowances :many
-- Drives the scheduled post. `last_posted_for` is the idempotency key: a job
-- that runs twice in one period sees its own previous write and skips.
SELECT a.*, p.household_id, p.display_name
FROM allowances a
JOIN household_people p ON p.id = a.person_id
WHERE a.auto_post
  AND a.amount IS NOT NULL
  AND a.cadence IS NOT NULL
  AND (a.last_posted_for IS NULL OR a.last_posted_for < sqlc.arg('period_start')::date);

-- name: MarkAllowancePosted :execrows
-- The predicate repeats the staleness check so two workers racing on the same
-- period cannot both post: the loser updates zero rows and skips.
UPDATE allowances
SET last_posted_for = sqlc.arg('period_start')::date
WHERE person_id = sqlc.arg('person_id')
  AND (last_posted_for IS NULL OR last_posted_for < sqlc.arg('period_start')::date);
