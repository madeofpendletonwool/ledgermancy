-- Bill split and the household reimbursement ledger.
--
-- A split is an ATTRIBUTION OVERLAY. The transaction happened once, on one
-- account, and household spending totals must be unchanged by splitting it.
-- Nothing in this file is joined into a spending aggregate, and nothing should
-- be — only per-person views consult these rows. Anything else and the app
-- starts inflating spend the same way un-typed transfers used to.
--
-- Balances are DERIVED by summing unsettled shares. There is no stored balance.

-- name: ReplaceTransactionSplits :exec
-- Splits are written as a set: the API resolves exact amounts, asserts they sum
-- to the transaction, and replaces whatever was there. Editing a split in place
-- would allow a partial write that no longer sums.
DELETE FROM transaction_splits WHERE transaction_id = $1;

-- name: CreateTransactionSplit :one
-- Both the transaction and the person are re-resolved through the household
-- guard rather than trusted from the request.
INSERT INTO transaction_splits (transaction_id, person_id, amount)
SELECT t.id, p.id, sqlc.arg('amount')::numeric
FROM transactions t
JOIN accounts a     ON a.id = t.account_id
JOIN plaid_items i  ON i.id = a.plaid_item_id
JOIN users u        ON u.id = i.user_id
JOIN household_people p
  ON p.id = sqlc.arg('person_id') AND p.household_id = u.household_id
WHERE t.id = sqlc.arg('transaction_id')
  AND u.household_id = sqlc.arg('household_id')
RETURNING *;

-- name: ListSplitsForTransaction :many
SELECT s.*, p.display_name
FROM transaction_splits s
JOIN household_people p ON p.id = s.person_id
WHERE s.transaction_id = $1 AND p.household_id = $2
ORDER BY p.display_name;

-- name: GetTransactionForSplit :one
-- The transaction a split is being written against, scoped to the household.
-- The handler needs `amount` to assert the shares sum exactly.
SELECT t.id, t.amount, t.name, t.date, i.user_id AS payer_user_id
FROM transactions t
JOIN accounts a    ON a.id = t.account_id
JOIN plaid_items i ON i.id = a.plaid_item_id
JOIN users u       ON u.id = i.user_id
WHERE t.id = $1 AND u.household_id = $2;

-- name: ListSplitTransactions :many
-- Every transaction in the household that carries splits, newest first. Powers
-- the "shared expenses" filter.
SELECT
    t.id,
    t.name,
    t.date,
    t.amount,
    i.user_id AS payer_user_id,
    payer.display_name AS payer_name,
    count(s.id)::bigint AS split_count,
    count(s.id) FILTER (WHERE s.settled_at IS NULL)::bigint AS unsettled_count
FROM transactions t
JOIN transaction_splits s ON s.transaction_id = t.id
JOIN accounts a    ON a.id = t.account_id
JOIN plaid_items i ON i.id = a.plaid_item_id
JOIN users u       ON u.id = i.user_id
LEFT JOIN household_people payer ON payer.user_id = i.user_id
WHERE u.household_id = $1
GROUP BY t.id, t.name, t.date, t.amount, i.user_id, payer.display_name
ORDER BY t.date DESC, t.id
LIMIT $2;

-- name: SettleSplit :execrows
UPDATE transaction_splits s
SET settled_at = now()
FROM household_people p
WHERE s.id = sqlc.arg('id')
  AND p.id = s.person_id
  AND p.household_id = sqlc.arg('household_id')
  AND s.settled_at IS NULL;

-- name: UnsettleSplit :execrows
UPDATE transaction_splits s
SET settled_at = NULL
FROM household_people p
WHERE s.id = sqlc.arg('id')
  AND p.id = s.person_id
  AND p.household_id = sqlc.arg('household_id');

-- name: HouseholdLedger :many
-- Who owes whom. One row per (debtor, creditor) pair with an outstanding
-- balance: the debtor is the person the share was assigned to, the creditor is
-- the person whose account actually paid.
--
-- A share assigned to the payer themselves is excluded — you do not owe
-- yourself, and including it would inflate every balance by the payer's own
-- portion. Settled shares drop out because settlement is what "this is done"
-- means; the row is kept for history, not for the balance.
SELECT
    debtor.id            AS debtor_id,
    debtor.display_name  AS debtor_name,
    creditor.id          AS creditor_id,
    creditor.display_name AS creditor_name,
    SUM(s.amount)::numeric AS amount
FROM transaction_splits s
JOIN transactions t ON t.id = s.transaction_id
JOIN accounts a     ON a.id = t.account_id
JOIN plaid_items i  ON i.id = a.plaid_item_id
JOIN users u        ON u.id = i.user_id
JOIN household_people debtor   ON debtor.id = s.person_id
JOIN household_people creditor ON creditor.user_id = i.user_id
WHERE u.household_id = $1
  AND s.settled_at IS NULL
  AND debtor.id <> creditor.id
GROUP BY debtor.id, debtor.display_name, creditor.id, creditor.display_name
HAVING SUM(s.amount) <> 0
ORDER BY debtor.display_name, creditor.display_name;

-- name: ListPersonSplits :many
-- One person's share of shared expenses. This is the ONLY kind of query that
-- may read splits for a spending figure, and it is explicitly per-person —
-- never summed into a household total.
SELECT
    s.id,
    s.amount,
    s.settled_at,
    t.id   AS transaction_id,
    t.name AS transaction_name,
    t.date,
    t.amount AS transaction_amount
FROM transaction_splits s
JOIN transactions t ON t.id = s.transaction_id
JOIN household_people p ON p.id = s.person_id
WHERE s.person_id = $1
  AND p.household_id = $2
ORDER BY t.date DESC
LIMIT $3;
