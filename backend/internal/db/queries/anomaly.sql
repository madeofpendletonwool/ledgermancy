-- Anomaly detection: per-merchant outliers and duplicate charges.
--
-- Both detectors ask a question about ONE MERCHANT'S OWN HISTORY, which is what
-- separates them from everything in reports.sql. "Well above your typical
-- purchase" (GetAverageSpendingTransaction, household-wide) cannot tell you that
-- Netflix charged $900, because $900 is unremarkable next to a mortgage payment.
-- Only the merchant's own distribution can.
--
-- Both queries open with the same `spend` CTE: the standard transaction universe
-- (visibility, is_spend, no transfers, no income, no excluded rows) with merchant
-- keys RESOLVED per the header of merchants.sql, plus the suppression NOT EXISTS.
-- Keeping the universe identical between the two matters — a charge that is a
-- candidate for one detector and invisible to the other would make the "exactly
-- one insight" guarantees untestable.
--
-- ALL statistics are computed here in NUMERIC. The producers pick a threshold and
-- compare; they never add, average or divide. PERCENTILE_DISC, never
-- PERCENTILE_CONT: _CONT takes and returns DOUBLE PRECISION and would round-trip
-- money through a float, which this codebase never does (see reports.sql:572).

-- name: ListMerchantOutlierCandidates :many
-- Recent charges paired with their own merchant's baseline, LEAVE-ONE-OUT.
--
-- The leave-one-out (`h.tx_id <> r.tx_id`) is the load-bearing line. Include the
-- candidate in its own baseline and PERCENTILE_DISC(0.95) over a merchant whose
-- largest-ever charge IS the candidate returns the candidate — so `amount >= p95`
-- is trivially true for every new maximum and the detector fires on everything.
-- GetMerchantSpendBaseline takes an exclude_tx parameter for the same reason.
--
-- This is also why the baseline is computed here rather than cached in a table:
-- median and p95 are order statistics, so a stored aggregate cannot have the
-- candidate backed out of it afterwards the way a stored mean and count can.
--
-- The baseline arm excludes is_one_time rows (it is a trailing baseline, and the
-- reports.sql rule is that only trailing baselines do). The candidate arm keeps
-- them: a loan payoff really happened and should still be flaggable — it just
-- must not teach the detector that $14k is normal at that merchant.
--
-- mean_amount and stddev_amount are returned but NEVER gated on. They exist so
-- the insight payload can stay auditable and so the tests can assert the contrast
-- that justifies the median/p95 design: on a merchant with one historical freak
-- charge, mean+3σ is dragged past every plausible outlier and fires on nothing.
WITH spend AS (
    SELECT
        t.id                                                 AS tx_id,
        t.plaid_transaction_id,
        t.pending_transaction_id,
        t.account_id,
        -- Resolved merchant key, with a name-derived fallback.
        --
        -- merchant_key is NULL for CSV imports and manual entries — Plaid is what
        -- populates it. Requiring it (as GetRecurringMerchants does) would drop
        -- that whole population, and largeTransactionProducer reads this query
        -- now, so it would have silently stopped reporting on manually entered
        -- rows it used to cover. For those the name IS the merchant identity, and
        -- it is already what the recap displays. The 'name:' prefix keeps a
        -- derived key from ever colliding with a real merchant_key or an entity
        -- UUID, so resolution stays idempotent.
        COALESCE(
            ma.entity_id::text,
            t.merchant_key,
            'name:' || lower(COALESCE(t.merchant_name, t.name))
        )                                                    AS merchant_key,
        COALESCE(me.canonical_name, t.merchant_name, t.name) AS merchant,
        COALESCE(c.name, '')                                 AS category_name,
        t.date,
        ABS(t.amount)::numeric                               AS amount,
        t.is_one_time
    FROM transactions t
    JOIN accounts a    ON a.id = t.account_id
    JOIN plaid_items i ON i.id = a.plaid_item_id
    JOIN users u       ON u.id = i.user_id
    LEFT JOIN categories c ON c.id = t.category_id
    LEFT JOIN merchant_aliases ma
           ON ma.household_id = @household_id
          AND ma.merchant_key = t.merchant_key
          AND ma.source <> 'suggested'
    LEFT JOIN merchant_entities me ON me.id = ma.entity_id
    WHERE u.household_id = @household_id
      AND (i.user_id = @user_id OR i.is_shared)
      AND a.is_active
      AND NOT t.excluded_from_reports
      AND NOT t.pending
      AND is_spend(t.amount, a.type)
      AND NOT COALESCE(c.is_income, FALSE)
      AND NOT COALESCE(c.is_transfer, FALSE)
      AND t.date >= @baseline_from
      -- Suppression, resolved on BOTH sides so an override recorded against one
      -- raw descriptor silences the whole merged merchant. 'all' covers both
      -- detectors; 'outlier' covers only this one.
      AND NOT EXISTS (
          SELECT 1 FROM anomaly_overrides ao
          LEFT JOIN merchant_aliases aoa
                 ON aoa.household_id = ao.household_id
                AND aoa.merchant_key = ao.merchant_key
                AND aoa.source <> 'suggested'
          WHERE ao.household_id = @household_id
            AND ao.scope IN ('all', 'outlier')
            AND COALESCE(aoa.entity_id::text, ao.merchant_key)
                = COALESCE(
                    ma.entity_id::text,
                    t.merchant_key,
                    'name:' || lower(COALESCE(t.merchant_name, t.name))
                  )
      )
),
recent AS (
    -- Bounded at both ends. The upper bound matters: a manually entered
    -- future-dated transaction is not yet an event, and flagging a charge as
    -- anomalous before it has happened is indefensible. GetLargestTransactions
    -- carried the same bound, and largeTransactionProducer reads this query now.
    SELECT * FROM spend
    WHERE date >= @recent_from
      AND date <= @recent_to::date
    ORDER BY amount DESC
    LIMIT @max_candidates
)
SELECT
    r.tx_id,
    r.merchant_key::text AS merchant_key,
    r.merchant,
    r.category_name,
    r.date,
    r.amount,
    b.sample_count,
    b.median_amount,
    b.p95_amount,
    b.mean_amount,
    b.stddev_amount,
    b.max_amount
FROM recent r
CROSS JOIN LATERAL (
    SELECT
        COUNT(*)::bigint                                                              AS sample_count,
        COALESCE(PERCENTILE_DISC(0.5)  WITHIN GROUP (ORDER BY h.amount), 0)::numeric  AS median_amount,
        COALESCE(PERCENTILE_DISC(0.95) WITHIN GROUP (ORDER BY h.amount), 0)::numeric  AS p95_amount,
        COALESCE(AVG(h.amount), 0)::numeric                                           AS mean_amount,
        COALESCE(STDDEV_SAMP(h.amount), 0)::numeric                                   AS stddev_amount,
        COALESCE(MAX(h.amount), 0)::numeric                                           AS max_amount
    FROM spend h
    WHERE h.merchant_key = r.merchant_key
      AND h.tx_id <> r.tx_id
      AND NOT h.is_one_time
) b
ORDER BY r.amount DESC;

-- name: ListDuplicateChargeCandidates :many
-- The same merchant billing the same amount on the same card within a day.
--
-- Five false-positive sources, each closed deliberately:
--
--  1. PENDING -> POSTED. `NOT t.pending` in the CTE drops the pending side
--     outright, and plaid/sync.go already deletes a pending row when its posted
--     counterpart arrives. The plaid_transaction_id / pending_transaction_id
--     cross-check below is belt-and-braces for an institution that never linked
--     the pair, or a row predating that delete path.
--
--  2. LEGITIMATE SAME-DAY REPEATS. Amount equality does NOT solve this — two
--     $5.75 lattes are exactly equal. Two things do: the caller's dollar floor,
--     and `historical_pairs` below. If this merchant on this card has EVER
--     produced a same-amount adjacent-day pair before the recent window, doubling
--     up is simply what happens there, and it is silenced permanently. That
--     covers transit fares, vending, parking and the fixed-price daily coffee —
--     the actual generators of this false positive.
--
--  3. RECURRING CHARGES ON CADENCE. Vacuous at this window and deliberately not
--     coded: the tightest cadence obligations.CadenceForGapDays recognises is
--     weekly, so no recurring charge can produce two billings a day apart. If a
--     sub-weekly cadence is ever added, this is the guard that has to appear.
--
--  4. THE SAME INSTITUTION LINKED TWICE. Two plaid_items, two accounts, one
--     charge reported twice — identical merchant, date and amount. Matching
--     within `account_id` closes it, and costs nothing: a genuine merchant
--     double-charge always hits the same card.
--
--  5. REFUNDS AND REVERSALS. Free, via is_spend() in the CTE — a reversal carries
--     the opposite sign and never enters the universe. Named here so nobody
--     "fixes" it later by widening the predicate.
--
-- DISTINCT ON collapses a group of identical charges to its earliest member, so a
-- pair raises one row and a third identical charge refreshes that same row rather
-- than raising a second.
WITH spend AS (
    SELECT
        t.id                                                 AS tx_id,
        t.plaid_transaction_id,
        t.pending_transaction_id,
        t.account_id,
        -- Resolved merchant key, with a name-derived fallback.
        --
        -- merchant_key is NULL for CSV imports and manual entries — Plaid is what
        -- populates it. Requiring it (as GetRecurringMerchants does) would drop
        -- that whole population, and largeTransactionProducer reads this query
        -- now, so it would have silently stopped reporting on manually entered
        -- rows it used to cover. For those the name IS the merchant identity, and
        -- it is already what the recap displays. The 'name:' prefix keeps a
        -- derived key from ever colliding with a real merchant_key or an entity
        -- UUID, so resolution stays idempotent.
        COALESCE(
            ma.entity_id::text,
            t.merchant_key,
            'name:' || lower(COALESCE(t.merchant_name, t.name))
        )                                                    AS merchant_key,
        COALESCE(me.canonical_name, t.merchant_name, t.name) AS merchant,
        COALESCE(c.name, '')                                 AS category_name,
        t.date,
        ABS(t.amount)::numeric                               AS amount,
        t.is_one_time
    FROM transactions t
    JOIN accounts a    ON a.id = t.account_id
    JOIN plaid_items i ON i.id = a.plaid_item_id
    JOIN users u       ON u.id = i.user_id
    LEFT JOIN categories c ON c.id = t.category_id
    LEFT JOIN merchant_aliases ma
           ON ma.household_id = @household_id
          AND ma.merchant_key = t.merchant_key
          AND ma.source <> 'suggested'
    LEFT JOIN merchant_entities me ON me.id = ma.entity_id
    WHERE u.household_id = @household_id
      AND (i.user_id = @user_id OR i.is_shared)
      AND a.is_active
      AND NOT t.excluded_from_reports
      AND NOT t.pending
      AND is_spend(t.amount, a.type)
      AND NOT COALESCE(c.is_income, FALSE)
      AND NOT COALESCE(c.is_transfer, FALSE)
      AND t.date >= @baseline_from
      AND NOT EXISTS (
          SELECT 1 FROM anomaly_overrides ao
          LEFT JOIN merchant_aliases aoa
                 ON aoa.household_id = ao.household_id
                AND aoa.merchant_key = ao.merchant_key
                AND aoa.source <> 'suggested'
          WHERE ao.household_id = @household_id
            AND ao.scope IN ('all', 'duplicate')
            AND COALESCE(aoa.entity_id::text, ao.merchant_key)
                = COALESCE(
                    ma.entity_id::text,
                    t.merchant_key,
                    'name:' || lower(COALESCE(t.merchant_name, t.name))
                  )
      )
),
-- Merchants that habitually double up, from BEFORE the recent window. Grouped by
-- merchant and card but deliberately NOT by amount: habitual repetition is a
-- property of the merchant, not of one price. A transit operator charging $2.75
-- twice in March and $3.25 twice in June is the same "this merchant bills twice"
-- fact, and keying on the amount would re-flag it every time the fare changed.
-- LAG over (card, merchant, amount) ordered by date: a gap of 0 or 1 day to the
-- previous identical charge IS a historical pair. A self-join would read more
-- directly, but sqlc's parser cannot resolve aliases when a CTE is joined to
-- itself — and the window form is one pass over the partition rather than a
-- quadratic match, so it is the better shape regardless.
hist AS (
    SELECT merchant_key, account_id, COUNT(*) AS pairs
    FROM (
        SELECT
            merchant_key,
            account_id,
            date - LAG(date) OVER (
                PARTITION BY account_id, merchant_key, amount ORDER BY date
            ) AS gap
        FROM spend
        WHERE date < @recent_from
          AND NOT is_one_time
    ) g
    WHERE gap IS NOT NULL AND gap <= 1
    GROUP BY merchant_key, account_id
),
counted AS (
    SELECT
        a.tx_id,
        a.merchant_key,
        a.merchant,
        a.category_name,
        a.account_id,
        a.amount,
        a.date,
        (
            SELECT COUNT(*) FROM spend b
             WHERE b.account_id   = a.account_id
               AND b.merchant_key = a.merchant_key
               AND b.amount       = a.amount
               AND b.tx_id       <> a.tx_id
               AND b.date BETWEEN a.date - 1 AND a.date + 1
               AND a.plaid_transaction_id IS DISTINCT FROM b.pending_transaction_id
               AND b.plaid_transaction_id IS DISTINCT FROM a.pending_transaction_id
        ) AS peers,
        COALESCE(hist.pairs, 0) AS historical_pairs
    FROM spend a
    LEFT JOIN hist
           ON hist.merchant_key = a.merchant_key
          AND hist.account_id   = a.account_id
    WHERE a.date >= @recent_from
      AND a.date <= @recent_to::date
)
SELECT DISTINCT ON (merchant_key, account_id, amount)
    tx_id,
    merchant_key::text AS merchant_key,
    merchant,
    category_name,
    amount,
    date             AS first_date,
    (peers + 1)::int AS charge_count
FROM counted
WHERE peers > 0
  AND historical_pairs = 0
ORDER BY merchant_key, account_id, amount, date, tx_id;

-- name: SuppressAnomalyMerchant :exec
-- Mark a merchant normal for one or both anomaly detectors. Idempotent:
-- re-suppressing is a no-op and does not disturb the stored label.
--
-- The key is stored resolved (merchants.sql), so suppressing a merged merchant
-- records one row against the entity rather than one per descriptor. Resolution
-- is idempotent, so a caller may pass either a raw or an already-resolved key.
INSERT INTO anomaly_overrides (household_id, merchant_key, merchant_label, scope)
VALUES (
    @household_id,
    COALESCE(
        (SELECT ma.entity_id::text FROM merchant_aliases ma
          WHERE ma.household_id = @household_id
            AND ma.merchant_key = @merchant_key::text
            AND ma.source <> 'suggested'),
        @merchant_key::text
    ),
    @merchant_label,
    @scope
)
ON CONFLICT (household_id, merchant_key, scope) DO NOTHING;

-- name: UnsuppressAnomalyMerchant :exec
-- Restore a merchant to the anomaly detectors. Clears every override at this
-- scope that resolves to the same merchant, so undoing a suppression on a merged
-- merchant also clears per-descriptor rows recorded before the merge.
DELETE FROM anomaly_overrides ao
WHERE ao.household_id = @household_id
  AND ao.scope = @scope
  AND COALESCE(
        (SELECT a2.entity_id::text FROM merchant_aliases a2
          WHERE a2.household_id = ao.household_id
            AND a2.merchant_key = ao.merchant_key
            AND a2.source <> 'suggested'),
        ao.merchant_key
      ) = COALESCE(
        (SELECT a3.entity_id::text FROM merchant_aliases a3
          WHERE a3.household_id = @household_id
            AND a3.merchant_key = @merchant_key::text
            AND a3.source <> 'suggested'),
        @merchant_key::text
      );

-- name: ListAnomalyOverrides :many
-- The household's suppressed merchants, for the "restore" UI.
--
-- The stored key is already resolved, but it is resolved again on the way out so
-- a row written before a later merge still addresses the merchant detail view.
-- Resolution is idempotent, so doing it twice is a no-op.
SELECT
    ao.merchant_key,
    ao.merchant_label,
    ao.scope,
    ao.created_at,
    COALESCE(ma.entity_id::text, ao.merchant_key)::text AS resolved_merchant_key
FROM anomaly_overrides ao
LEFT JOIN merchant_aliases ma
       ON ma.household_id = ao.household_id
      AND ma.merchant_key = ao.merchant_key
      AND ma.source <> 'suggested'
WHERE ao.household_id = $1
ORDER BY ao.merchant_label, ao.merchant_key, ao.scope;
