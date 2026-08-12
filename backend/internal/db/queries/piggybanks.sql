-- Piggy banks: lightweight savings envelopes on an asset account.
--
-- A piggy bank's current balance is DERIVED from its event log (deposits minus
-- withdrawals), never stored — see 00061_piggy_banks.sql for why. Every read
-- here sums the events in SQL, so a balance and the events behind it cannot
-- disagree.
--
-- Visibility is household-scoped, matching the spec: a piggy bank is a shared
-- household object (there is no per-user or per-person scope the way goals
-- have). The account it sits on is itself household-scoped through
-- account_access, and every query here re-checks household_id on the piggy
-- bank row so a caller can never read or move another household's jar.

-- name: CreatePiggyBank :one
INSERT INTO piggy_banks (household_id, account_id, name, target_amount)
VALUES ($1, $2, $3, sqlc.narg('target_amount')::numeric)
RETURNING *;

-- name: ListPiggyBanks :many
-- Every piggy bank in the household with its DERIVED current balance. The
-- balance is deposits minus withdrawals, summed here in SQL; the outer
-- ::numeric cast is the one proven place for it (casting each COALESCE
-- separately leaves sqlc unable to infer the result type).
SELECT
    p.*,
    (COALESCE(SUM(e.amount) FILTER (WHERE e.type = 'deposit'), 0)
   - COALESCE(SUM(e.amount) FILTER (WHERE e.type = 'withdraw'), 0))::numeric AS current_amount
FROM piggy_banks p
LEFT JOIN piggy_bank_events e ON e.piggy_bank_id = p.id
WHERE p.household_id = $1
GROUP BY p.id
ORDER BY p.created_at DESC;

-- name: ListPiggyBanksForAccount :many
-- The piggy banks drawing from one account. household_id is on the piggy bank,
-- so even an account_id from outside the household yields no rows — no separate
-- account-visibility check is needed to keep this safe.
SELECT
    p.*,
    (COALESCE(SUM(e.amount) FILTER (WHERE e.type = 'deposit'), 0)
   - COALESCE(SUM(e.amount) FILTER (WHERE e.type = 'withdraw'), 0))::numeric AS current_amount
FROM piggy_banks p
LEFT JOIN piggy_bank_events e ON e.piggy_bank_id = p.id
WHERE p.household_id = $1 AND p.account_id = $2
GROUP BY p.id
ORDER BY p.created_at DESC;

-- name: GetPiggyBank :one
-- One piggy bank plus its DERIVED current balance, scoped to the household.
-- The deposit/withdraw handlers read this to validate ownership and (for a
-- withdrawal) to check the balance covers the amount.
SELECT
    p.*,
    (COALESCE((SELECT SUM(e.amount) FROM piggy_bank_events e
               WHERE e.piggy_bank_id = p.id AND e.type = 'deposit'), 0)
   - COALESCE((SELECT SUM(e.amount) FROM piggy_bank_events e
               WHERE e.piggy_bank_id = p.id AND e.type = 'withdraw'), 0))::numeric AS current_amount
FROM piggy_banks p
WHERE p.id = $1 AND p.household_id = $2;

-- name: UpdatePiggyBank :one
-- Name and target only. The account is fixed at create time: moving a jar
-- between accounts would detach it from the events' funding account, and the
-- available-for-piggy math is per-account.
UPDATE piggy_banks
SET name = sqlc.arg('name'),
    target_amount = sqlc.narg('target_amount')::numeric
WHERE id = sqlc.arg('id') AND household_id = sqlc.arg('household_id')
RETURNING *;

-- name: DeletePiggyBank :execrows
-- Hard delete; the event log cascades away with it. A piggy bank is casual by
-- design — closing one out (you bought the laptop) is deletion, not archiving,
-- and the money is already in the account so there is nothing to reconcile.
DELETE FROM piggy_banks WHERE id = $1 AND household_id = $2;

-- name: CreatePiggyBankEvent :one
-- Append-only. type and amount come from the caller; ownership is checked in
-- the handler via GetPiggyBank before this runs, and both run inside one
-- transaction so a withdrawal's balance check and its event land together.
INSERT INTO piggy_bank_events (piggy_bank_id, type, amount, transaction_id)
VALUES ($1, sqlc.arg('type'), sqlc.arg('amount')::numeric, sqlc.narg('transaction_id'))
RETURNING *;

-- name: ListPiggyBankEvents :many
SELECT e.*
FROM piggy_bank_events e
JOIN piggy_banks p ON p.id = e.piggy_bank_id
WHERE e.piggy_bank_id = $1 AND p.household_id = $2
ORDER BY e.created_at DESC;

-- name: GetAccountAvailableForPiggy :one
-- The unused balance on an account: its current balance minus the sum of every
-- piggy bank's derived balance drawing from it. Negative means the household
-- has earmarked more than the account holds — the over-allocation case the UI
-- must surface as a loud warning, never a silently clipped number.
--
-- Restricted to asset accounts (type NOT IN credit/loan): a piggy bank is money
-- held, not owed, and the create path rejects a debt account. The household
-- join is through account_access, the same view every other account read uses.
SELECT
    (COALESCE(a.current_balance, 0)
     - COALESCE((
         SELECT (COALESCE(SUM(e.amount) FILTER (WHERE e.type = 'deposit'), 0)
               - COALESCE(SUM(e.amount) FILTER (WHERE e.type = 'withdraw'), 0))::numeric
         FROM piggy_bank_events e
         JOIN piggy_banks p ON p.id = e.piggy_bank_id
         WHERE p.account_id = a.id AND p.household_id = v.household_id
       ), 0))::numeric AS available,
    COALESCE(a.current_balance, 0)::numeric AS account_balance,
    COALESCE((
         SELECT (COALESCE(SUM(e.amount) FILTER (WHERE e.type = 'deposit'), 0)
               - COALESCE(SUM(e.amount) FILTER (WHERE e.type = 'withdraw'), 0))::numeric
         FROM piggy_bank_events e
         JOIN piggy_banks p ON p.id = e.piggy_bank_id
         WHERE p.account_id = a.id AND p.household_id = v.household_id
       ), 0)::numeric AS assigned
FROM accounts a
JOIN account_access v ON v.account_id = a.id
WHERE a.id = $1 AND v.household_id = $2
  AND a.type NOT IN ('credit', 'loan');
