-- Goals: savings/target goals and the derived-progress lookups behind them.
-- Every read is scoped so a caller sees their household's shared goals plus
-- their own, never another member's private goal.
--
-- Three scopes, and the visibility rule differs per scope:
--   household — everyone in the household
--   user      — only the login that owns it
--   person    — the person it belongs to, plus any adult (a parent manages a
--               child's goals, and a child who cannot see their own goal has
--               no teaching surface at all)
--
-- `all_person_goals` carries the adult/child decision into the SQL rather than
-- letting each caller re-derive it. It is set from the caller's role, which is
-- checked server-side; a child session always passes false.

-- name: CreateGoal :one
INSERT INTO goals (
    household_id, scope, user_id, person_id, kind, name, target_amount,
    target_date, account_id, category_id
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- name: ListGoals :many
-- Active goals visible to the caller.
SELECT * FROM goals
WHERE household_id = sqlc.arg('household_id')
  AND archived_at IS NULL
  AND (
        scope = 'household'
     OR (scope = 'user'   AND user_id = sqlc.narg('user_id'))
     OR (scope = 'person' AND (sqlc.arg('all_person_goals')::boolean
                               OR person_id = sqlc.narg('person_id')))
  )
ORDER BY created_at DESC;

-- name: GetGoal :one
SELECT * FROM goals
WHERE id = sqlc.arg('id')
  AND household_id = sqlc.arg('household_id')
  AND (
        scope = 'household'
     OR (scope = 'user'   AND user_id = sqlc.narg('user_id'))
     OR (scope = 'person' AND (sqlc.arg('all_person_goals')::boolean
                               OR person_id = sqlc.narg('person_id')))
  );

-- name: UpdateGoal :one
UPDATE goals
SET name = sqlc.arg('name'),
    target_amount = sqlc.arg('target_amount'),
    target_date = sqlc.narg('target_date'),
    account_id = sqlc.narg('account_id'),
    category_id = sqlc.narg('category_id')
WHERE id = sqlc.arg('id')
  AND household_id = sqlc.arg('household_id')
  AND (
        scope = 'household'
     OR (scope = 'user'   AND user_id = sqlc.narg('user_id'))
     OR (scope = 'person' AND (sqlc.arg('all_person_goals')::boolean
                               OR person_id = sqlc.narg('person_id')))
  )
RETURNING *;

-- name: ArchiveGoal :exec
UPDATE goals
SET archived_at = now()
WHERE id = sqlc.arg('id')
  AND household_id = sqlc.arg('household_id')
  AND (
        scope = 'household'
     OR (scope = 'user'   AND user_id = sqlc.narg('user_id'))
     OR (scope = 'person' AND (sqlc.arg('all_person_goals')::boolean
                               OR person_id = sqlc.narg('person_id')))
  );

-- name: MarkGoalAchieved :exec
-- Stamps the first time progress reaches the target; the guard keeps the
-- original achievement time even if the producer re-runs.
UPDATE goals
SET achieved_at = now()
WHERE id = $1 AND household_id = $2 AND achieved_at IS NULL;

-- name: ListActiveHouseholdGoals :many
-- Household-scoped active goals only. The insight feed is household-shared and
-- has no per-user visibility, so the coaching producer coaches shared goals
-- exclusively — a personal goal never leaks into a feed the whole household
-- reads. Personal goals still work everywhere else (CRUD + feasibility on the
-- Goals page).
SELECT * FROM goals
WHERE household_id = $1 AND scope = 'household' AND archived_at IS NULL;

-- name: GetGoalAccountBalance :one
-- Current balance of a goal's linked account, scoped to the household so a goal
-- can never read another household's account. Coalesced to 0 when unknown.
SELECT COALESCE(a.current_balance, 0)::numeric AS balance
FROM accounts a
JOIN plaid_items i ON i.id = a.plaid_item_id
JOIN users u       ON u.id = i.user_id
WHERE a.id = $1 AND u.household_id = $2;

-- name: GetGoalDebtTerms :one
-- Everything a debt-payoff goal needs to know about its linked account: whether
-- it is a debt at all, what is owed on it right now, and the terms to amortize
-- at — the institution's, and the household's own, which override them.
--
-- Replaces GetGoalLiability, which selected FROM liabilities and so answered
-- "does Plaid serve terms for this account?" when the question is "is this
-- account a debt?". Those are different questions with different answers: Plaid
-- serves the Liabilities product for a minority of institutions, and the answer
-- to the first was no for every debt the household that found this bug had. The
-- gate is now a.type, matching ComputeNetWorth and money.ts isLiability().
--
-- Scoped through the same account → plaid_item → user chain as
-- GetGoalAccountBalance, and deliberately with the same household-wide rule
-- rather than the stricter is_shared one, so no goal that resolves today stops
-- resolving.
--
-- BALANCE is the account's current_balance, never liabilities.balance: the
-- latter is the last statement balance for a card, and a payoff schedule must
-- start from what is owed now. Coalesced to 0 when unknown, as
-- GetGoalAccountBalance does.
--
-- Student loans and mortgages report interest_rate_percentage rather than apr,
-- so both are returned and mergeDebtTerms falls back between them.
SELECT
    a.type,
    a.subtype,
    COALESCE(a.current_balance, 0)::numeric AS balance,
    t.apr             AS manual_apr,
    t.minimum_payment AS manual_minimum_payment,
    -- The scheduled bill outranks the typed minimum payment, so a schedule
    -- edited on the bill calendar and a payoff projection can never quote
    -- different numbers for the same payment.
    o.amount          AS obligation_amount,
    l.apr,
    l.interest_rate_percentage,
    l.minimum_payment
FROM accounts a
JOIN plaid_items i        ON i.id = a.plaid_item_id
JOIN users u              ON u.id = i.user_id
LEFT JOIN liabilities l   ON l.account_id = a.id
LEFT JOIN account_terms t ON t.account_id = a.id
LEFT JOIN recurring_obligations o
       ON o.id = t.payment_obligation_id AND o.is_active
WHERE a.id = $1 AND u.household_id = $2;

-- --------------------------------------------------------------------------
-- Contributions
-- --------------------------------------------------------------------------
--
-- ATTRIBUTION, not progress. For an account-linked goal, progress remains the
-- account balance (see the 00012 header rule: progress is DERIVED, never
-- stored); these rows record who funded what. For an unlinked goal they are the
-- natural progress source. Never let the two become one number.

-- name: CreateGoalContribution :one
-- Goal and person are both re-resolved through the household guard rather than
-- trusted from the request.
INSERT INTO goal_contributions (goal_id, person_id, amount, occurred_on, note)
SELECT g.id, p.id, sqlc.arg('amount')::numeric,
       sqlc.arg('occurred_on')::date, sqlc.narg('note')
FROM goals g
JOIN household_people p
  ON p.id = sqlc.arg('person_id') AND p.household_id = g.household_id
WHERE g.id = sqlc.arg('goal_id')
  AND g.household_id = sqlc.arg('household_id')
RETURNING *;

-- name: ListGoalContributions :many
SELECT c.*, p.display_name
FROM goal_contributions c
JOIN goals g            ON g.id = c.goal_id
JOIN household_people p ON p.id = c.person_id
WHERE c.goal_id = $1 AND g.household_id = $2
ORDER BY c.occurred_on DESC, c.created_at DESC;

-- name: SumGoalContributionsByPerson :many
-- The "who funded what" breakdown. Ordered by size so the biggest contributor
-- reads first.
SELECT p.id AS person_id, p.display_name, SUM(c.amount)::numeric AS total
FROM goal_contributions c
JOIN goals g            ON g.id = c.goal_id
JOIN household_people p ON p.id = c.person_id
WHERE c.goal_id = $1 AND g.household_id = $2
GROUP BY p.id, p.display_name
ORDER BY SUM(c.amount) DESC;

-- name: SumGoalContributions :one
SELECT COALESCE(SUM(c.amount), 0)::numeric AS total
FROM goal_contributions c
JOIN goals g ON g.id = c.goal_id
WHERE c.goal_id = $1 AND g.household_id = $2;

-- name: DeleteGoalContribution :execrows
DELETE FROM goal_contributions c
USING goals g
WHERE c.id = sqlc.arg('id')
  AND g.id = c.goal_id
  AND g.household_id = sqlc.arg('household_id');
