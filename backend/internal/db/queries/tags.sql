-- Tag queries.
--
-- Two scoping rules run through every statement here, and they are different
-- rules on purpose:
--
--   * A TAG is household-scoped. Every read and write is guarded by
--     household_id = $1, so a tag id from another household matches nothing.
--   * A TAGGED TRANSACTION is visibility-scoped, exactly like every other
--     transaction read: account_access with
--     (v.user_id = $2 OR v.is_shared). A tag is a label, not an escape hatch —
--     labelling a charge on a private account must not make that charge, or its
--     amount, readable by the other member.
--
-- So both members see the tag "Summer Vacation"; each member's count and total
-- for it are computed over the rows that member was already allowed to see.
-- Every query below that touches transactions therefore takes the viewer's user
-- id as well as the household id.

-- name: ListTags :many
-- Every tag in the household, with the two figures the management page and the
-- tag picker need, both computed for THIS viewer.
--
-- This also serves the single-tag read, via the optional `id` narg. There is no
-- separate GetTag on purpose: a second statement would have to repeat the two
-- aggregate subqueries below, and the day one of them was fixed and the other
-- was not, a tag's total would depend on which endpoint you asked. One query,
-- one definition of the numbers.
--
-- The two numbers deliberately answer different questions:
--
--   transaction_count — how many visible transactions carry this tag. A fact
--     about the LABEL, so it counts every tagged row: income, transfers and
--     pending ones included. "You put this on 14 things" must not silently
--     become 11 because three of them were refunds.
--
--   total — what this tag has COST, all-time. A fact about the MONEY, so it
--     obeys the same rules as every other spending figure in the app: excluded
--     rows dropped, pending dropped, income and transfer categories dropped,
--     direction from is_spend so a loan payment counts as an outflow. This is
--     the figure that reads against expected_amount, and it must reconcile with
--     GetSpendingByTag over an unbounded window.
--
-- The two aggregates are computed in separate scalar subqueries rather than one
-- join, because joining both would multiply the rows and inflate whichever
-- aggregate is not the one being filtered.
SELECT
    t.id,
    t.name,
    t.description,
    t.expected_amount,
    t.created_at,
    (
        SELECT COUNT(*)::bigint
        FROM transaction_tags tt
        JOIN transactions tr    ON tr.id = tt.transaction_id
        JOIN accounts a         ON a.id = tr.account_id
        JOIN account_access v   ON v.account_id = a.id
        WHERE tt.tag_id = t.id
          AND v.household_id = sqlc.arg('household_id')
          AND (v.user_id = sqlc.arg('user_id') OR v.is_shared)
          AND a.is_active
    ) AS transaction_count,
    (
        SELECT COALESCE(SUM(ABS(tr.amount)), 0)::numeric
        FROM transaction_tags tt
        JOIN transactions tr    ON tr.id = tt.transaction_id
        JOIN accounts a         ON a.id = tr.account_id
        JOIN account_access v   ON v.account_id = a.id
        LEFT JOIN categories c  ON c.id = tr.category_id
        WHERE tt.tag_id = t.id
          AND v.household_id = sqlc.arg('household_id')
          AND (v.user_id = sqlc.arg('user_id') OR v.is_shared)
          AND a.is_active
          AND NOT tr.excluded_from_reports
          AND NOT tr.pending
          AND NOT COALESCE(c.is_income, FALSE)
          AND NOT COALESCE(c.is_transfer, FALSE)
          AND is_spend(tr.amount, a.type)
    ) AS total
FROM tags t
WHERE t.household_id = sqlc.arg('household_id')
  -- NULL means "every tag"; an id narrows to one. A tag id from another
  -- household is filtered out by the household guard above rather than by this
  -- clause, so it comes back as an empty result — indistinguishable from an id
  -- that never existed, which is what the handler turns into a 404.
  AND (sqlc.narg('id')::uuid IS NULL OR t.id = sqlc.narg('id')::uuid)
ORDER BY lower(t.name);

-- name: CreateTag :one
-- Returns the id only: the response is then re-read through ListTags, so a newly
-- created tag is rendered by the same code path (and the same aggregate
-- definitions) as one that has existed for a year.
INSERT INTO tags (household_id, name, description, expected_amount)
VALUES (sqlc.arg('household_id'), sqlc.arg('name'), sqlc.narg('description'),
        sqlc.narg('expected_amount'))
RETURNING id;

-- name: UpdateTag :one
-- The household_id guard is the authorisation: an id from another household
-- matches no rows, the handler reads pgx.ErrNoRows, and it returns 404 rather
-- than editing it.
UPDATE tags
SET name            = sqlc.arg('name'),
    description     = sqlc.narg('description'),
    expected_amount = sqlc.narg('expected_amount'),
    updated_at      = now()
WHERE id = sqlc.arg('id') AND household_id = sqlc.arg('household_id')
RETURNING id;

-- name: DeleteTag :execrows
-- transaction_tags cascades, so deleting a tag unlabels its transactions and
-- leaves the transactions themselves untouched. :execrows returns 0 for another
-- household's id, which the handler maps to 404.
DELETE FROM tags WHERE id = sqlc.arg('id') AND household_id = sqlc.arg('household_id');

-- name: ListTagsForTransactions :many
-- Every (transaction, tag) pair for a page of transactions, in one round trip.
--
-- This is what keeps tag chips off the N+1 path: the ledger renders fifty rows
-- and asks for their labels once, rather than once per row. The transaction ids
-- come from a list the caller has ALREADY read under the visibility predicate,
-- and the household guard here means an id smuggled in from elsewhere returns
-- nothing.
SELECT tt.transaction_id, t.id AS tag_id, t.name
FROM transaction_tags tt
JOIN tags t ON t.id = tt.tag_id
WHERE tt.transaction_id = ANY(sqlc.arg('transaction_ids')::uuid[])
  AND t.household_id = sqlc.arg('household_id')
ORDER BY lower(t.name);

-- name: ClearTransactionTags :exec
-- Removes every tag from one transaction, as the first half of "set the tags to
-- exactly this list". The transaction is re-checked against the caller's
-- visibility here rather than trusted from an earlier read, so this statement is
-- safe on its own: a transaction the caller cannot see matches nothing and the
-- delete is a no-op.
DELETE FROM transaction_tags tt
USING transactions tr, accounts a, account_access v
WHERE tt.transaction_id = sqlc.arg('transaction_id')
  AND tr.id = tt.transaction_id
  AND a.id = tr.account_id
  AND v.account_id = a.id
  AND v.household_id = sqlc.arg('household_id')
  AND (v.user_id = sqlc.arg('user_id') OR v.is_shared);

-- name: AddTransactionTag :execrows
-- Labels one transaction with one tag.
--
-- Both scopes are enforced in the SELECT, so this cannot be used to reach across
-- a boundary even with two valid-looking ids: the tag must belong to the
-- caller's household, and the transaction must be visible to the caller. A miss
-- on either inserts nothing and returns 0 rows.
--
-- ON CONFLICT DO NOTHING makes re-applying a tag idempotent — the composite
-- primary key already forbids the duplicate; this just means the caller does not
-- have to care whether the label was already there.
INSERT INTO transaction_tags (transaction_id, tag_id)
SELECT tr.id, tg.id
FROM transactions tr
JOIN accounts a       ON a.id = tr.account_id
JOIN account_access v ON v.account_id = a.id
JOIN tags tg          ON tg.id = sqlc.arg('tag_id')
WHERE tr.id = sqlc.arg('transaction_id')
  AND v.household_id = sqlc.arg('household_id')
  AND (v.user_id = sqlc.arg('user_id') OR v.is_shared)
  AND tg.household_id = sqlc.arg('household_id')
ON CONFLICT DO NOTHING;

-- name: ApplyTagToMerchant :execrows
-- Adds one tag to every visible transaction sharing a merchant descriptor — the
-- tag half of "apply to all from this merchant".
--
-- It ADDS rather than replaces, which is the one place tags and categories
-- differ on this action and the difference is deliberate. A category is
-- single-valued, so applying one to a merchant necessarily overwrites. A tag is
-- one of several, so replacing would silently strip labels somebody else put on
-- those charges for unrelated reasons ("Reimbursable" wiped by "Groceries
-- 2026"). Adding is the only version of this that cannot destroy work.
--
-- Scoped both ways, exactly like AddTransactionTag: only the caller's own
-- household's tag, only transactions the caller can see.
INSERT INTO transaction_tags (transaction_id, tag_id)
SELECT tr.id, tg.id
FROM transactions tr
JOIN accounts a       ON a.id = tr.account_id
JOIN account_access v ON v.account_id = a.id
JOIN tags tg          ON tg.id = sqlc.arg('tag_id')
WHERE tr.merchant_key = sqlc.arg('merchant_key')
  AND v.household_id = sqlc.arg('household_id')
  AND (v.user_id = sqlc.arg('user_id') OR v.is_shared)
  AND tg.household_id = sqlc.arg('household_id')
ON CONFLICT DO NOTHING;

-- name: CountHouseholdTags :one
-- How many of these tag ids are this household's. The bulk tag endpoint compares
-- the answer against the number it was given, so an id from another household
-- fails the request outright.
--
-- It cannot infer that from rows-written the way the single-row handler does: an
-- add that writes nothing is ambiguous between "no such tag" and "already
-- tagged", and treating the second as an error would make replaying an action
-- fail. Asking directly is the only version that is right both times.
SELECT count(DISTINCT id) FROM tags
WHERE id = ANY(sqlc.arg('tag_ids')::uuid[])
  AND household_id = sqlc.arg('household_id');

-- name: AddTagToTransactions :execrows
-- Adds one tag to an explicit set of transactions — the multi-select answer to
-- ApplyTagToMerchant's "everything from this merchant".
--
-- ADDS, never replaces, and the reasoning is ApplyTagToMerchant's verbatim: the
-- single-row editor can replace a tag set because it IS the confirmed set the
-- user just ticked, but a selection of fifty rows carries fifty different sets,
-- and replacing across them would silently strip labels put there for unrelated
-- reasons. RemoveTagFromTransactions below is the explicit undo.
--
-- Scoped both ways exactly like AddTransactionTag: only the caller's own
-- household's tag, only transactions the caller can see. An id from outside
-- either boundary inserts nothing rather than erroring, so a stale selection
-- degrades to "fewer rows changed" instead of failing the whole request.
--
-- ON CONFLICT DO NOTHING makes re-tagging an already-tagged row a no-op, which
-- is what lets the same action be applied twice without thinking about it.
INSERT INTO transaction_tags (transaction_id, tag_id)
SELECT DISTINCT tr.id, tg.id
FROM transactions tr
JOIN accounts a       ON a.id = tr.account_id
JOIN account_access v ON v.account_id = a.id
JOIN tags tg          ON tg.id = sqlc.arg('tag_id')
WHERE tr.id = ANY(sqlc.arg('transaction_ids')::uuid[])
  AND v.household_id = sqlc.arg('household_id')
  AND (v.user_id = sqlc.arg('user_id') OR v.is_shared)
  AND tg.household_id = sqlc.arg('household_id')
ON CONFLICT DO NOTHING;

-- name: RemoveTagFromTransactions :execrows
-- Strips one tag from an explicit set of transactions, and only that tag —
-- every other label on those rows survives. This is the undo for
-- AddTagToTransactions, and the reason the bulk action can be additive without
-- being a one-way door.
--
-- Visibility is re-checked here rather than trusted from an earlier read, the
-- same way ClearTransactionTags does it, so the statement is safe on its own.
DELETE FROM transaction_tags tt
USING transactions tr, accounts a, account_access v
WHERE tt.tag_id = sqlc.arg('tag_id')
  AND tt.transaction_id = ANY(sqlc.arg('transaction_ids')::uuid[])
  AND tr.id = tt.transaction_id
  AND a.id = tr.account_id
  AND v.account_id = a.id
  AND v.household_id = sqlc.arg('household_id')
  AND (v.user_id = sqlc.arg('user_id') OR v.is_shared);

-- name: ListTagTransactions :many
-- Every visible transaction carrying one tag, newest first — the tag detail
-- list, and the "show me the trip" drill-down.
--
-- Unlike the reporting queries this does NOT drop income, transfers or pending
-- rows: it is a ledger view, and a refund that was part of the trip belongs in
-- the list of what the trip involved. Excluded rows stay hidden, matching the
-- default ledger.
SELECT
    tr.id,
    tr.date,
    tr.name,
    COALESCE(me.canonical_name, tr.merchant_name, tr.name) AS merchant,
    tr.amount,
    tr.currency,
    tr.category_id,
    a.name AS account_name
FROM transaction_tags tt
JOIN transactions tr  ON tr.id = tt.transaction_id
JOIN accounts a       ON a.id = tr.account_id
JOIN account_access v ON v.account_id = a.id
LEFT JOIN merchant_aliases ma
       ON ma.household_id = sqlc.arg('household_id')
      AND ma.merchant_key = tr.merchant_key
      AND ma.source <> 'suggested'
LEFT JOIN merchant_entities me ON me.id = ma.entity_id
WHERE tt.tag_id = sqlc.arg('tag_id')
  AND v.household_id = sqlc.arg('household_id')
  AND (v.user_id = sqlc.arg('user_id') OR v.is_shared)
  AND a.is_active
  AND NOT tr.excluded_from_reports
ORDER BY tr.date DESC, tr.created_at DESC
LIMIT sqlc.arg('lim');
