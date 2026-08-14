-- Transaction link queries.
--
-- Two scoping rules, and they differ for the same reason the tag queries' two
-- rules do:
--
--   * A LINK TYPE is reference data or household data. The three seeded types
--     (household_id NULL) are readable by everyone and writable by no one; a
--     household's own types are guarded by household_id = $1, so an id from
--     another household matches nothing.
--   * A LINK is scoped by BOTH transactions it joins, through the same
--     account_access predicate as every other transaction read. Not one end —
--     both. A link is the only object in the schema that names two transactions
--     at once, so it is the only one where scoping the row the caller asked
--     about would still hand back a row they were never allowed to see.
--
-- The practical consequence, stated plainly because it is easy to get wrong
-- later: if Bob links a charge on his private account to a shared charge, Alice
-- reading the shared charge's links sees NOTHING. Not a redacted entry, not a
-- count — the link is absent, because "there is a link to something you cannot
-- see" is itself a fact about Bob's private account.
--
-- Nothing in this file writes to `transactions`. Links are annotations; the
-- only thing they change is what a report is asked to compute (see
-- `net_refunds` in reports.sql).

-- name: ListLinkTypes :many
-- The vocabulary this household can use: the seeded types plus its own.
--
-- Also serves the single-type read via the optional `id` narg, so a type is
-- rendered by the same SQL whether it was created a second ago or shipped with
-- the app. A type id from another household is filtered by the household guard
-- rather than by the narg, so it comes back empty — indistinguishable from an
-- id that never existed, which the handler turns into a 404.
--
-- Ordered system-first then by name, so the pickers put the three well-defined
-- relationships above whatever the household has invented.
SELECT
    id,
    household_id,
    slug,
    name,
    outward,
    inward,
    nets_spend,
    -- Cast so sqlc types it `bool` rather than `interface{}`; a bare IS NULL
    -- test has no column for it to infer from.
    (household_id IS NULL)::bool AS is_system,
    created_at
FROM link_types
WHERE (household_id IS NULL OR household_id = sqlc.arg('household_id'))
  AND (sqlc.narg('id')::uuid IS NULL OR id = sqlc.narg('id')::uuid)
ORDER BY (household_id IS NOT NULL), lower(name);

-- name: CountLinkTypesBySlug :one
-- Whether a slug is already taken for this household, counting the system rows
-- as taken. Feeds the -2, -3, … disambiguation when a custom type's name
-- slugifies onto one that exists ("Refund?" and "Refund!" are two names and one
-- slug). Same shape as uniqueCategorySlug's loop.
SELECT COUNT(*)::bigint
FROM link_types
WHERE slug = sqlc.arg('slug')
  AND (household_id IS NULL OR household_id = sqlc.arg('household_id'));

-- name: CountSystemLinkTypesByName :one
-- Whether a name is one of the three shipped relationships, case-folded.
--
-- This is a check no index can do. link_types_household_name_key folds case, but
-- a system row's household_id is NULL and a household's is not, so the two are
-- different keys and Postgres is right to let both exist — a unique index cannot
-- express "unique within the union of these two namespaces". Doing it in the
-- handler is a race in principle; in practice the loser of that race gets a
-- second row in a picker, and the alternative was a trigger to prevent a
-- duplicate label.
--
-- It matters because the picker would otherwise list two entries called
-- "Refund", one of which nets spending and one of which never can. Someone picks
-- the wrong one and their refunds silently stop netting, with nothing on screen
-- to explain why.
SELECT COUNT(*)::bigint
FROM link_types
WHERE household_id IS NULL AND lower(name) = lower(sqlc.arg('name'));

-- name: CreateLinkType :one
-- Returns the id only; the response is re-read through ListLinkTypes so create
-- and list cannot drift.
--
-- nets_spend is not a parameter. A household-defined type never nets — see the
-- column comment in migration 00067 — and leaving it off the INSERT is what
-- makes that structural rather than a rule the handler has to remember.
INSERT INTO link_types (household_id, slug, name, outward, inward)
VALUES (sqlc.arg('household_id'), sqlc.arg('slug'), sqlc.arg('name'),
        sqlc.arg('outward'), sqlc.arg('inward'))
RETURNING id;

-- name: UpdateLinkType :one
-- The household_id guard IS the authorisation, and it is also what makes the
-- three system types immutable: their household_id is NULL, and NULL never
-- equals the caller's id, so they match no rows and the handler returns 404.
-- There is no admin path that edits them either — a seeded relationship means
-- one thing in every deployment, and `refund` in particular is the type the
-- netting query keys on.
--
-- The slug is deliberately absent: it is derived once at creation and then
-- stable, so renaming a type does not silently invalidate anything holding it.
UPDATE link_types
SET name       = sqlc.arg('name'),
    outward    = sqlc.arg('outward'),
    inward     = sqlc.arg('inward'),
    updated_at = now()
WHERE id = sqlc.arg('id') AND household_id = sqlc.arg('household_id')
RETURNING id;

-- name: DeleteLinkType :execrows
-- Custom types only (the household_id guard, again). transaction_links cascades:
-- the edges go, and both transactions are left exactly as they were — which is
-- the non-destructive outcome the whole feature rests on. :execrows returns 0
-- for a system type or another household's id, which the handler maps to 404.
DELETE FROM link_types
WHERE id = sqlc.arg('id') AND household_id = sqlc.arg('household_id');

-- name: ListTransactionLinks :many
-- Every link on one transaction, from BOTH ends, phrased from that
-- transaction's point of view.
--
-- One row in transaction_links is one edge, and the same edge is read
-- differently depending on which end you stand at: the source "refunds", the
-- target "is refunded by". `anchor` pins the end the caller asked about, the
-- UNION ALL walks out of it in both directions, and `relation` picks the verb
-- to match. That is why there is no reverse row in the table — the reverse
-- reading is computed here, so the two can never disagree.
--
-- Both ends are scoped. `anchor` scopes the transaction named in the URL;
-- the join to account_access at the bottom scopes the FAR end. A link whose far
-- end the caller cannot see is dropped entirely rather than redacted: see the
-- file header.
WITH anchor AS (
    SELECT t.id
    FROM transactions t
    JOIN accounts a       ON a.id = t.account_id
    JOIN account_access v ON v.account_id = a.id
    WHERE t.id = sqlc.arg('transaction_id')
      AND v.household_id = sqlc.arg('household_id')
      AND (v.user_id = sqlc.arg('user_id') OR v.is_shared)
), edges AS (
    SELECT l.id, l.link_type_id, l.created_at,
           TRUE                     AS is_outward,
           l.target_transaction_id  AS other_id
    FROM transaction_links l
    JOIN anchor an ON an.id = l.source_transaction_id
    UNION ALL
    SELECT l.id, l.link_type_id, l.created_at,
           FALSE,
           l.source_transaction_id
    FROM transaction_links l
    JOIN anchor an ON an.id = l.target_transaction_id
)
SELECT
    e.id,
    e.is_outward,
    e.created_at,
    lt.id         AS link_type_id,
    lt.slug       AS link_type_slug,
    lt.name       AS link_type_name,
    lt.nets_spend,
    -- The verb as this end reads it. The one place the two phrasings on a link
    -- type are chosen between, so a panel never has to know which column to use.
    (CASE WHEN e.is_outward THEN lt.outward ELSE lt.inward END)::text AS relation,
    tr.id         AS other_transaction_id,
    tr.date       AS other_date,
    tr.name       AS other_name,
    COALESCE(me.canonical_name, tr.merchant_name, tr.name) AS other_merchant,
    tr.amount     AS other_amount,
    tr.currency   AS other_currency,
    tr.category_id AS other_category_id,
    a.name        AS other_account_name
FROM edges e
JOIN link_types lt    ON lt.id = e.link_type_id
JOIN transactions tr  ON tr.id = e.other_id
JOIN accounts a       ON a.id = tr.account_id
JOIN account_access v ON v.account_id = a.id
LEFT JOIN merchant_aliases ma
       ON ma.household_id = sqlc.arg('household_id')
      AND ma.merchant_key = tr.merchant_key
      AND ma.source <> 'suggested'
LEFT JOIN merchant_entities me ON me.id = ma.entity_id
WHERE v.household_id = sqlc.arg('household_id')
  AND (v.user_id = sqlc.arg('user_id') OR v.is_shared)
ORDER BY tr.date DESC, e.created_at DESC;

-- name: CreateTransactionLink :one
-- Links two transactions. Every guard is in the SELECT, so two valid-looking
-- ids cannot reach across a boundary: the source must be visible to the caller,
-- the target must be visible to the caller, and the type must be a system one
-- or the caller's household's. A miss on any of the three inserts nothing and
-- the statement returns no rows.
--
-- The handler resolves each of the three separately first, purely so it can say
-- WHICH one was wrong. This statement is the enforcement; that is the error
-- message.
--
-- Direction is the caller's, not this statement's: the handler decides which of
-- the two transactions is the source (see linkDirection). A refund link written
-- the wrong way round would net the charge against the credit.
--
-- The self-link case is caught by the CHECK constraint rather than here, and
-- the duplicate-pair case by transaction_links_pair_key — both are races a Go
-- pre-check cannot close.
INSERT INTO transaction_links (source_transaction_id, target_transaction_id, link_type_id)
SELECT src.id, tgt.id, lt.id
FROM transactions src
JOIN accounts sa       ON sa.id = src.account_id
JOIN account_access sv ON sv.account_id = sa.id
JOIN transactions tgt  ON tgt.id = sqlc.arg('target_transaction_id')
JOIN accounts ta       ON ta.id = tgt.account_id
JOIN account_access tv ON tv.account_id = ta.id
JOIN link_types lt     ON lt.id = sqlc.arg('link_type_id')
WHERE src.id = sqlc.arg('source_transaction_id')
  AND sv.household_id = sqlc.arg('household_id')
  AND (sv.user_id = sqlc.arg('user_id') OR sv.is_shared)
  AND tv.household_id = sqlc.arg('household_id')
  AND (tv.user_id = sqlc.arg('user_id') OR tv.is_shared)
  AND (lt.household_id IS NULL OR lt.household_id = sqlc.arg('household_id'))
RETURNING id;

-- name: DeleteTransactionLink :execrows
-- Removes one link, from either of its ends.
--
-- The caller must be able to see the transaction they are standing on — not
-- both ends. Visibility can narrow after a link is made (an account stops being
-- shared), and the alternative is a link that is now unremovable from the only
-- row that still shows it. Deleting it destroys nothing: both transactions
-- survive untouched, which is the property that makes the looser guard safe
-- here and would not make it safe on a write.
--
-- :execrows returns 0 when the link id is unknown, belongs to a different pair,
-- or the anchor is not visible — all of which the handler maps to 404.
DELETE FROM transaction_links l
USING transactions t, accounts a, account_access v
WHERE l.id = sqlc.arg('id')
  AND t.id = sqlc.arg('transaction_id')
  AND (l.source_transaction_id = t.id OR l.target_transaction_id = t.id)
  AND a.id = t.account_id
  AND v.account_id = a.id
  AND v.household_id = sqlc.arg('household_id')
  AND (v.user_id = sqlc.arg('user_id') OR v.is_shared);
