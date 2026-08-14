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
-- amount_min/amount_max are the stated expected range (MAD-120), both NULL when
-- the member did not state one. amount stays the expected figure the projection
-- uses either way.
INSERT INTO recurring_obligations (
    household_id, user_id, is_shared, label, amount, category_id, account_id,
    interval_count, interval_unit, anchor_date, end_date, amount_min, amount_max,
    source, merchant_key
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, 'manual', NULL)
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
    amount_min     = $14,
    amount_max     = $15,
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

-- name: SetObligationRemind :one
-- Toggle the per-item reminders opt-out (MAD-85). user_edited is stamped so a
-- later promotion pass leaves the row alone, matching SetObligationActive: a
-- member's choice about reminders must survive re-detection.
UPDATE recurring_obligations
SET remind = $4, user_edited = TRUE, updated_at = now()
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
    b.amount_min,
    b.amount_max,
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
    v.institution_name
FROM accounts a
JOIN account_access v ON v.account_id = a.id
WHERE v.household_id = $1
  AND (v.user_id = $2 OR v.is_shared)
  AND a.is_active
  AND a.type = 'depository'
ORDER BY v.institution_name, a.name;

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
JOIN account_access v ON v.account_id = a.id
LEFT JOIN merchant_aliases ma
       ON ma.household_id = $1
      AND ma.merchant_key = t.merchant_key
      AND ma.source <> 'suggested'
WHERE v.household_id = $1
  AND (v.user_id = $2 OR v.is_shared)
  AND a.is_active
  AND NOT t.excluded_from_reports
  AND NOT t.pending
  AND t.merchant_key IS NOT NULL
  AND t.category_id IS NOT NULL
  AND t.amount > 0
  AND t.date >= $3
GROUP BY COALESCE(ma.entity_id::text, t.merchant_key);

-- --------------------------------------------------------------------------
-- Auto-posting (doc 30)
-- --------------------------------------------------------------------------

-- name: SetObligationAutoPost :one
-- Turns materialisation on or off for one obligation, and names the account a
-- posting credits. Separate from UpdateObligation because it is a different
-- decision with a different blast radius: editing a label changes a forecast,
-- turning this on starts writing transactions.
--
-- last_posted_date is reset to NULL when auto_post is switched OFF, so
-- re-enabling it later does not silently backfill every occurrence that fell in
-- the gap. Re-enabling starts from the anchor's next due date, and the handler
-- says so.
UPDATE recurring_obligations
SET auto_post          = sqlc.arg('auto_post'),
    posting_account_id = sqlc.narg('posting_account_id'),
    last_posted_date   = CASE WHEN sqlc.arg('auto_post') THEN last_posted_date ELSE NULL END,
    user_edited        = TRUE,
    updated_at         = now()
WHERE id = sqlc.arg('id')
  AND household_id = sqlc.arg('household_id')
  AND (user_id IS NULL OR user_id = sqlc.arg('user_id') OR is_shared)
  -- An obligation cannot post without a target. The DB CHECK says the same
  -- thing; repeating it here turns a constraint violation into "no rows", which
  -- the handler reports as a 400 rather than a 500.
  AND (NOT sqlc.arg('auto_post')
       OR sqlc.narg('posting_account_id')::uuid IS NOT NULL
       OR account_id IS NOT NULL)
RETURNING *;

-- name: ListObligationsDueForPosting :many
-- The worker's queue: every auto-posting obligation with at least one occurrence
-- on or before today that has not been posted yet.
--
-- Deliberately NOT household-scoped — this is a background sweep over every
-- household, like the snapshot jobs. Nothing here is returned to a user;
-- visibility is enforced when the resulting transactions are read.
--
-- The occurrence expansion is the same arithmetic as ListUpcomingObligations,
-- and for the same reason it lives in SQL: anchor_date + n whole periods
-- clamps a month end (2025-01-31 + 1 month is 2025-02-28), where stepping from
-- the previous occurrence would drift off the 31st permanently and Go's
-- time.AddDate would roll forward into March.
WITH due AS (
    SELECT *
    FROM recurring_obligations
    WHERE auto_post
      AND is_active
      AND (end_date IS NULL OR end_date >= anchor_date)
      AND (account_id IS NOT NULL OR posting_account_id IS NOT NULL)
),
bounded AS (
    SELECT
        due.*,
        CASE interval_unit
            WHEN 'day'   THEN (sqlc.arg('today')::date - anchor_date) / interval_count
            WHEN 'week'  THEN (sqlc.arg('today')::date - anchor_date) / (7 * interval_count)
            WHEN 'month' THEN (12 * (EXTRACT(YEAR  FROM sqlc.arg('today')::date)::int - EXTRACT(YEAR  FROM anchor_date)::int)
                                  + (EXTRACT(MONTH FROM sqlc.arg('today')::date)::int - EXTRACT(MONTH FROM anchor_date)::int))
                              / interval_count
            WHEN 'year'  THEN (EXTRACT(YEAR FROM sqlc.arg('today')::date)::int - EXTRACT(YEAR FROM anchor_date)::int)
                              / interval_count
        END AS n_max
    FROM due
)
SELECT
    b.id                                      AS obligation_id,
    b.household_id,
    b.label,
    b.amount,
    b.category_id,
    COALESCE(b.posting_account_id, b.account_id)::uuid AS target_account_id,
    b.account_id                              AS source_account_id,
    -- The target's shape decides how much of the posting happens. A manual
    -- investment account gets the full treatment (transaction + investment
    -- transaction + balance move); anything else gets the transaction only,
    -- because an institution owns that balance and reports the movement itself.
    ta.type                                   AS target_type,
    ta.source                                 AS target_source,
    d.due_date::date                          AS due_date
FROM bounded b
JOIN accounts ta ON ta.id = COALESCE(b.posting_account_id, b.account_id)
                AND ta.is_active
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
WHERE d.due_date <= sqlc.arg('today')::date
  AND (b.last_posted_date IS NULL OR d.due_date > b.last_posted_date)
  AND (b.end_date IS NULL OR d.due_date <= b.end_date)
  -- Bounds how far back a newly-enabled obligation reaches. Without it, turning
  -- auto-post on for a bill anchored in 2019 posts six years of transactions in
  -- one batch.
  AND d.due_date >= sqlc.arg('earliest')::date
ORDER BY b.id, d.due_date;

-- name: MarkObligationPosted :execrows
-- Advances the cursor. Runs in the same transaction as the rows it accounts
-- for: a crash before the commit replays from the same cursor, and a crash
-- after it has already written everything the cursor claims.
--
-- The WHERE repeats the cursor predicate so two workers racing on one
-- obligation produce exactly one winner, matching MarkAllowancePosted.
UPDATE recurring_obligations
SET last_posted_date = sqlc.arg('posted_through'),
    updated_at       = now()
WHERE id = sqlc.arg('id')
  AND auto_post
  AND (last_posted_date IS NULL OR last_posted_date < sqlc.arg('posted_through'));

-- name: InsertScheduledTransaction :one
-- The materialised row. obligation_id ties it back to the template, and the
-- partial unique index on (obligation_id, date) makes a duplicate posting
-- impossible rather than merely unlikely — ON CONFLICT DO NOTHING turns a
-- replay into zero rows instead of a second charge.
INSERT INTO transactions (
    account_id, amount, currency, date, name, merchant_key,
    category_id, category_source, source, pending, obligation_id
)
SELECT
    a.id, sqlc.arg('amount'), a.currency, sqlc.arg('date'), sqlc.arg('name'),
    sqlc.narg('merchant_key'), sqlc.narg('category_id'),
    CASE WHEN sqlc.narg('category_id')::uuid IS NULL THEN NULL ELSE 'manual' END,
    'scheduled', false, sqlc.arg('obligation_id')
FROM accounts a
WHERE a.id = sqlc.arg('account_id')
ON CONFLICT (obligation_id, date) WHERE obligation_id IS NOT NULL DO NOTHING
RETURNING *;

-- --------------------------------------------------------------------------
-- Reminders (docs MAD-85): overdue detection + per-occurrence satisfaction.
-- --------------------------------------------------------------------------

-- name: ListOverdueUnsatisfiedObligations :many
-- Past-due occurrences the matcher believes are STILL UNPAID — the backlog the
-- overdue_bill insight producer raises and the Reminders view surfaces.
--
-- The expansion is the same cadence arithmetic as ListUpcomingObligations
-- (anchor + n whole periods, so a 31st-monthly bill clamps rather than drifts);
-- only the WHERE differs: the window is backward (the caller passes today-N ..
-- today-1), rows are limited to remind = TRUE, and four NOT EXISTS guards drop
-- any occurrence the household has already dealt with.
--
-- What "dealt with" means, weakest to strongest:
--   (S) a satisfaction row — a member marked it paid, or a prior match recorded.
--   (A) an auto-posted transaction tied to this obligation near the due date.
--   (T) a real transaction matching the obligation's merchant, or its category
--       at a near-expected amount, within a few days of the due date.
--   (P) a structural transfer pair touching the obligation's account near the
--       due date — the credit-card-payment case, where the payment is a transfer
--       and the pair links the legs even when the two payee names disagree.
--
-- The category branch is amount-gated so two same-category bills of different
-- sizes don't suppress each other; the merchant branch is not, because
-- merchant_key is specific enough on its own. Every check is household-scoped
-- through account_access, matching the rest of the bill-calendar reads.
--
-- What the category gate is depends on whether the member stated a range
-- (MAD-120). Without one it stays the ±25% band around amount — the app's own
-- guess at "near enough". With one it widens to the CANDIDATE band, half the low
-- bound to double the high one, and that widening is deliberate: guard (T) asks
-- "is this plausibly the payment", not "was it the amount you expected". A phone
-- bill stated at $40–$60 that lands at $90 IS the payment, so it must suppress
-- the overdue reminder — and ListBillRangeExceptions below, which shares this
-- exact candidate predicate, raises it as a surprise instead. Splitting the two
-- questions this way is what stops one out-of-range charge producing both an
-- "we can't find a payment" and a "that was more than expected" insight.


--
-- Suppression is the safer direction to overdo here: a hidden reminder for a
-- bill that was genuinely paid is a minor nuisance, while nagging about one that
-- was paid is the failure that makes a reminder feature untrustworthy. The
-- manual mark-paid (below) and the remind toggle are the escape hatches when the
-- matcher gets it wrong either way.
WITH visible AS (
    SELECT *
    FROM recurring_obligations
    WHERE household_id = $1
      AND (recurring_obligations.user_id IS NULL
           OR recurring_obligations.user_id = $2
           OR recurring_obligations.is_shared)
      AND is_active
      AND remind
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
    -- Cast to date here rather than only in the SELECT: the satisfaction NOT
    -- EXISTS guards below also reference d.due_date, and make_interval yields a
    -- timestamp, so an uncast d.due_date would make `d.due_date - 3` a
    -- timestamp-minus-integer that Postgres has no operator for.
    SELECT (b.anchor_date + make_interval(
        days   => CASE b.interval_unit
                      WHEN 'day'  THEN g.n * b.interval_count
                      WHEN 'week' THEN g.n * b.interval_count * 7
                      ELSE 0 END,
        months => CASE b.interval_unit WHEN 'month' THEN g.n * b.interval_count ELSE 0 END,
        years  => CASE b.interval_unit WHEN 'year'  THEN g.n * b.interval_count ELSE 0 END
    ))::date AS due_date
) d
WHERE d.due_date >= $3::date
  AND d.due_date <= $4::date
  AND (b.end_date IS NULL OR d.due_date <= b.end_date)
  -- (S) a member marked it paid, or a match was recorded before.
  AND NOT EXISTS (
      SELECT 1 FROM obligation_satisfaction os
      WHERE os.obligation_id = b.id AND os.due_date = d.due_date
  )
  -- (A) the auto-posting worker materialised this occurrence.
  AND NOT EXISTS (
      SELECT 1 FROM transactions t
      WHERE t.obligation_id = b.id
        AND t.date BETWEEN d.due_date - 3 AND d.due_date + 3
  )
  -- (T) a household transaction under the obligation's merchant, or in its
  -- category at a near-expected amount, within the payment window.
  AND NOT EXISTS (
      SELECT 1
      FROM transactions t
      JOIN accounts a   ON a.id = t.account_id
      JOIN account_access v ON v.account_id = a.id
      WHERE v.household_id = $1
        AND t.date BETWEEN d.due_date - 5 AND d.due_date + 10
        AND NOT t.excluded_from_reports
        AND NOT t.pending
        AND (
            (b.merchant_key IS NOT NULL AND t.merchant_key = b.merchant_key)
            OR (b.category_id IS NOT NULL AND t.category_id = b.category_id
                AND t.amount > 0
                AND CASE
                        WHEN b.amount_min IS NOT NULL
                            THEN t.amount BETWEEN b.amount_min * 0.5 AND b.amount_max * 2
                        ELSE ABS(t.amount - b.amount) <= b.amount * 0.25
                    END)
        )
  )
  -- (P) a structural transfer pair touched the obligation's account within the
  -- window — the card-payment case. Either leg counts: a payment leaving
  -- checking (out leg) or landing on the card (in leg). Skipped entirely when
  -- the obligation names no account.
  AND (
      b.account_id IS NULL
      OR NOT EXISTS (
          SELECT 1 FROM transaction_pairs tp
          JOIN transactions tl ON tl.id = tp.out_txn_id
          WHERE tp.household_id = $1
            AND tl.account_id = b.account_id
            AND tl.date BETWEEN d.due_date - 5 AND d.due_date + 10
          UNION ALL
          SELECT 1 FROM transaction_pairs tp
          JOIN transactions tl ON tl.id = tp.in_txn_id
          WHERE tp.household_id = $1
            AND tl.account_id = b.account_id
            AND tl.date BETWEEN d.due_date - 5 AND d.due_date + 10
      )
  )
ORDER BY d.due_date, b.label;

-- name: ListBillRangeExceptions :many
-- Occurrences whose payment landed, but not for the amount the household said to
-- expect — what the bill_out_of_range insight raises (MAD-120).
--
-- This is the inverse of the query above and deliberately its mirror image. That
-- one asks "did nothing plausibly pay this?"; this one asks "something plausibly
-- paid it, was it inside the stated range?". The candidate predicate is
-- IDENTICAL in both — merchant match, or category match inside the half-to-double
-- band — so every occurrence with a range falls into exactly one of the three
-- outcomes and never into two:
--
--   nothing in the candidate band ....... overdue_bill (the query above)
--   candidate inside [min, max] ......... paid, silence
--   candidate outside [min, max] ........ bill_out_of_range (this query)
--
-- Editing the candidate predicate in one place and not the other opens a gap
-- where a real charge is reported as both missing and surprising, or as neither.
--
-- Only obligations that state a range are considered: without one there is no
-- expectation to have missed, and the ±25% heuristic is the app's guess rather
-- than the member's statement — not a thing to raise an alarm about. remind
-- gates this the same way it gates the overdue reminder, so one opt-out silences
-- both halves of a bill's coaching.
--
-- One row per occurrence: the LATERAL picks the single closest candidate by
-- amount, so a month with two charges under the same merchant reports the one
-- most likely to be the bill rather than one insight per charge.
WITH visible AS (
    SELECT *
    FROM recurring_obligations
    WHERE household_id = $1
      AND (recurring_obligations.user_id IS NULL
           OR recurring_obligations.user_id = $2
           OR recurring_obligations.is_shared)
      AND is_active
      AND remind
      AND amount_min IS NOT NULL
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
    b.amount_min,
    b.amount_max,
    b.interval_count,
    b.interval_unit,
    b.source,
    d.due_date::date AS due_date,
    m.id           AS transaction_id,
    m.amount       AS charged_amount,
    m.date::date   AS charged_date,
    m.name         AS charged_name
FROM bounded b
CROSS JOIN LATERAL generate_series(0, GREATEST(b.n_max, 0)) AS g(n)
CROSS JOIN LATERAL (
    -- Cast to date for the same reason as the query above: d.due_date is used in
    -- date arithmetic below and make_interval yields a timestamp.
    SELECT (b.anchor_date + make_interval(
        days   => CASE b.interval_unit
                      WHEN 'day'  THEN g.n * b.interval_count
                      WHEN 'week' THEN g.n * b.interval_count * 7
                      ELSE 0 END,
        months => CASE b.interval_unit WHEN 'month' THEN g.n * b.interval_count ELSE 0 END,
        years  => CASE b.interval_unit WHEN 'year'  THEN g.n * b.interval_count ELSE 0 END
    ))::date AS due_date
) d
JOIN LATERAL (
    SELECT t.id, t.amount, t.date, t.name
    FROM transactions t
    JOIN accounts a       ON a.id = t.account_id
    JOIN account_access v ON v.account_id = a.id
    WHERE v.household_id = $1
      AND t.date BETWEEN d.due_date - 5 AND d.due_date + 10
      AND NOT t.excluded_from_reports
      AND NOT t.pending
      AND t.amount > 0
      -- Candidate: plausibly this bill's payment. Mirrors guard (T) above.
      AND (
          (b.merchant_key IS NOT NULL AND t.merchant_key = b.merchant_key)
          OR (b.category_id IS NOT NULL AND t.category_id = b.category_id
              AND t.amount BETWEEN b.amount_min * 0.5 AND b.amount_max * 2)
      )
      -- ...but outside what the member said to expect. This is the whole point.
      AND (t.amount < b.amount_min OR t.amount > b.amount_max)
    ORDER BY ABS(t.amount - b.amount), t.date
    LIMIT 1
) m ON TRUE
WHERE d.due_date >= $3::date
  AND d.due_date <= $4::date
  AND (b.end_date IS NULL OR d.due_date <= b.end_date)
  -- Already dealt with: a member who marked this occurrence paid has accepted
  -- the charge, whatever it was, so there is no surprise left to report.
  AND NOT EXISTS (
      SELECT 1 FROM obligation_satisfaction os
      WHERE os.obligation_id = b.id AND os.due_date = d.due_date
  )
  -- Auto-posted: the app wrote this transaction itself from the obligation's own
  -- amount, so it cannot be a surprise about what the biller charged.
  AND NOT EXISTS (
      SELECT 1 FROM transactions t
      WHERE t.obligation_id = b.id
        AND t.date BETWEEN d.due_date - 3 AND d.due_date + 3
  )
ORDER BY d.due_date, b.label;

-- name: MarkObligationSatisfied :one
-- Record that one occurrence was paid. A manual mark is authoritative and
-- overwrites a prior 'matched' row; re-marking refreshes satisfied_at. The
-- obligation is re-resolved through the household guard so a member can only
-- satisfy obligations their household owns, and the (obligation_id, due_date)
-- PK makes the operation an idempotent upsert.
INSERT INTO obligation_satisfaction (obligation_id, due_date, source, user_id)
SELECT sqlc.arg('obligation_id')::uuid, sqlc.arg('due_date')::date, 'manual', sqlc.narg('user_id')
FROM recurring_obligations o
WHERE o.id = sqlc.arg('obligation_id')
  AND o.household_id = sqlc.arg('household_id')
  AND (o.user_id IS NULL OR o.user_id = sqlc.narg('user_id') OR o.is_shared)
ON CONFLICT (obligation_id, due_date) DO UPDATE SET
    source       = 'manual',
    user_id      = EXCLUDED.user_id,
    satisfied_at = now()
RETURNING *;

-- name: ClearObligationSatisfied :execrows
-- Remove a satisfaction row, re-arming the reminder. Used to undo a manual
-- mark-paid that was wrong, or to clear a stale entry. Household-scoped through
-- the obligation so a member can only clear their own household's rows.
DELETE FROM obligation_satisfaction os
USING recurring_obligations o
WHERE os.obligation_id = sqlc.arg('obligation_id')
  AND os.due_date = sqlc.arg('due_date')
  AND o.id = os.obligation_id
  AND o.household_id = sqlc.arg('household_id');

-- name: ListSatisfiedOccurrences :many
-- The paid occurrences for the obligations visible to the caller, inside a date
-- window. Backs the "already paid" check the Schedule view renders beside each
-- bill, and is what lets the UI mark an occurrence green without re-running the
-- matcher.
SELECT os.obligation_id, os.due_date, os.source, os.satisfied_at
FROM obligation_satisfaction os
JOIN recurring_obligations o ON o.id = os.obligation_id
WHERE o.household_id = $1
  AND (o.user_id IS NULL OR o.user_id = $2 OR o.is_shared)
  AND os.due_date BETWEEN $3 AND $4
ORDER BY os.due_date;
