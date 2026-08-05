-- Canonical merchants: entities, the aliases that map raw merchant_keys onto
-- them, and the rejection memory that keeps the review queue from refilling
-- with merges the user already said no to.
--
-- THE RESOLUTION RULE, used verbatim by every report that groups by merchant:
--
--     LEFT JOIN merchant_aliases ma
--            ON ma.household_id = <household>
--           AND ma.merchant_key = t.merchant_key
--           AND ma.source <> 'suggested'
--     LEFT JOIN merchant_entities me ON me.id = ma.entity_id
--
--     resolved key  = COALESCE(ma.entity_id::text, t.merchant_key)
--     display name  = COALESCE(me.canonical_name, t.merchant_name, t.name)
--
-- Two properties make this safe to sprinkle across the reporting layer:
--
--   1. A household with no aliases resolves every key to itself and every name
--      the way it did before this feature existed. The feature is additive.
--   2. Resolution is idempotent. A merged merchant's resolved key is an entity
--      UUID rendered as text, and no alias is ever keyed by a UUID, so
--      resolve(resolve(k)) = resolve(k). That is what lets a stored
--      recurring_overrides row or a promoted obligation hold *either* a raw key
--      or a resolved one and still match — including rows written before a
--      merge happened.
--
-- The `source <> 'suggested'` predicate is load-bearing: suggested aliases are
-- pending review and must not move a single number in any report.

-- name: ListMerchantKeyStats :many
-- Every distinct merchant_key the household can see, with the evidence a person
-- needs to judge a merge (how often, how much, over what span, and what the
-- descriptor actually looked like) and its current alias state. Feeds both the
-- suggestion engine and the review UI.
--
-- Deliberately unfiltered by category or excluded_from_reports: this is a view
-- of the raw descriptor space, not a spending report. Pending rows are excluded
-- because a pending descriptor is often rewritten when it posts.
SELECT
    t.merchant_key::text                                                   AS merchant_key,
    COUNT(*)::bigint                                                       AS transaction_count,
    COALESCE(SUM(t.amount), 0)::numeric                                    AS total_amount,
    MIN(t.date)::date                                                      AS first_seen,
    MAX(t.date)::date                                                      AS last_seen,
    (array_agg(COALESCE(t.merchant_name, t.name) ORDER BY t.date DESC))[1]::text AS sample_name,
    ma.entity_id,
    COALESCE(ma.source, '')::text                                          AS alias_source
FROM transactions t
JOIN accounts a    ON a.id = t.account_id
JOIN account_access v ON v.account_id = a.id
LEFT JOIN merchant_aliases ma
       ON ma.household_id = @household_id
      AND ma.merchant_key = t.merchant_key
WHERE v.household_id = @household_id
  AND (v.user_id = @user_id OR v.is_shared)
  AND a.is_active
  AND NOT t.pending
  AND t.merchant_key IS NOT NULL
GROUP BY t.merchant_key, ma.entity_id, ma.source
ORDER BY COUNT(*) DESC, t.merchant_key;

-- name: ListMerchantAliasStats :many
-- Every alias row in the household, joined to its entity and to the same
-- evidence ListMerchantKeyStats reports. One query serves two surfaces: rows
-- whose entity has any 'suggested' alias are the review queue; the rest are the
-- confirmed merchant list. Grouping is done by the caller.
WITH stats AS (
    SELECT
        t.merchant_key,
        COUNT(*)::bigint                    AS transaction_count,
        COALESCE(SUM(t.amount), 0)::numeric AS total_amount,
        MIN(t.date)::date                   AS first_seen,
        MAX(t.date)::date                   AS last_seen,
        (array_agg(COALESCE(t.merchant_name, t.name) ORDER BY t.date DESC))[1] AS sample_name
    FROM transactions t
    JOIN accounts a    ON a.id = t.account_id
    JOIN account_access v ON v.account_id = a.id
    WHERE v.household_id = @household_id
      AND (v.user_id = @user_id OR v.is_shared)
      AND a.is_active
      AND NOT t.pending
      AND t.merchant_key IS NOT NULL
    GROUP BY t.merchant_key
)
SELECT
    e.id                                          AS entity_id,
    e.canonical_name,
    e.default_category_id,
    e.color,
    ma.merchant_key,
    ma.source,
    ma.confidence,
    COALESCE(s.transaction_count, 0)::bigint      AS transaction_count,
    COALESCE(s.total_amount, 0)::numeric          AS total_amount,
    s.first_seen,
    s.last_seen,
    COALESCE(s.sample_name, ma.merchant_key)::text AS sample_name
FROM merchant_aliases ma
JOIN merchant_entities e ON e.id = ma.entity_id
LEFT JOIN stats s ON s.merchant_key = ma.merchant_key
WHERE ma.household_id = @household_id
ORDER BY e.canonical_name, e.id, ma.source, ma.merchant_key;

-- name: GetMerchantAliasByKey :one
-- One alias by descriptor. Used by the split path, which has a key and needs the
-- merchant it currently belongs to.
SELECT ma.merchant_key, ma.entity_id, ma.source, e.canonical_name
FROM merchant_aliases ma
JOIN merchant_entities e ON e.id = ma.entity_id
WHERE ma.household_id = $1 AND ma.merchant_key = $2;

-- name: CreateMerchantEntity :one
INSERT INTO merchant_entities (household_id, canonical_name, default_category_id, color)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetMerchantEntity :one
SELECT * FROM merchant_entities WHERE id = $1 AND household_id = $2;

-- name: FindMerchantEntityByName :one
SELECT * FROM merchant_entities
WHERE household_id = @household_id AND lower(canonical_name) = lower(@name::text);

-- name: RenameMerchantEntity :one
UPDATE merchant_entities
SET canonical_name = $3, updated_at = now()
WHERE id = $1 AND household_id = $2
RETURNING *;

-- name: SetMerchantEntityDefaultCategory :exec
UPDATE merchant_entities
SET default_category_id = $3, updated_at = now()
WHERE id = $1 AND household_id = $2;

-- name: DeleteMerchantEntity :exec
DELETE FROM merchant_entities WHERE id = $1 AND household_id = $2;

-- name: DeleteEmptyMerchantEntities :exec
-- An entity with no aliases canonicalises nothing and is retired.
--
-- This used to retire entities with fewer than TWO aliases, on the reasoning that
-- a single-alias entity is just the raw key wearing a hat. That stopped being
-- true once the import path started normalising descriptors through
-- plaid.MerchantKey: a business whose every descriptor collapses to one key --
-- twenty-odd AMAZON MKTPL*<order id> rows becoming a single "amazon mktpl" --
-- legitimately has one alias and a name the user chose. Deleting it would drop
-- that name and send every report back through COALESCE to the raw descriptor,
-- so "Amazon Marketplace" would redisplay as "AMAZON MKTPL*3842D24Q3".
--
-- A one-alias entity is therefore a rename, which is real user intent, and only a
-- zero-alias one is genuinely empty.
DELETE FROM merchant_entities e
WHERE e.household_id = $1
  AND NOT EXISTS (SELECT 1 FROM merchant_aliases ma WHERE ma.entity_id = e.id);

-- name: UpsertMerchantAlias :one
-- Claims a merchant_key for an entity. The unique constraint on
-- (household_id, merchant_key) is what stops a key belonging to two entities;
-- this re-points it instead of failing, which is how confirming a suggestion
-- and moving a key between entities both work.
INSERT INTO merchant_aliases (household_id, entity_id, merchant_key, source, confidence)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (household_id, merchant_key) DO UPDATE SET
    entity_id  = EXCLUDED.entity_id,
    source     = EXCLUDED.source,
    confidence = EXCLUDED.confidence
RETURNING *;

-- name: PromoteSuggestedAlias :one
-- Confirming a suggestion. Scoped to source = 'suggested' so a confirm can never
-- rewrite an alias the user already established by hand.
UPDATE merchant_aliases
SET source = 'manual', confidence = NULL
WHERE household_id = $1 AND merchant_key = $2 AND source = 'suggested'
RETURNING *;

-- name: DeleteMerchantAlias :exec
DELETE FROM merchant_aliases WHERE household_id = $1 AND merchant_key = $2;

-- name: DeleteSuggestedAliasesForEntity :exec
DELETE FROM merchant_aliases
WHERE household_id = $1 AND entity_id = $2 AND source = 'suggested';

-- name: DeleteSuggestedAliases :exec
-- Clears a household's whole pending queue, so a suggestion pass rebuilds it from
-- scratch rather than adding to it.
--
-- Without this, a pass could only ever ADD, and a proposal made by an older
-- version of the matching rules would outlive the rules that produced it: the
-- fixes that stopped HOMEGOODS being read as THE HOME DEPOT would never reach the
-- proposal already sitting in the queue saying it was. Suggestions are inert by
-- construction — no report counts them until a person confirms one — so throwing
-- them away and re-deriving costs nothing but the pass itself.
--
-- Confirmed aliases are untouched: source <> 'suggested' is the line between a
-- guess and a decision, and only guesses are disposable.
DELETE FROM merchant_aliases
WHERE household_id = $1 AND source = 'suggested';

-- name: ListMerchantAliases :many
SELECT merchant_key, source, confidence
FROM merchant_aliases
WHERE household_id = $1 AND entity_id = $2
ORDER BY source, merchant_key;

-- name: ListSuggestedAliasKeys :many
SELECT merchant_key FROM merchant_aliases
WHERE household_id = $1 AND source = 'suggested'
ORDER BY merchant_key;

-- name: RecordMergeRejection :exec
-- Remembers that these two keys are NOT the same business. The engine consults
-- this before proposing anything, so a rejected pair is proposed exactly once.
-- Keys arrive pre-ordered (key_a < key_b) so a pair has one representation.
INSERT INTO merchant_merge_rejections (household_id, key_a, key_b)
VALUES ($1, $2, $3)
ON CONFLICT (household_id, key_a, key_b) DO NOTHING;

-- name: ListMergeRejections :many
SELECT key_a, key_b FROM merchant_merge_rejections WHERE household_id = $1;

-- name: ListMerchantCategoryMappingsForKeys :many
-- The cached category decisions for a set of merchant_keys, so a merge can
-- reconcile them. `source` is what decides the winner: a manual mapping outranks
-- everything, per the app's "a manual category is sticky" rule.
SELECT merchant_key, category_id, source, updated_at
FROM merchant_category_map
WHERE household_id = $1 AND merchant_key = ANY($2::text[]);
