-- Rule queries: the user-editable IF-THEN engine's storage.
--
-- Two scoping rules run through this file, and they are different rules for the
-- same reason the tag queries' two are:
--
--   * A RULE (and its triggers and actions) is HOUSEHOLD data. Every read and
--     write is guarded by household_id = $1, so a rule id from another
--     household matches nothing and comes back as a 404 rather than as an edit.
--   * A TRANSACTION a rule acts on is per-member data, scoped by the same
--     account_access predicate as every other transaction read. A rule is not
--     an escape hatch: it must never become a way to read, count, or learn the
--     existence of a charge on the other member's private account.
--
-- The second scope is OPTIONAL on the candidate reads, and the two callers are
-- deliberately different:
--
--   viewer_user_id SET   — a member asked for this (test a rule, run it now).
--     Only rows that member can already see are considered, so the match count
--     and the preview cannot describe anything they were not allowed to read.
--
--   viewer_user_id NULL  — the system asked for this (a Plaid sync just landed
--     rows, nobody is on the other end of the request). The whole household is
--     in scope, exactly as internal/categorize already works: automation has to
--     file the household's charges whether or not the member who wrote the rule
--     could have read them, and nothing is returned to a user here.
--
-- Nothing in this file decides WHETHER a rule fires. Matching lives entirely in
-- internal/rules, in Go, against the snapshot the candidate reads return —
-- there is no half of the predicate hiding in SQL for someone to change on its
-- own later.

-- name: ListRules :many
-- This household's rules, in the order the engine runs them: higher priority
-- first, oldest first inside a priority so the order is stable and explicable.
--
-- Also serves the single-rule read via the optional `id` narg. There is no
-- separate GetRule on purpose — a rule read one way and the same rule read
-- another way should not be able to drift.
--
-- Inactive rules are INCLUDED. This is the management list as well as the
-- engine's load, and a rule the user switched off must still render (that is
-- the whole point of `active` being a flag rather than a delete). The engine
-- filters on active itself.
SELECT id, household_id, name, description, active, priority, created_at, updated_at
FROM rules
WHERE household_id = sqlc.arg('household_id')
  AND (sqlc.narg('id')::uuid IS NULL OR id = sqlc.narg('id')::uuid)
ORDER BY priority DESC, created_at;

-- name: ListRuleTriggers :many
-- Every trigger belonging to this household's rules, in one round trip.
--
-- The join to `rules` is the authorisation, not a convenience: rule_triggers
-- carries no household_id of its own, so without it a rule id from anywhere
-- would return its conditions. It is also what keeps the rules page off the N+1
-- path — twenty rules cost one query for their conditions, not twenty.
SELECT t.id, t.rule_id, t.trigger_type, t.value, t.invert, t.position
FROM rule_triggers t
JOIN rules r ON r.id = t.rule_id
WHERE r.household_id = sqlc.arg('household_id')
  AND (sqlc.narg('rule_id')::uuid IS NULL OR t.rule_id = sqlc.narg('rule_id')::uuid)
ORDER BY t.rule_id, t.position, t.created_at;

-- name: ListRuleActions :many
-- Every action belonging to this household's rules, same shape and same
-- reasoning as ListRuleTriggers. Ordered by position because for actions the
-- order is load-bearing: "set notes" then "append notes" is not the reverse.
SELECT a.id, a.rule_id, a.action_type, a.value, a.stop_on_fail, a.position
FROM rule_actions a
JOIN rules r ON r.id = a.rule_id
WHERE r.household_id = sqlc.arg('household_id')
  AND (sqlc.narg('rule_id')::uuid IS NULL OR a.rule_id = sqlc.narg('rule_id')::uuid)
ORDER BY a.rule_id, a.position, a.created_at;

-- name: CreateRule :one
-- Returns the id only: the response is re-read through ListRules, so a rule
-- created a second ago is rendered by the same SQL as one that has existed for
-- a year.
INSERT INTO rules (household_id, name, description, active, priority)
VALUES (sqlc.arg('household_id'), sqlc.arg('name'), sqlc.narg('description'),
        sqlc.arg('active'), sqlc.arg('priority'))
RETURNING id;

-- name: UpdateRule :one
-- The household_id guard IS the authorisation: an id from another household
-- matches no rows, the handler reads pgx.ErrNoRows and returns 404 rather than
-- editing it.
UPDATE rules
SET name        = sqlc.arg('name'),
    description = sqlc.narg('description'),
    active      = sqlc.arg('active'),
    priority    = sqlc.arg('priority'),
    updated_at  = now()
WHERE id = sqlc.arg('id') AND household_id = sqlc.arg('household_id')
RETURNING id;

-- name: DeleteRule :execrows
-- Triggers and actions cascade. :execrows returns 0 for another household's id,
-- which the handler maps to 404.
DELETE FROM rules
WHERE id = sqlc.arg('id') AND household_id = sqlc.arg('household_id');

-- name: DeleteRuleTriggers :exec
-- The first half of "the rule's conditions are now exactly these". The editor
-- is a list of rows the user confirms, not a stream of deltas, so an update
-- replaces the set rather than reconciling it — and it happens inside the same
-- transaction as the re-insert, so a rule is never briefly condition-less (a
-- rule with no triggers would match everything).
DELETE FROM rule_triggers
WHERE rule_id = sqlc.arg('rule_id');

-- name: DeleteRuleActions :exec
DELETE FROM rule_actions
WHERE rule_id = sqlc.arg('rule_id');

-- name: CreateRuleTrigger :exec
-- The caller has already proved the rule is this household's (via CreateRule or
-- UpdateRule in the same transaction), and internal/rules has already validated
-- the type and value.
INSERT INTO rule_triggers (rule_id, trigger_type, value, invert, position)
VALUES (sqlc.arg('rule_id'), sqlc.arg('trigger_type'), sqlc.arg('value'),
        sqlc.arg('invert'), sqlc.arg('position'));

-- name: CreateRuleAction :exec
INSERT INTO rule_actions (rule_id, action_type, value, stop_on_fail, position)
VALUES (sqlc.arg('rule_id'), sqlc.arg('action_type'), sqlc.arg('value'),
        sqlc.arg('stop_on_fail'), sqlc.arg('position'));

-- name: ListRuleCandidates :many
-- A page of transactions for the engine to evaluate, with everything a trigger
-- can ask about and everything an action needs to decide it would change
-- nothing.
--
-- The snapshot is complete on purpose. `tag_ids` is here so "add tag" can tell
-- an already-tagged row from a new one without a query per transaction, and
-- `has_attachments` so the attachment trigger does not become a second round
-- trip per row. That completeness is what makes idempotence cheap: the engine
-- decides "this would change nothing" from the row in front of it.
--
-- Keyset pagination on id rather than OFFSET. The engine WRITES as it walks, so
-- an offset would skip or repeat rows as the underlying order shifted; ordering
-- by the primary key and asking for "the next ids after this one" is stable
-- under concurrent mutation of the rows already passed.
--
-- viewer_user_id NULL means the system is asking (a sync) and the whole
-- household is in scope; a non-NULL value narrows to what that member can see.
-- See the header for why those two callers differ.
SELECT
    t.id,
    t.account_id,
    t.date,
    t.name,
    t.merchant_name,
    t.amount,
    t.currency,
    t.category_id,
    t.category_source,
    t.notes,
    a.name AS account_name,
    COALESCE(
        ARRAY(SELECT tt.tag_id FROM transaction_tags tt WHERE tt.transaction_id = t.id),
        '{}'
    )::uuid[] AS tag_ids,
    EXISTS(
        SELECT 1 FROM document_links dl WHERE dl.transaction_id = t.id
    )::bool AS has_attachments
FROM transactions t
JOIN accounts a       ON a.id = t.account_id
JOIN account_access v ON v.account_id = a.id
WHERE v.household_id = sqlc.arg('household_id')
  AND (sqlc.narg('viewer_user_id')::uuid IS NULL
       OR v.user_id = sqlc.narg('viewer_user_id')::uuid
       OR v.is_shared)
  AND (sqlc.narg('after_id')::uuid IS NULL OR t.id > sqlc.narg('after_id')::uuid)
ORDER BY t.id
LIMIT sqlc.arg('lim');

-- name: GetRuleCandidate :one
-- One transaction in the same shape as ListRuleCandidates, for the hook that
-- fires when a single row is created or edited.
--
-- Same columns, deliberately duplicated rather than factored into a view: the
-- two statements must return the same snapshot, and a view would put that
-- guarantee in a third place that a migration could change without either
-- caller noticing. The test asserts both shapes feed the same engine.
SELECT
    t.id,
    t.account_id,
    t.date,
    t.name,
    t.merchant_name,
    t.amount,
    t.currency,
    t.category_id,
    t.category_source,
    t.notes,
    a.name AS account_name,
    COALESCE(
        ARRAY(SELECT tt.tag_id FROM transaction_tags tt WHERE tt.transaction_id = t.id),
        '{}'
    )::uuid[] AS tag_ids,
    EXISTS(
        SELECT 1 FROM document_links dl WHERE dl.transaction_id = t.id
    )::bool AS has_attachments
FROM transactions t
JOIN accounts a       ON a.id = t.account_id
JOIN account_access v ON v.account_id = a.id
WHERE t.id = sqlc.arg('id')
  AND v.household_id = sqlc.arg('household_id')
  AND (sqlc.narg('viewer_user_id')::uuid IS NULL
       OR v.user_id = sqlc.narg('viewer_user_id')::uuid
       OR v.is_shared);

-- name: ApplyRuleCategory :execrows
-- The set-category action.
--
-- THE STICKY-MANUAL INVARIANT LIVES HERE, in the WHERE clause, not in Go. A row
-- the user filed by hand is the one thing no automation may overwrite — the
-- merchant cache already follows this rule, and a rule engine that did not
-- would silently undo the corrections a user makes one row at a time.
-- Expressing it as a predicate means the guarantee holds even if a future
-- caller forgets to check first: the UPDATE simply matches nothing and returns
-- 0 rows, which the engine reports as a REFUSED action.
--
-- The category must be one this household can use — its own, or a system
-- default (household_id IS NULL) — which is the same visibility ListCategories
-- offers. A category id from another household matches nothing.
--
-- category_source becomes 'rule', which is a value the schema has allowed since
-- 00001; it now has a second producer. Both mean the same thing to every reader
-- ("a rule decided this"), which is why they share it rather than inventing a
-- 'user_rule' the reports would have to learn.
UPDATE transactions t
SET category_id     = sqlc.arg('category_id'),
    category_source = 'rule',
    updated_at      = now()
FROM accounts a, account_access v, categories c
WHERE t.id = sqlc.arg('id')
  AND a.id = t.account_id
  AND v.account_id = a.id
  AND v.household_id = sqlc.arg('household_id')
  AND c.id = sqlc.arg('category_id')
  AND (c.household_id IS NULL OR c.household_id = sqlc.arg('household_id'))
  AND COALESCE(t.category_source, '') <> 'manual';

-- name: SetRuleNotes :execrows
-- The set-notes and append-notes actions. Both write the FINAL text: the engine
-- computes the appended string from the snapshot it already holds, so there is
-- no read-modify-write race hiding in a SQL-side concatenation, and no way for
-- a retried apply to append twice.
--
-- The household guard is defence in depth — the id came from a candidate read
-- that was already scoped — and costs one join.
UPDATE transactions t
SET notes      = sqlc.narg('notes'),
    updated_at = now()
FROM accounts a, account_access v
WHERE t.id = sqlc.arg('id')
  AND a.id = t.account_id
  AND v.account_id = a.id
  AND v.household_id = sqlc.arg('household_id');

-- name: AddRuleTag :execrows
-- The add-tag action.
--
-- Both scopes are in the SELECT, so this cannot reach across a boundary even
-- with two valid-looking ids: the tag must be this household's, and so must the
-- transaction. A miss on either inserts nothing and returns 0 rows, which the
-- engine reports as a refused action rather than as success.
--
-- ON CONFLICT DO NOTHING is the idempotence: the composite primary key already
-- forbids the duplicate, and this means a rule re-run over a row it has already
-- tagged is a no-op instead of an error. The engine ALSO checks the snapshot's
-- tag_ids first, so the common case never reaches the database at all — this is
-- the backstop for the race, not the mechanism.
INSERT INTO transaction_tags (transaction_id, tag_id)
SELECT t.id, tg.id
FROM transactions t
JOIN accounts a       ON a.id = t.account_id
JOIN account_access v ON v.account_id = a.id
JOIN tags tg          ON tg.id = sqlc.arg('tag_id')
WHERE t.id = sqlc.arg('transaction_id')
  AND v.household_id = sqlc.arg('household_id')
  AND tg.household_id = sqlc.arg('household_id')
ON CONFLICT DO NOTHING;

-- name: CountRuleTargets :one
-- Whether the ids a rule names still exist and still belong to this household —
-- the validation behind "a trigger or action must not point at nothing".
--
-- One statement for all three kinds rather than three round trips per rule
-- save. Exactly one of the three args is non-NULL per call; the others
-- short-circuit to 0.
SELECT (
    COALESCE((SELECT COUNT(*) FROM categories c
              WHERE c.id = sqlc.narg('category_id')::uuid
                AND (c.household_id IS NULL
                     OR c.household_id = sqlc.arg('household_id'))), 0)
  + COALESCE((SELECT COUNT(*) FROM tags tg
              WHERE tg.id = sqlc.narg('tag_id')::uuid
                AND tg.household_id = sqlc.arg('household_id')), 0)
  + COALESCE((SELECT COUNT(*) FROM account_access v
              WHERE v.account_id = sqlc.narg('account_id')::uuid
                AND v.household_id = sqlc.arg('household_id')), 0)
)::bigint AS found;
