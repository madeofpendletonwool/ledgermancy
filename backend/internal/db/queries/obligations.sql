-- Recurring obligations: the bill calendar's reads and writes.
--
-- Two rules hold everywhere in this file:
--
--   * Occurrences are DERIVED, never stored. anchor_date plus the (count, unit)
--     cadence generates them on demand, so there is no next-due cache to go
--     stale. All of that arithmetic happens here, in Postgres, because Postgres
--     interval addition clamps month ends the way a person would read them
--     (2025-01-31 + interval '1 month' = 2025-02-28) and Go's time.AddDate does
--     not (it normalises to 2025-03-03). One place, one behaviour.
--   * Visibility is scoped like every other household read: household-owned
--     rows (user_id IS NULL — what the detector promotes), the caller's own
--     rows, and other members' rows only when shared.

-- name: CreateObligation :one
INSERT INTO recurring_obligations (
    household_id, user_id, is_shared, label, amount, category_id, account_id,
    interval_count, interval_unit, anchor_date, end_date, source, merchant_key
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 'manual', NULL)
RETURNING *;

-- name: ListObligations :many
-- Every obligation the caller can see, active first. The occurrence expansion
-- lives in ListUpcomingObligations; this is the row-level list the management UI
-- edits.
--
-- merchant_key needs no canonicalisation on the way out, unlike most reads of a
-- stored key. A detected row's key comes from GetRecurringMerchants and is
-- already resolved, and a later merge does not strand it: the merge changes the
-- resolved key, so the next promotion pass writes a row under the new key and
-- DeactivateUndetectedObligations retires the old one. The key a live detected
-- row carries is therefore always current, and addresses the merchant detail
-- view directly.
SELECT * FROM recurring_obligations
WHERE household_id = $1
  AND (user_id IS NULL OR user_id = $2 OR is_shared)
ORDER BY is_active DESC, label;

-- name: GetObligation :one
SELECT * FROM recurring_obligations
WHERE id = $1 AND household_id = $2
  AND (user_id IS NULL OR user_id = $3 OR is_shared);

-- name: UpdateObligation :one
-- Edits from the UI. user_edited is stamped unconditionally: once a human has
-- touched a row, the promotion pass must leave it alone forever, whether the
-- edit changed a detected row's cadence or only its label.
UPDATE recurring_obligations
SET label          = $4,
    amount         = $5,
    category_id    = $6,
    account_id     = $7,
    interval_count = $8,
    interval_unit  = $9,
    anchor_date    = $10,
    end_date       = $11,
    is_shared      = $12,
    is_active      = $13,
    user_edited    = TRUE,
    updated_at     = now()
WHERE id = $1 AND household_id = $2
  AND (user_id IS NULL OR user_id = $3 OR is_shared)
RETURNING *;

-- name: SetObligationActive :one
-- Deactivate rather than delete: a detected row that is hard-deleted comes
-- straight back on the next promotion pass, and a deactivated one keeps the
-- user's decision. user_edited is stamped for the same reason as UpdateObligation.
UPDATE recurring_obligations
SET is_active = $4, user_edited = TRUE, updated_at = now()
WHERE id = $1 AND household_id = $2
  AND (user_id IS NULL OR user_id = $3 OR is_shared)
RETURNING *;

-- name: DeleteObligation :exec
-- Hard delete, for manual rows only. A detected row would be re-promoted, so the
-- API routes those to SetObligationActive instead.
DELETE FROM recurring_obligations
WHERE id = $1 AND household_id = $2 AND source = 'manual'
  AND (user_id = $3 OR is_shared);

-- name: ListUpcomingObligations :many
-- One row per (obligation × occurrence) falling inside [$3, $4]. This single
-- query backs the calendar, the list view, the balance projection, the
-- safe-to-spend split and the upcoming-bill insight — there is deliberately no
-- second variant to disagree with.
--
-- How the expansion works, and why it is shaped this way:
--
--   n_max bounds how many steps could possibly land on or before $4, computed
--   per unit in whole units so it is exact and cheap. A window that ends before
--   anchor_date yields a negative bound, GREATEST clamps it to 0, and the WHERE
--   drops that single candidate — so a window entirely before the first
--   occurrence correctly returns nothing.
--
--   Each occurrence is anchor_date + n whole periods, NOT the previous
--   occurrence plus one period. That distinction is the whole month-end story:
--   generate_series over timestamps steps by repeated addition, so a monthly
--   obligation anchored on the 31st would walk 01-31, 02-28, 03-28, 04-28 and
--   drift permanently off the 31st. Multiplying the interval instead gives
--   01-31, 02-28, 03-31, 04-30 — clamped, never drifting.
WITH visible AS (
    SELECT *
    FROM recurring_obligations
    WHERE household_id = $1
      AND (user_id IS NULL OR user_id = $2 OR is_shared)
      AND is_active
),
bounded AS (
    SELECT
        visible.*,
        CASE interval_unit
            WHEN 'day'   THEN ($4::date - anchor_date) / interval_count
            WHEN 'week'  THEN ($4::date - anchor_date) / (7 * interval_count)
            WHEN 'month' THEN (12 * (EXTRACT(YEAR  FROM $4::date)::int - EXTRACT(YEAR  FROM anchor_date)::int)
                                  + (EXTRACT(MONTH FROM $4::date)::int - EXTRACT(MONTH FROM anchor_date)::int))
                              / interval_count
            WHEN 'year'  THEN (EXTRACT(YEAR FROM $4::date)::int - EXTRACT(YEAR FROM anchor_date)::int)
                              / interval_count
        END AS n_max
    FROM visible
)
SELECT
    b.id           AS obligation_id,
    b.label,
    b.amount,
    b.category_id,
    b.account_id,
    b.interval_count,
    b.interval_unit,
    b.source,
    b.merchant_key,
    d.due_date::date AS due_date
FROM bounded b
CROSS JOIN LATERAL generate_series(0, GREATEST(b.n_max, 0)) AS g(n)
CROSS JOIN LATERAL (
    SELECT b.anchor_date + make_interval(
        days   => CASE b.interval_unit
                      WHEN 'day'  THEN g.n * b.interval_count
                      WHEN 'week' THEN g.n * b.interval_count * 7
                      ELSE 0 END,
        months => CASE b.interval_unit WHEN 'month' THEN g.n * b.interval_count ELSE 0 END,
        years  => CASE b.interval_unit WHEN 'year'  THEN g.n * b.interval_count ELSE 0 END
    ) AS due_date
) d
WHERE d.due_date >= $3::date
  AND d.due_date <= $4::date
  AND (b.end_date IS NULL OR d.due_date <= b.end_date)
ORDER BY due_date, b.label;

-- name: ListProjectionAccounts :many
-- The accounts a projected balance is meaningful for: depository only. Running
-- the projection over a credit card would subtract that card's own bills from
-- the balance those bills are *made of*, which is double-counting dressed as a
-- forecast. Same visibility scoping as ListVisibleAccounts.
SELECT
    a.id,
    a.name,
    a.mask,
    a.subtype,
    COALESCE(a.current_balance, 0)::numeric AS current_balance,
    i.institution_name
FROM accounts a
JOIN plaid_items i ON i.id = a.plaid_item_id
JOIN users u       ON u.id = i.user_id
WHERE u.household_id = $1
  AND (i.user_id = $2 OR i.is_shared)
  AND a.is_active
  AND a.type = 'depository'
ORDER BY i.institution_name, a.name;

-- name: UpsertDetectedObligation :one
-- Promotion from the recurring detector. Idempotent by construction: the
-- partial unique index on (household_id, merchant_key) WHERE source='detected'
-- means a re-run updates the same row rather than adding a second one.
--
-- The WHERE on the DO UPDATE is the load-bearing part. Without it the first pass
-- after a user fixes a wrong cadence would silently put the wrong one back.
INSERT INTO recurring_obligations (
    household_id, user_id, is_shared, label, amount, category_id,
    interval_count, interval_unit, anchor_date, source, merchant_key
) VALUES ($1, NULL, TRUE, $2, $3, $4, $5, $6, $7, 'detected', $8)
ON CONFLICT (household_id, merchant_key) WHERE source = 'detected'
DO UPDATE SET
    label          = EXCLUDED.label,
    amount         = EXCLUDED.amount,
    category_id    = EXCLUDED.category_id,
    interval_count = EXCLUDED.interval_count,
    interval_unit  = EXCLUDED.interval_unit,
    anchor_date    = EXCLUDED.anchor_date,
    updated_at     = now()
WHERE NOT recurring_obligations.user_edited
RETURNING *;

-- name: DeactivateSuppressedObligations :execrows
-- A merchant the household marked "not recurring" must never show up as a bill.
-- GetRecurringMerchants already excludes suppressed merchants, so promotion will
-- not re-create these; this retires rows promoted before the suppression.
--
-- Both keys are resolved (merchants.sql) rather than compared literally, so an
-- obligation promoted under a raw descriptor is still retired by a suppression
-- recorded against the merged merchant, and vice versa.
UPDATE recurring_obligations o
SET is_active = FALSE, updated_at = now()
FROM recurring_overrides ro
WHERE o.household_id = $1
  AND o.source = 'detected'
  AND o.is_active
  AND ro.household_id = o.household_id
  AND COALESCE(
        (SELECT a1.entity_id::text FROM merchant_aliases a1
          WHERE a1.household_id = ro.household_id
            AND a1.merchant_key = ro.merchant_key
            AND a1.source <> 'suggested'),
        ro.merchant_key
      ) = COALESCE(
        (SELECT a2.entity_id::text FROM merchant_aliases a2
          WHERE a2.household_id = o.household_id
            AND a2.merchant_key = o.merchant_key
            AND a2.source <> 'suggested'),
        o.merchant_key
      );

-- name: DeactivateUndetectedObligations :execrows
-- Retire detected bills the detector no longer returns. Promotion is an upsert
-- and nothing else ever cleared a row, so without this a detected obligation
-- lives forever. Two ways that goes wrong, and the second is the expensive one:
--
--   A merchant that simply stops recurring leaves a tombstone — a bill on the
--   calendar predicting money that will never move.
--
--   Worse, merging descriptors into an entity CHANGES the resolved merchant key
--   the detector reports. Promotion then writes a brand new row under the entity
--   key while the row under the raw descriptor stays active, so one bill is
--   counted twice — on the calendar, in the balance projection, and in the
--   safe-to-spend split.
--
-- @detected_keys is the resolved key of every merchant promoted this pass, so
-- "no longer detected" covers both cases with one comparison. Passing an empty
-- array is meaningful and must retire everything: `<> ALL` over zero rows is
-- TRUE, which is the behaviour wanted (the detector found nothing, so nothing
-- detected should be live).
--
-- A user-edited row is left alone, matching UpsertDetectedObligation: once
-- someone has corrected a bill by hand the detector no longer owns it, and
-- silently deleting a correction is worse than carrying a stale one.
UPDATE recurring_obligations
SET is_active = FALSE, updated_at = now()
WHERE household_id = @household_id
  AND source = 'detected'
  AND is_active
  AND NOT user_edited
  AND merchant_key <> ALL(@detected_keys::text[]);

-- name: GetMerchantDominantCategories :many
-- The category each recurring merchant is usually filed under, so a promoted
-- obligation lands in the right category. That matters beyond cosmetics: the
-- safe-to-spend split uses an obligation's category to decide whether it
-- REPLACES that category's trailing typical cost or is already inside a
-- discretionary budget envelope. Without a category every promoted bill would
-- have to be assumed uncovered, and fixed costs would be counted twice.
--
-- Same spend definition, visibility scoping, and resolved merchant grouping as
-- GetRecurringMerchants — the caller looks these rows up by the key that query
-- returned, so the two must agree on the key space.
SELECT
    COALESCE(ma.entity_id::text, t.merchant_key)::text   AS merchant_key,
    (mode() WITHIN GROUP (ORDER BY t.category_id))::uuid AS category_id
FROM transactions t
JOIN accounts a    ON a.id = t.account_id
JOIN plaid_items i ON i.id = a.plaid_item_id
JOIN users u       ON u.id = i.user_id
LEFT JOIN merchant_aliases ma
       ON ma.household_id = $1
      AND ma.merchant_key = t.merchant_key
      AND ma.source <> 'suggested'
WHERE u.household_id = $1
  AND (i.user_id = $2 OR i.is_shared)
  AND a.is_active
  AND NOT t.excluded_from_reports
  AND NOT t.pending
  AND t.merchant_key IS NOT NULL
  AND t.category_id IS NOT NULL
  AND t.amount > 0
  AND t.date >= $3
GROUP BY COALESCE(ma.entity_id::text, t.merchant_key);
