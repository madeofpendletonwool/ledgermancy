-- Reporting queries.
--
-- Conventions that every query here shares, and that the numbers depend on:
--
--   * Plaid signs amounts as POSITIVE = money leaving the account. So spending
--     is sum(amount) over positive rows, and income is -sum(amount) over
--     negative rows in income categories.
--   * Transfers are excluded from BOTH income and spending. Moving money
--     between your own accounts is neither, and counting it would inflate
--     both sides. This is also what stops credit-card payments being
--     double-counted against the purchases they settle.
--   * Every total is computed here, in NUMERIC, never in the application and
--     never in JavaScript.
--   * Visibility is always scoped: own items plus shared household items.

-- name: GetSpendingSummary :one
-- Headline figures for one period: what came in, what went out, and what was
-- left to invest.
WITH visible AS (
    SELECT t.amount, c.is_income, c.is_transfer, c.is_fixed
    FROM transactions t
    JOIN accounts a    ON a.id = t.account_id
    JOIN plaid_items i ON i.id = a.plaid_item_id
    JOIN users u       ON u.id = i.user_id
    LEFT JOIN categories c ON c.id = t.category_id
    WHERE u.household_id = $1
      AND (i.user_id = $2 OR i.is_shared)
      AND a.is_active
      AND NOT t.excluded_from_reports
      AND NOT t.pending
      AND t.date >= $3 AND t.date <= $4
)
SELECT
    COALESCE(SUM(-amount) FILTER (WHERE is_income), 0)::numeric        AS income,
    COALESCE(SUM(amount)  FILTER (WHERE NOT COALESCE(is_income, FALSE)
                                    AND NOT COALESCE(is_transfer, FALSE)
                                    AND amount > 0), 0)::numeric       AS spending,
    COALESCE(SUM(amount)  FILTER (WHERE COALESCE(is_fixed, FALSE)
                                    AND amount > 0), 0)::numeric       AS fixed_spending,
    COALESCE(SUM(amount)  FILTER (WHERE NOT COALESCE(is_income, FALSE)
                                    AND NOT COALESCE(is_transfer, FALSE)
                                    AND NOT COALESCE(is_fixed, FALSE)
                                    AND amount > 0), 0)::numeric       AS discretionary_spending,
    COUNT(*)::bigint                                                    AS transaction_count
FROM visible;

-- name: GetSpendingByCategory :many
-- Spending broken down by category for one period, largest first.
SELECT
    c.id      AS category_id,
    c.name    AS category_name,
    c.slug    AS category_slug,
    c.color   AS category_color,
    c.is_fixed,
    SUM(t.amount)::numeric AS total,
    COUNT(*)::bigint       AS transaction_count
FROM transactions t
JOIN accounts a    ON a.id = t.account_id
JOIN plaid_items i ON i.id = a.plaid_item_id
JOIN users u       ON u.id = i.user_id
JOIN categories c  ON c.id = t.category_id
WHERE u.household_id = $1
  AND (i.user_id = $2 OR i.is_shared)
  AND a.is_active
  AND NOT t.excluded_from_reports
  AND NOT t.pending
  AND t.date >= $3 AND t.date <= $4
  AND NOT c.is_income
  AND NOT c.is_transfer
  AND t.amount > 0
GROUP BY c.id, c.name, c.slug, c.color, c.is_fixed
ORDER BY total DESC;

-- name: GetMonthlyTrend :many
-- Income, spending and leftover per calendar month across a range. Drives the
-- rolling-twelve chart and the month-over-month comparison.
SELECT
    date_trunc('month', t.date)::date AS month,
    COALESCE(SUM(-t.amount) FILTER (WHERE c.is_income), 0)::numeric AS income,
    COALESCE(SUM(t.amount)  FILTER (WHERE NOT COALESCE(c.is_income, FALSE)
                                     AND NOT COALESCE(c.is_transfer, FALSE)
                                     AND t.amount > 0), 0)::numeric AS spending
FROM transactions t
JOIN accounts a    ON a.id = t.account_id
JOIN plaid_items i ON i.id = a.plaid_item_id
JOIN users u       ON u.id = i.user_id
LEFT JOIN categories c ON c.id = t.category_id
WHERE u.household_id = $1
  AND (i.user_id = $2 OR i.is_shared)
  AND a.is_active
  AND NOT t.excluded_from_reports
  AND NOT t.pending
  AND t.date >= $3 AND t.date <= $4
GROUP BY 1
ORDER BY 1;

-- name: GetSpendingByDay :many
-- Spending per calendar day across a range. Drives the dashboard's
-- "this month, by day" chart. Same spend definition as everywhere else:
-- money out (amount > 0), excluding income and transfers. Only days with
-- spending appear; the frontend fills the empty days across the month.
SELECT
    t.date::date AS day,
    COALESCE(SUM(t.amount) FILTER (WHERE NOT COALESCE(c.is_income, FALSE)
                                     AND NOT COALESCE(c.is_transfer, FALSE)
                                     AND t.amount > 0), 0)::numeric AS spending
FROM transactions t
JOIN accounts a    ON a.id = t.account_id
JOIN plaid_items i ON i.id = a.plaid_item_id
JOIN users u       ON u.id = i.user_id
LEFT JOIN categories c ON c.id = t.category_id
WHERE u.household_id = $1
  AND (i.user_id = $2 OR i.is_shared)
  AND a.is_active
  AND NOT t.excluded_from_reports
  AND NOT t.pending
  AND t.date >= $3 AND t.date <= $4
GROUP BY t.date
ORDER BY t.date;

-- name: GetCategoryAverages :many
-- Per-category monthly average and annual total — the figures planning needs
-- ("what do you spend on groceries in a typical month?").
--
-- The average divides by the number of months actually covered by the range,
-- not the number of months that happen to contain a transaction, so an
-- occasional category is not overstated.
-- Months elapsed across the range, floored at 1.
--
-- Note the absence of a "+ 1". Adding one to make the count inclusive looks
-- right for a single month, but over a trailing year it yields 13 and
-- understates every average by about 8% — a $6,235 annual total came out as
-- $479.65/month instead of $519.63. Elapsed months is what "per month" means.
WITH months AS (
    SELECT GREATEST(
        1,
        EXTRACT(YEAR FROM age($4::date, $3::date)) * 12
        + EXTRACT(MONTH FROM age($4::date, $3::date))
    )::numeric AS n
)
SELECT
    c.id    AS category_id,
    c.name  AS category_name,
    c.slug  AS category_slug,
    c.color AS category_color,
    c.is_fixed,
    SUM(t.amount)::numeric                        AS total,
    (SUM(t.amount) / (SELECT n FROM months))::numeric AS monthly_average,
    COUNT(*)::bigint                              AS transaction_count
FROM transactions t
JOIN accounts a    ON a.id = t.account_id
JOIN plaid_items i ON i.id = a.plaid_item_id
JOIN users u       ON u.id = i.user_id
JOIN categories c  ON c.id = t.category_id
WHERE u.household_id = $1
  AND (i.user_id = $2 OR i.is_shared)
  AND a.is_active
  AND NOT t.excluded_from_reports
  AND NOT t.pending
  AND t.date >= $3 AND t.date <= $4
  AND NOT c.is_income
  AND NOT c.is_transfer
  AND t.amount > 0
GROUP BY c.id, c.name, c.slug, c.color, c.is_fixed
ORDER BY total DESC;

-- name: GetTopMerchants :many
-- Grouped by canonical merchant: fragments of one business that the household
-- has merged (see merchants.sql) collapse into a single row under the entity's
-- name. With no aliases the COALESCE falls straight through to the descriptor
-- and the result reads as it did before canonicalisation existed.
--
-- merchant_key is the RESOLVED key, which is what makes a row here a link: it
-- addresses the merchant detail view for both a merged entity and a bare
-- descriptor, and it is stable across a later merge because resolution is
-- idempotent.
--
-- The grouping is BY THAT KEY, not by the display name. Grouping by name splits
-- one merchant across several rows whenever the bank varies its own text —
-- "THE HOME DEPOT #4905" and "THE HOME DEPOT 4905" normalise to one key and are
-- one business, but as names they are two, and they listed as two top merchants
-- with two different totals that both linked to the same detail page. A row and
-- the page it opens have to agree.
--
-- The name shown is the most RECENT descriptor for the key, matching how
-- ListMerchantKeyStats picks its sample: the form a merchant bills under now is
-- the form the user recognises.
SELECT
    COALESCE(
        me.canonical_name,
        (array_agg(COALESCE(t.merchant_name, t.name) ORDER BY t.date DESC))[1]
    )::text                                                AS merchant,
    COALESCE(ma.entity_id::text, t.merchant_key, '')::text AS merchant_key,
    SUM(t.amount)::numeric            AS total,
    COUNT(*)::bigint                  AS transaction_count
FROM transactions t
JOIN accounts a    ON a.id = t.account_id
JOIN plaid_items i ON i.id = a.plaid_item_id
JOIN users u       ON u.id = i.user_id
LEFT JOIN categories c ON c.id = t.category_id
LEFT JOIN merchant_aliases ma
       ON ma.household_id = $1
      AND ma.merchant_key = t.merchant_key
      AND ma.source <> 'suggested'
LEFT JOIN merchant_entities me ON me.id = ma.entity_id
WHERE u.household_id = $1
  AND (i.user_id = $2 OR i.is_shared)
  AND a.is_active
  AND NOT t.excluded_from_reports
  AND NOT t.pending
  AND t.date >= $3 AND t.date <= $4
  AND NOT COALESCE(c.is_income, FALSE)
  AND NOT COALESCE(c.is_transfer, FALSE)
  AND t.amount > 0
GROUP BY me.canonical_name, 2
ORDER BY total DESC
LIMIT $5;

-- name: ListMerchantSpend :many
-- Every merchant with spend in the window, canonicalised, with everything the
-- merchant explorer needs to rank and annotate a row: the window's total, the
-- equivalent prior window's total, whether the merchant is new, and the category
-- most of its spend lands in.
--
-- Deliberately NOT paginated and NOT sorted server-side beyond total DESC. A
-- household has hundreds of merchants, not millions, so the whole window ships
-- in one response and the page searches, sorts and pages it locally — which is
-- what makes search feel instant and keeps the ranking rules in one readable
-- place instead of a CASE-per-sort-order in SQL. The caller passes a limit as a
-- backstop and reports the truncation rather than hiding it.
--
-- GetTopMerchants stays as it is: the Dashboard's top-five card needs nothing
-- here, and two reconciliation tests pin it. A third test pins the two together
-- so this query cannot drift from it.
--
-- The search needle is applied AFTER aggregation, and this is the whole subtlety
-- of the query. Filtering rows by descriptor first would show a grouped merchant
-- carrying only the matching fragment's total — a number that contradicts the
-- page the row links to. bool_or folds the test into the aggregate instead, so
-- matching ANY descriptor returns the merchant with ALL of its spend.
WITH window_spend AS (
    SELECT
        COALESCE(ma.entity_id::text, t.merchant_key, '')::text AS merchant_key,
        COALESCE(
            me.canonical_name,
            (array_agg(COALESCE(t.merchant_name, t.name) ORDER BY t.date DESC))[1]
        )::text                                     AS merchant,
        SUM(t.amount)::numeric                      AS total,
        COUNT(*)::bigint                            AS transaction_count,
        (SUM(t.amount) / COUNT(*))::numeric         AS average,
        MIN(t.date)::date                           AS first_seen,
        MAX(t.date)::date                           AS last_seen,
        -- Does ANY raw form this merchant bills under match the needle? A null
        -- needle makes every row true, so bool_or passes the whole household.
        bool_or(
            sqlc.narg('search')::text IS NULL
            OR t.merchant_key ILIKE '%' || sqlc.narg('search')::text || '%'
            OR COALESCE(t.merchant_name, t.name) ILIKE '%' || sqlc.narg('search')::text || '%'
        )                                           AS descriptor_match
    FROM transactions t
    JOIN accounts a    ON a.id = t.account_id
    JOIN plaid_items i ON i.id = a.plaid_item_id
    JOIN users u       ON u.id = i.user_id
    LEFT JOIN categories c ON c.id = t.category_id
    LEFT JOIN merchant_aliases ma
           ON ma.household_id = $1
          AND ma.merchant_key = t.merchant_key
          AND ma.source <> 'suggested'
    LEFT JOIN merchant_entities me ON me.id = ma.entity_id
    WHERE u.household_id = $1
      AND (i.user_id = $2 OR i.is_shared)
      AND a.is_active
      AND NOT t.excluded_from_reports
      AND NOT t.pending
      AND t.date >= $3 AND t.date <= $4
      AND NOT COALESCE(c.is_income, FALSE)
      AND NOT COALESCE(c.is_transfer, FALSE)
      AND t.amount > 0
      -- Optional category filter. Unlike the search needle this one IS applied
      -- before aggregation, on purpose: "what do I spend on groceries at Costco"
      -- is a question about a slice of a merchant, and the answer should be the
      -- slice.
      AND (sqlc.narg('category_id')::uuid IS NULL OR t.category_id = sqlc.narg('category_id')::uuid)
    GROUP BY me.canonical_name, 1
),
prior_spend AS (
    -- The equivalent window immediately before this one, for the change column.
    -- Same filters throughout, so a change of +18% compares like with like.
    SELECT
        COALESCE(ma.entity_id::text, t.merchant_key, '')::text AS merchant_key,
        SUM(t.amount)::numeric                                 AS total
    FROM transactions t
    JOIN accounts a    ON a.id = t.account_id
    JOIN plaid_items i ON i.id = a.plaid_item_id
    JOIN users u       ON u.id = i.user_id
    LEFT JOIN categories c ON c.id = t.category_id
    LEFT JOIN merchant_aliases ma
           ON ma.household_id = $1
          AND ma.merchant_key = t.merchant_key
          AND ma.source <> 'suggested'
    WHERE u.household_id = $1
      AND (i.user_id = $2 OR i.is_shared)
      AND a.is_active
      AND NOT t.excluded_from_reports
      AND NOT t.pending
      AND t.date >= sqlc.arg('prior_from') AND t.date <= sqlc.arg('prior_to')
      AND NOT COALESCE(c.is_income, FALSE)
      AND NOT COALESCE(c.is_transfer, FALSE)
      AND t.amount > 0
      AND (sqlc.narg('category_id')::uuid IS NULL OR t.category_id = sqlc.narg('category_id')::uuid)
    GROUP BY 1
),
seen_before AS (
    -- Merchants that existed before the window at all, so "new" means genuinely
    -- first-time rather than merely absent from the previous period. Unbounded
    -- backwards on purpose: a merchant last charged three years ago is not new.
    SELECT DISTINCT COALESCE(ma.entity_id::text, t.merchant_key, '')::text AS merchant_key
    FROM transactions t
    JOIN accounts a    ON a.id = t.account_id
    JOIN plaid_items i ON i.id = a.plaid_item_id
    JOIN users u       ON u.id = i.user_id
    LEFT JOIN categories c ON c.id = t.category_id
    LEFT JOIN merchant_aliases ma
           ON ma.household_id = $1
          AND ma.merchant_key = t.merchant_key
          AND ma.source <> 'suggested'
    WHERE u.household_id = $1
      AND (i.user_id = $2 OR i.is_shared)
      AND a.is_active
      AND NOT t.excluded_from_reports
      AND NOT t.pending
      AND t.date < $3
      AND NOT COALESCE(c.is_income, FALSE)
      AND NOT COALESCE(c.is_transfer, FALSE)
      AND t.amount > 0
),
merchant_category AS (
    -- Spend per (merchant, category) in the window. Inner join to categories, so
    -- an uncategorised merchant simply has no chip rather than a fake one.
    SELECT
        COALESCE(ma.entity_id::text, t.merchant_key, '')::text AS merchant_key,
        c.id                   AS category_id,
        c.name                 AS category_name,
        c.color                AS category_color,
        SUM(t.amount)          AS total
    FROM transactions t
    JOIN accounts a    ON a.id = t.account_id
    JOIN plaid_items i ON i.id = a.plaid_item_id
    JOIN users u       ON u.id = i.user_id
    JOIN categories c  ON c.id = t.category_id
    LEFT JOIN merchant_aliases ma
           ON ma.household_id = $1
          AND ma.merchant_key = t.merchant_key
          AND ma.source <> 'suggested'
    WHERE u.household_id = $1
      AND (i.user_id = $2 OR i.is_shared)
      AND a.is_active
      AND NOT t.excluded_from_reports
      AND NOT t.pending
      AND t.date >= $3 AND t.date <= $4
      AND NOT c.is_income
      AND NOT c.is_transfer
      AND t.amount > 0
      AND (sqlc.narg('category_id')::uuid IS NULL OR t.category_id = sqlc.narg('category_id')::uuid)
    GROUP BY 1, c.id, c.name, c.color
),
top_category AS (
    SELECT DISTINCT ON (merchant_key)
        merchant_key, category_id, category_name, category_color
    FROM merchant_category
    ORDER BY merchant_key, total DESC
)
SELECT
    w.merchant_key,
    w.merchant,
    w.total,
    w.transaction_count,
    w.average,
    w.first_seen,
    w.last_seen,
    COALESCE(p.total, 0)::numeric      AS prior_total,
    (b.merchant_key IS NULL)::boolean  AS is_new,
    tc.category_id,
    tc.category_name,
    tc.category_color,
    -- The concentration denominator: everything spent in the window, unaffected
    -- by the search needle so "your top 10 are 43% of spending" stays true while
    -- the user is typing. Constant across rows; the handler reads it once.
    (SELECT COALESCE(SUM(total), 0) FROM window_spend)::numeric AS window_total,
    -- How many merchants matched, so the caller can report an honest count even
    -- when the row limit clips the list.
    COUNT(*) OVER ()::bigint           AS matched_count
FROM window_spend w
LEFT JOIN prior_spend p  ON p.merchant_key = w.merchant_key
LEFT JOIN seen_before b  ON b.merchant_key = w.merchant_key
LEFT JOIN top_category tc ON tc.merchant_key = w.merchant_key
WHERE w.descriptor_match
   OR w.merchant ILIKE '%' || sqlc.narg('search')::text || '%'
ORDER BY w.total DESC
LIMIT sqlc.arg('lim');

-- name: GetRecurringMerchants :many
-- Heuristic subscription/recurring detection: a merchant that recurs on a
-- roughly regular cadence, anywhere from weekly to annual. No AI — just the
-- shape of the history. Same spend definition and visibility scoping as every
-- other report. COALESCE wraps the averaged columns so they are non-null Go
-- types — the WHERE already guarantees a value.
--
-- This query is the WHOLE definition of "recurring". It used to return
-- candidates that each caller then filtered in Go, and the copies drifted: the
-- Spending table dropped anything quiet for 45 days while obligation promotion
-- used 75, so a merchant quiet for 46-75 days appeared on the Schedule page and
-- nowhere else. Both the cadence test and the gone-quiet test now live here, so
-- there is nothing left for a caller to disagree about.
--
-- A merchant the household has explicitly marked "not recurring" is excluded
-- outright via recurring_overrides, so every consumer of this query (report
-- table, insight producers, recap, chat) honours the suppression at once.
--
-- The `lapsed` narg flips the gone-quiet test at the bottom instead of adding a
-- second query. NULL/false gives the live subscriptions every existing caller
-- wants; true gives their exact complement — merchants that pass the same
-- cadence bands but have stopped billing, i.e. the forgotten cancellations. Two
-- separate queries would mean two copies of the calibration below, and the last
-- time this logic was duplicated the copies drifted (see above). One definition,
-- one flag, and the two lists can never disagree about what "recurring" means.
--
-- Charges are grouped by RESOLVED merchant (merchants.sql), which is the whole
-- point of canonicalisation: a subscription billing under two descriptors never
-- reached the n >= 3 threshold before, and now does. The returned merchant_key
-- is likewise the resolved key — an entity UUID for a merged merchant, the raw
-- key otherwise — so a caller that stores it (a suppression, a promoted
-- obligation) gets an identifier that survives further aliases joining the
-- entity. Suppressions are matched on the resolved key at both ends, so an
-- override recorded against one raw descriptor silences the whole merchant.
WITH tx AS (
    SELECT
        COALESCE(ma.entity_id::text, t.merchant_key) AS merchant_key,
        COALESCE(me.canonical_name, t.merchant_name, t.name) AS merchant,
        t.date,
        t.amount
    FROM transactions t
    JOIN accounts a    ON a.id = t.account_id
    JOIN plaid_items i ON i.id = a.plaid_item_id
    JOIN users u       ON u.id = i.user_id
    LEFT JOIN categories c ON c.id = t.category_id
    LEFT JOIN merchant_aliases ma
           ON ma.household_id = $1
          AND ma.merchant_key = t.merchant_key
          AND ma.source <> 'suggested'
    LEFT JOIN merchant_entities me ON me.id = ma.entity_id
    WHERE u.household_id = $1
      AND (i.user_id = $2 OR i.is_shared)
      AND a.is_active
      AND NOT t.excluded_from_reports
      AND NOT t.pending
      AND t.merchant_key IS NOT NULL
      AND t.amount > 0
      AND NOT COALESCE(c.is_income, FALSE)
      AND NOT COALESCE(c.is_transfer, FALSE)
      -- Discretionary spend is never a subscription. Eating at the same place
      -- on a roughly regular rhythm is a habit, not a bill, but the cadence
      -- test cannot tell the two apart: a fast-food merchant visited a dozen
      -- times a year passes every threshold below and lands on the calendar as
      -- a monthly charge. Before this the only remedy was a manual "not
      -- recurring" per merchant, one restaurant at a time.
      AND COALESCE(c.slug, '') NOT IN ('food-and-drink', 'groceries')
      AND t.date >= $3
      AND NOT EXISTS (
          SELECT 1 FROM recurring_overrides ro
          LEFT JOIN merchant_aliases roa
                 ON roa.household_id = ro.household_id
                AND roa.merchant_key = ro.merchant_key
                AND roa.source <> 'suggested'
          WHERE ro.household_id = $1
            AND COALESCE(roa.entity_id::text, ro.merchant_key)
                = COALESCE(ma.entity_id::text, t.merchant_key)
      )
),
gaps AS (
    SELECT
        merchant_key,
        merchant,
        amount,
        date,
        date - LAG(date) OVER (PARTITION BY merchant_key ORDER BY date) AS gap
    FROM tx
),
agg AS (
    SELECT
        merchant_key,
        COALESCE(MAX(merchant), '')::text                          AS merchant,
        COUNT(*)                                                   AS n,
        AVG(amount)                                                AS avg_amount,
        MIN(amount)                                                AS min_amount,
        MAX(amount)                                                AS max_amount,
        MAX(date)                                                  AS last_seen,
        MIN(date)                                                  AS first_seen,
        ($4::date - MAX(date))                                     AS days_quiet,
        AVG(gap) FILTER (WHERE gap IS NOT NULL)                    AS avg_gap,
        COALESCE(STDDEV_POP(gap) FILTER (WHERE gap IS NOT NULL), 0) AS gap_stddev
    FROM gaps
    GROUP BY merchant_key
)
SELECT
    merchant_key::text AS merchant_key,
    merchant,
    n::bigint                       AS occurrences,
    COALESCE(avg_amount, 0)::numeric AS average_amount,
    first_seen::date                AS first_seen,
    last_seen::date                 AS last_seen,
    COALESCE(avg_gap, 0)::numeric    AS avg_gap_days,
    days_quiet::int                 AS days_quiet
FROM agg
WHERE avg_gap IS NOT NULL
  -- Three cadence bands. Each carries its own minimum span between the first and
  -- last charge, because a real subscription persists across cycles and a
  -- coincidental cluster does not — and how long "persists" has to be depends
  -- entirely on how long the cycle is.
  AND (
      -- Weekly to monthly. This band was the only one that existed, and it is
      -- well-calibrated, so it is unchanged. 45 days clears a 3-charge monthly
      -- subscription (~60-day span) while dropping a short burst at one
      -- merchant.
         (n >= 3 AND avg_gap BETWEEN 6 AND 40
              AND gap_stddev <= avg_gap * 0.50
              AND (last_seen - first_seen) >= 45)
      -- Bi-monthly through annual. Two extra demands over the band above, both
      -- because coincidence is much easier to mistake for a cycle when the cycle
      -- is long. First, a fifth of the spread: three shopping trips a hundred
      -- days apart are chance, whereas a quarterly utility bill lands within a
      -- few days of its date every single time. Second, half a year of history,
      -- which is what separates a bill from a pair of visits that happened to
      -- fall two months apart — and, not incidentally, what stops a subscription
      -- split across two ALTERNATING descriptors from reading as two genuine
      -- bi-monthly charges before the descriptors are merged.
      OR (n >= 3 AND avg_gap BETWEEN 41 AND 400
              AND gap_stddev <= avg_gap * 0.20
              AND (last_seen - first_seen) >= 180)
      -- An annual charge can only ever be observed twice in a household with a
      -- couple of years of history, and with a single gap there is no spread to
      -- measure — gap_stddev is trivially 0 and proves nothing. Amount identity
      -- carries the whole burden instead: a renewal costs the same to the cent,
      -- two coincidental visits to the same shop do not. 2% absorbs a sales-tax
      -- or exchange-rate wobble and nothing more. With n = 2 the span IS the gap,
      -- so the 180-day floor is expressed in the band itself.
      OR (n = 2 AND avg_gap BETWEEN 180 AND 400
              AND (max_amount - min_amount) <= avg_amount * 0.02)
  )
  -- Has it gone quiet? A cancelled or paid-off charge must stop being a bill,
  -- but a flat day count cannot serve both a weekly childcare payment and an
  -- annual domain renewal — which is exactly how the callers ended up with two
  -- different numbers. Scale the tolerance to the merchant's own cadence:
  -- roughly one and a half missed cycles, never more than 90 days past the
  -- expected date, and never less than three weeks. That yields 21 days for a
  -- weekly charge, 75 for a monthly one (identical to what obligation promotion
  -- used, so the Schedule page is unchanged for the common case), 181 for
  -- quarterly and 455 for annual.
  --
  -- Written as three comparisons rather than the equivalent
  -- days_quiet <= GREATEST(LEAST(avg_gap * 2.5, avg_gap + 90), 21) because sqlc
  -- cannot resolve an aggregate alias referenced twice inside one expression.
  --
  -- With lapsed = true the same test is negated, which is what makes the two
  -- lists complements rather than two heuristics that happen to look similar.
  AND (
    CASE WHEN sqlc.narg('lapsed')::bool IS TRUE THEN
        NOT (
             days_quiet <= 21
          OR (days_quiet <= avg_gap * 2.5 AND days_quiet <= avg_gap + 90)
        )
    ELSE
             days_quiet <= 21
          OR (days_quiet <= avg_gap * 2.5 AND days_quiet <= avg_gap + 90)
    END
  )
ORDER BY COALESCE(avg_amount, 0) * (30.0 / GREATEST(avg_gap, 1)) DESC;

-- name: GetAverageSpendingTransaction :one
-- The household's typical single spending transaction over a window — the
-- baseline the "unusually large transaction" insight measures against. Same
-- spend definition and visibility scoping as every other report. The producer
-- compares individual charges (from GetLargestTransactions) to this in Go, so
-- no arithmetic the model sees happens outside SQL/decimal.
SELECT
    COALESCE(AVG(t.amount), 0)::numeric AS avg_amount,
    COUNT(*)::bigint                    AS transaction_count
FROM transactions t
JOIN accounts a    ON a.id = t.account_id
JOIN plaid_items i ON i.id = a.plaid_item_id
JOIN users u       ON u.id = i.user_id
LEFT JOIN categories c ON c.id = t.category_id
WHERE u.household_id = $1
  AND (i.user_id = $2 OR i.is_shared)
  AND a.is_active
  AND NOT t.excluded_from_reports
  AND NOT t.pending
  AND t.amount > 0
  AND NOT COALESCE(c.is_income, FALSE)
  AND NOT COALESCE(c.is_transfer, FALSE)
  AND t.date >= $3;

-- name: SuppressRecurringMerchant :exec
-- Mark a merchant "not recurring" for a household. Idempotent: re-suppressing an
-- already-suppressed merchant is a no-op (and does not disturb the label).
--
-- The key is stored resolved (merchants.sql), so suppressing a merged merchant
-- records one row against the entity rather than one per descriptor. Resolution
-- is idempotent, so a caller may pass either a raw or an already-resolved key.
INSERT INTO recurring_overrides (household_id, merchant_key, merchant_label)
VALUES (
    @household_id,
    COALESCE(
        (SELECT ma.entity_id::text FROM merchant_aliases ma
          WHERE ma.household_id = @household_id
            AND ma.merchant_key = @merchant_key::text
            AND ma.source <> 'suggested'),
        @merchant_key::text
    ),
    @merchant_label
)
ON CONFLICT (household_id, merchant_key) DO NOTHING;

-- name: UnsuppressRecurringMerchant :exec
-- Restore a merchant to the recurring detector. Clears every override that
-- resolves to the same merchant, so undoing a suppression on a merged merchant
-- also clears any per-descriptor rows recorded before the merge.
DELETE FROM recurring_overrides ro
WHERE ro.household_id = @household_id
  AND COALESCE(
        (SELECT a2.entity_id::text FROM merchant_aliases a2
          WHERE a2.household_id = ro.household_id
            AND a2.merchant_key = ro.merchant_key
            AND a2.source <> 'suggested'),
        ro.merchant_key
      ) = COALESCE(
        (SELECT a3.entity_id::text FROM merchant_aliases a3
          WHERE a3.household_id = @household_id
            AND a3.merchant_key = @merchant_key::text
            AND a3.source <> 'suggested'),
        @merchant_key::text
      );

-- name: ListRecurringOverrides :many
-- The household's suppressed merchants, for the "restore" UI.
--
-- The stored key comes from GetRecurringMerchants and is therefore already
-- resolved, but it is resolved again on the way out so a row written before a
-- later merge still addresses the merchant detail view. Resolution is idempotent
-- — a resolved key is an entity id as text and is never itself aliased — so
-- doing it twice is a no-op.
SELECT
    ro.merchant_key,
    ro.merchant_label,
    ro.created_at,
    COALESCE(ma.entity_id::text, ro.merchant_key)::text AS resolved_merchant_key
FROM recurring_overrides ro
LEFT JOIN merchant_aliases ma
       ON ma.household_id = ro.household_id
      AND ma.merchant_key = ro.merchant_key
      AND ma.source <> 'suggested'
WHERE ro.household_id = $1
ORDER BY ro.merchant_label, ro.merchant_key;

-- name: GetMerchantSpendBaseline :one
-- Typical spend at one merchant for this household, EXCLUDING the flagged
-- transaction, so "you normally spend ~$X" is a real prior rather than one
-- skewed by the charge that triggered the alert. All arithmetic stays in SQL;
-- the model only quotes the result. Same visibility scoping as every report.
--
-- Both sides of the key comparison are resolved (merchants.sql), so the prior
-- spans every descriptor of a merged merchant and the caller may pass a raw key
-- straight off a transaction.
SELECT
    COALESCE(AVG(t.amount), 0)::numeric AS typical_amount,
    COUNT(*)::bigint                    AS visit_count,
    COALESCE(MIN(t.amount), 0)::numeric AS min_amount,
    COALESCE(MAX(t.amount), 0)::numeric AS max_amount
FROM transactions t
JOIN accounts a    ON a.id = t.account_id
JOIN plaid_items i ON i.id = a.plaid_item_id
JOIN users u       ON u.id = i.user_id
LEFT JOIN merchant_aliases ma
       ON ma.household_id = @household_id
      AND ma.merchant_key = t.merchant_key
      AND ma.source <> 'suggested'
WHERE u.household_id = @household_id
  AND (i.user_id = @user_id OR i.is_shared)
  AND a.is_active
  AND NOT t.excluded_from_reports
  AND NOT t.pending
  AND COALESCE(ma.entity_id::text, t.merchant_key) = COALESCE(
        (SELECT a2.entity_id::text FROM merchant_aliases a2
          WHERE a2.household_id = @household_id
            AND a2.merchant_key = @merchant_key::text
            AND a2.source <> 'suggested'),
        @merchant_key::text
      )
  AND t.id <> @exclude_tx::uuid
  AND t.amount > 0;

-- name: GetRecurringAmountTrend :many
-- Price-creep detection for recurring merchants: split each merchant's charges
-- into an older half and a newer half by date and compare the averages. The
-- split and the difference are computed here in SQL; the caller only formats and
-- explains, never subtracts. Same tx CTE (visibility + spend filters, and the
-- same resolved grouping) as GetRecurringMerchants so both agree on what counts
-- as a charge and key their rows the same way — the caller joins the two by key.
WITH tx AS (
    SELECT
        COALESCE(ma.entity_id::text, t.merchant_key) AS merchant_key,
        COALESCE(me.canonical_name, t.merchant_name, t.name) AS merchant,
        t.date,
        t.amount
    FROM transactions t
    JOIN accounts a    ON a.id = t.account_id
    JOIN plaid_items i ON i.id = a.plaid_item_id
    JOIN users u       ON u.id = i.user_id
    LEFT JOIN categories c ON c.id = t.category_id
    LEFT JOIN merchant_aliases ma
           ON ma.household_id = $1
          AND ma.merchant_key = t.merchant_key
          AND ma.source <> 'suggested'
    LEFT JOIN merchant_entities me ON me.id = ma.entity_id
    WHERE u.household_id = $1
      AND (i.user_id = $2 OR i.is_shared)
      AND a.is_active
      AND NOT t.excluded_from_reports
      AND NOT t.pending
      AND t.merchant_key IS NOT NULL
      AND t.amount > 0
      AND NOT COALESCE(c.is_income, FALSE)
      AND NOT COALESCE(c.is_transfer, FALSE)
      AND t.date >= $3
),
ranked AS (
    SELECT
        merchant_key,
        merchant,
        amount,
        NTILE(2) OVER (PARTITION BY merchant_key ORDER BY date) AS half
    FROM tx
)
SELECT
    merchant_key::text AS merchant_key,
    COALESCE(MAX(merchant), '')::text                                      AS merchant,
    COALESCE(AVG(amount) FILTER (WHERE half = 1), 0)::numeric              AS early_avg,
    COALESCE(AVG(amount) FILTER (WHERE half = 2), 0)::numeric              AS recent_avg,
    COALESCE(
        AVG(amount) FILTER (WHERE half = 2) - AVG(amount) FILTER (WHERE half = 1),
        0
    )::numeric                                                            AS delta
FROM ranked
GROUP BY merchant_key
HAVING COUNT(*) >= 4                                    -- two charges per half
   AND AVG(amount) FILTER (WHERE half = 1) > 0
   AND (AVG(amount) FILTER (WHERE half = 2) - AVG(amount) FILTER (WHERE half = 1))
       >= AVG(amount) FILTER (WHERE half = 1) * 0.10;  -- ≥10% rise clears noise

-- name: GetBudgetProgress :many
-- Each budget alongside what has been spent against it this period, so the UI
-- can show "$X of $Y left in this category".
--
-- For rollover (envelope) budgets it also returns the spend BEFORE this period,
-- since the budget's start month, and that start month itself. The caller
-- derives the carried balance from those in decimal (amount × prior months −
-- prior spend), so the envelope math stays out of SQL but the inputs come from
-- one query. rollover_start is effective_from's month, or the month the budget
-- was created when effective_from is unset.
-- The spend window is period-aware: a monthly budget measures against the
-- selected month (@window_start/@window_end); a weekly or yearly budget always measures against
-- the current week or year of the reference date @ref, since "this week"/"this
-- year" only make sense relative to now. period_start/period_end are returned so
-- the caller can label the window.
SELECT
    b.id          AS budget_id,
    b.amount      AS budgeted,
    b.period      AS period,
    b.rollover    AS rollover,
    c.id          AS category_id,
    c.name        AS category_name,
    c.slug        AS category_slug,
    c.color       AS category_color,
    (CASE b.period
        WHEN 'weekly' THEN date_trunc('week', @ref::date)::date
        WHEN 'yearly' THEN date_trunc('year', @ref::date)::date
        ELSE @window_start::date
     END)::date AS period_start,
    (CASE b.period
        WHEN 'weekly' THEN (date_trunc('week', @ref::date) + interval '6 days')::date
        WHEN 'yearly' THEN (date_trunc('year', @ref::date) + interval '1 year' - interval '1 day')::date
        ELSE @window_end::date
     END)::date AS period_end,
    COALESCE(date_trunc('month', b.effective_from)::date,
             date_trunc('month', b.created_at)::date)::date AS rollover_start,
    COALESCE((
        SELECT SUM(t.amount)
        FROM transactions t
        JOIN accounts a    ON a.id = t.account_id
        JOIN plaid_items i ON i.id = a.plaid_item_id
        JOIN users u       ON u.id = i.user_id
        WHERE u.household_id = b.household_id
          AND (i.user_id = @user_id OR i.is_shared)
          AND a.is_active
          AND NOT t.excluded_from_reports
          AND NOT t.pending
          AND t.category_id = b.category_id
          AND t.amount > 0
          AND t.date >= (CASE b.period
                WHEN 'weekly' THEN date_trunc('week', @ref::date)::date
                WHEN 'yearly' THEN date_trunc('year', @ref::date)::date
                ELSE @window_start::date END)
          AND t.date <= (CASE b.period
                WHEN 'weekly' THEN (date_trunc('week', @ref::date) + interval '6 days')::date
                WHEN 'yearly' THEN (date_trunc('year', @ref::date) + interval '1 year' - interval '1 day')::date
                ELSE @window_end::date END)
    ), 0)::numeric AS spent,
    COALESCE((
        SELECT SUM(t.amount)
        FROM transactions t
        JOIN accounts a    ON a.id = t.account_id
        JOIN plaid_items i ON i.id = a.plaid_item_id
        JOIN users u       ON u.id = i.user_id
        WHERE u.household_id = b.household_id
          AND (i.user_id = @user_id OR i.is_shared)
          AND a.is_active
          AND NOT t.excluded_from_reports
          AND NOT t.pending
          AND t.category_id = b.category_id
          AND t.amount > 0
          AND t.date >= COALESCE(date_trunc('month', b.effective_from)::date,
                                 date_trunc('month', b.created_at)::date)
          AND t.date < @window_start::date
    ), 0)::numeric AS prior_spent
FROM budgets b
JOIN categories c ON c.id = b.category_id
WHERE b.household_id = @household_id
ORDER BY c.sort_order, c.name;

-- name: UpsertBudget :one
-- One household budget per category; setting it again updates its amount,
-- period, and rollover flag in place.
INSERT INTO budgets (household_id, category_id, amount, owner_scope, period, rollover)
VALUES (@household_id, @category_id, @amount, 'household', @period, @rollover)
ON CONFLICT (household_id, category_id, owner_scope)
    WHERE user_id IS NULL
DO UPDATE SET amount = EXCLUDED.amount, period = EXCLUDED.period,
              rollover = EXCLUDED.rollover, updated_at = now()
RETURNING *;

-- name: DeleteBudget :exec
DELETE FROM budgets WHERE id = $1 AND household_id = $2;

-- name: SumHouseholdBudgets :one
-- Total household monthly budget, split by whether the category is fixed. Feeds
-- the "safe to spend" calculation, which counts fixed categories at their actual
-- typical cost (not their budget) and discretionary categories at their budgeted
-- envelope — so the split keeps those from being double-counted.
SELECT
    COALESCE(SUM(b.amount) FILTER (WHERE c.is_fixed), 0)::numeric      AS fixed_budgeted,
    COALESCE(SUM(b.amount) FILTER (WHERE NOT c.is_fixed), 0)::numeric  AS discretionary_budgeted
FROM budgets b
JOIN categories c ON c.id = b.category_id
WHERE b.household_id = $1
  AND b.owner_scope = 'household'
  AND b.period = 'monthly';

-- name: ExportTransactions :many
-- Every visible transaction in a window, flattened for CSV. Includes the
-- transfer/income flags so a spreadsheet can reproduce the app's totals rather
-- than having to guess which rows to exclude.
SELECT
    t.date,
    t.name,
    t.merchant_name,
    t.amount,
    t.currency,
    a.name AS account_name,
    i.institution_name,
    c.name AS category_name,
    COALESCE(c.is_transfer, FALSE) AS is_transfer,
    COALESCE(c.is_income, FALSE)   AS is_income
FROM transactions t
JOIN accounts a    ON a.id = t.account_id
JOIN plaid_items i ON i.id = a.plaid_item_id
JOIN users u       ON u.id = i.user_id
LEFT JOIN categories c ON c.id = t.category_id
WHERE u.household_id = $1
  AND (i.user_id = $2 OR i.is_shared)
  AND a.is_active
  AND NOT t.excluded_from_reports
  AND NOT t.pending
  AND t.date >= $3 AND t.date <= $4
ORDER BY t.date DESC, t.created_at DESC;

-- name: GetLargestTransactions :many
-- The single biggest purchases in a window, largest first. Feeds the monthly
-- recap ("your biggest hits were …"). Same spend definition and visibility
-- scoping as every other report: money out (amount > 0), no income, no
-- transfers. Merchant falls back to the raw transaction name when Plaid has no
-- cleaned merchant.
SELECT
    COALESCE(t.merchant_name, t.name) AS merchant,
    t.amount::numeric                 AS amount,
    t.date::date                      AS date,
    COALESCE(c.name, '')              AS category_name
FROM transactions t
JOIN accounts a    ON a.id = t.account_id
JOIN plaid_items i ON i.id = a.plaid_item_id
JOIN users u       ON u.id = i.user_id
LEFT JOIN categories c ON c.id = t.category_id
WHERE u.household_id = $1
  AND (i.user_id = $2 OR i.is_shared)
  AND a.is_active
  AND NOT t.excluded_from_reports
  AND NOT t.pending
  AND t.date >= $3 AND t.date <= $4
  AND NOT COALESCE(c.is_income, FALSE)
  AND NOT COALESCE(c.is_transfer, FALSE)
  AND t.amount > 0
ORDER BY t.amount DESC
LIMIT $5;

-- --------------------------------------------------------------------------
-- Merchant detail
-- --------------------------------------------------------------------------
--
-- Everything below addresses ONE merchant by its resolved key — the same
-- identifier GetTopMerchants returns and GetRecurringMerchants groups by, i.e.
-- an entity UUID as text for a merged merchant and the raw descriptor for
-- everything else. Keying on the resolved form is what lets the detail view work
-- for the merchants a household has never grouped, which is most of them and
-- most of their spending.
--
-- All four apply the same spending filter the rest of the reporting layer uses
-- (active accounts, not excluded, not pending, not income, not a transfer,
-- outflow only), so the totals here reconcile with the Spending page rather than
-- telling a second story about the same money.

-- name: GetMerchantSummary :one
-- The headline numbers. Returns a row of zeroes and NULLs when the key matches
-- nothing, so a stale link renders an empty merchant rather than a 500.
SELECT
    COALESCE(SUM(t.amount), 0)::numeric  AS total,
    COUNT(*)::bigint                     AS transaction_count,
    COALESCE(AVG(t.amount), 0)::numeric  AS average,
    COALESCE(MAX(t.amount), 0)::numeric  AS largest,
    COALESCE(MIN(t.date)::text, '')::text  AS first_seen,
    COALESCE(MAX(t.date)::text, '')::text AS last_seen
FROM transactions t
JOIN accounts a    ON a.id = t.account_id
JOIN plaid_items i ON i.id = a.plaid_item_id
JOIN users u       ON u.id = i.user_id
LEFT JOIN categories c ON c.id = t.category_id
LEFT JOIN merchant_aliases ma
       ON ma.household_id = $1
      AND ma.merchant_key = t.merchant_key
      AND ma.source <> 'suggested'
WHERE u.household_id = $1
  AND (i.user_id = $2 OR i.is_shared)
  AND a.is_active
  AND NOT t.excluded_from_reports
  AND NOT t.pending
  AND t.date >= $3 AND t.date <= $4
  AND NOT COALESCE(c.is_income, FALSE)
  AND NOT COALESCE(c.is_transfer, FALSE)
  AND t.amount > 0
  AND COALESCE(ma.entity_id::text, t.merchant_key) = sqlc.arg('resolved_key')::text;

-- name: GetMerchantMonthlySpend :many
-- Spend per calendar month. Gaps are left out rather than zero-filled: the
-- caller knows the requested range and can fill it, and inventing rows here
-- would make an empty month indistinguishable from a month outside the data.
SELECT
    date_trunc('month', t.date)::date AS month,
    SUM(t.amount)::numeric            AS total,
    COUNT(*)::bigint                  AS transaction_count
FROM transactions t
JOIN accounts a    ON a.id = t.account_id
JOIN plaid_items i ON i.id = a.plaid_item_id
JOIN users u       ON u.id = i.user_id
LEFT JOIN categories c ON c.id = t.category_id
LEFT JOIN merchant_aliases ma
       ON ma.household_id = $1
      AND ma.merchant_key = t.merchant_key
      AND ma.source <> 'suggested'
WHERE u.household_id = $1
  AND (i.user_id = $2 OR i.is_shared)
  AND a.is_active
  AND NOT t.excluded_from_reports
  AND NOT t.pending
  AND t.date >= $3 AND t.date <= $4
  AND NOT COALESCE(c.is_income, FALSE)
  AND NOT COALESCE(c.is_transfer, FALSE)
  AND t.amount > 0
  AND COALESCE(ma.entity_id::text, t.merchant_key) = sqlc.arg('resolved_key')::text
GROUP BY 1
ORDER BY 1;

-- name: GetMerchantCategoryBreakdown :many
-- How this merchant's spending is filed. Shaped to match the category-spend rows
-- the CategoryBars chart already consumes, so the frontend reuses that component
-- rather than growing a second one.
SELECT
    c.id                   AS category_id,
    c.name                 AS category_name,
    c.slug                 AS category_slug,
    c.color                AS category_color,
    c.is_fixed,
    SUM(t.amount)::numeric AS total,
    COUNT(*)::bigint       AS transaction_count
FROM transactions t
JOIN accounts a    ON a.id = t.account_id
JOIN plaid_items i ON i.id = a.plaid_item_id
JOIN users u       ON u.id = i.user_id
JOIN categories c  ON c.id = t.category_id
LEFT JOIN merchant_aliases ma
       ON ma.household_id = $1
      AND ma.merchant_key = t.merchant_key
      AND ma.source <> 'suggested'
WHERE u.household_id = $1
  AND (i.user_id = $2 OR i.is_shared)
  AND a.is_active
  AND NOT t.excluded_from_reports
  AND NOT t.pending
  AND t.date >= $3 AND t.date <= $4
  AND NOT c.is_income
  AND NOT c.is_transfer
  AND t.amount > 0
  AND COALESCE(ma.entity_id::text, t.merchant_key) = sqlc.arg('resolved_key')::text
GROUP BY c.id, c.name, c.slug, c.color, c.is_fixed
ORDER BY total DESC;

-- name: ListMerchantTransactions :many
-- The charges behind the numbers above. Returns the raw descriptor per row as
-- well as the account, because on a merged merchant the interesting question is
-- often WHICH fragment a given charge came in under.
SELECT
    t.id,
    t.date,
    t.amount,
    t.name,
    COALESCE(t.merchant_name, t.name) AS descriptor,
    t.merchant_key                    AS raw_merchant_key,
    a.name                            AS account_name,
    c.name                            AS category_name,
    c.id                              AS category_id
FROM transactions t
JOIN accounts a    ON a.id = t.account_id
JOIN plaid_items i ON i.id = a.plaid_item_id
JOIN users u       ON u.id = i.user_id
LEFT JOIN categories c ON c.id = t.category_id
LEFT JOIN merchant_aliases ma
       ON ma.household_id = $1
      AND ma.merchant_key = t.merchant_key
      AND ma.source <> 'suggested'
WHERE u.household_id = $1
  AND (i.user_id = $2 OR i.is_shared)
  AND a.is_active
  AND NOT t.excluded_from_reports
  AND NOT t.pending
  AND t.date >= $3 AND t.date <= $4
  AND NOT COALESCE(c.is_income, FALSE)
  AND NOT COALESCE(c.is_transfer, FALSE)
  AND t.amount > 0
  AND COALESCE(ma.entity_id::text, t.merchant_key) = sqlc.arg('resolved_key')::text
ORDER BY t.date DESC, t.amount DESC
LIMIT sqlc.arg('lim');

-- name: GetMerchantIdentity :one
-- The merchant's display name and the descriptors that resolve to it.
--
-- Resolved from the transaction side rather than by looking up the entity,
-- because a resolved key is just as likely to be a bare descriptor with no entity
-- behind it — and that case still has a name and exactly one descriptor.
SELECT
    COALESCE(me.canonical_name, MIN(t.merchant_name), MIN(t.name))  AS merchant,
    (me.id IS NOT NULL)::boolean                                   AS is_grouped,
    ARRAY_AGG(DISTINCT t.merchant_key)::text[]                     AS descriptors
FROM transactions t
JOIN accounts a    ON a.id = t.account_id
JOIN plaid_items i ON i.id = a.plaid_item_id
JOIN users u       ON u.id = i.user_id
LEFT JOIN merchant_aliases ma
       ON ma.household_id = $1
      AND ma.merchant_key = t.merchant_key
      AND ma.source <> 'suggested'
LEFT JOIN merchant_entities me ON me.id = ma.entity_id
WHERE u.household_id = $1
  AND (i.user_id = $2 OR i.is_shared)
  AND a.is_active
  AND NOT t.pending
  AND COALESCE(ma.entity_id::text, t.merchant_key) = sqlc.arg('resolved_key')::text
GROUP BY me.id, me.canonical_name;

-- --------------------------------------------------------------------------
-- Category detail
-- --------------------------------------------------------------------------
--
-- The category counterpart of the merchant detail block above, addressed by
-- category id. Every category click in the app used to land in a filtered
-- transaction list, which answers "which charges" but never "how much, how
-- often, trending which way, and to whom" — so these four exist to answer the
-- same questions about a category that the block above answers about a merchant.
--
-- They carry the identical spending filter as the rest of the reporting layer
-- (active accounts, not excluded, not pending, not income, not a transfer,
-- outflow only), so a category's headline total equals its row in
-- GetSpendingByCategory rather than telling a second story about the same money.
--
-- There is no GetCategoryIdentity: unlike a merchant, a category always has a
-- row of its own, so the name, colour and flags come from ListCategories.

-- name: GetCategorySummary :one
-- The headline numbers. Returns a row of zeroes and empty strings when the
-- category matches nothing in the window, so an empty category renders as empty
-- rather than a 500 — same contract as GetMerchantSummary.
SELECT
    COALESCE(SUM(t.amount), 0)::numeric    AS total,
    COUNT(*)::bigint                       AS transaction_count,
    COALESCE(AVG(t.amount), 0)::numeric    AS average,
    COALESCE(MAX(t.amount), 0)::numeric    AS largest,
    COALESCE(MIN(t.date)::text, '')::text  AS first_seen,
    COALESCE(MAX(t.date)::text, '')::text  AS last_seen
FROM transactions t
JOIN accounts a    ON a.id = t.account_id
JOIN plaid_items i ON i.id = a.plaid_item_id
JOIN users u       ON u.id = i.user_id
LEFT JOIN categories c ON c.id = t.category_id
WHERE u.household_id = $1
  AND (i.user_id = $2 OR i.is_shared)
  AND a.is_active
  AND NOT t.excluded_from_reports
  AND NOT t.pending
  AND t.date >= $3 AND t.date <= $4
  AND NOT COALESCE(c.is_income, FALSE)
  AND NOT COALESCE(c.is_transfer, FALSE)
  AND t.amount > 0
  AND t.category_id = sqlc.arg('category_id');

-- name: GetCategoryMonthlySpend :many
-- Spend per calendar month in one category. Gaps are left out rather than
-- zero-filled, matching GetMerchantMonthlySpend so both feed MonthlyBars, which
-- re-expands the range itself.
--
-- This replaces the workaround in internal/insights/budgettrend.go, which built
-- the same series by calling GetSpendingByCategory once per month in Go.
SELECT
    date_trunc('month', t.date)::date AS month,
    SUM(t.amount)::numeric            AS total,
    COUNT(*)::bigint                  AS transaction_count
FROM transactions t
JOIN accounts a    ON a.id = t.account_id
JOIN plaid_items i ON i.id = a.plaid_item_id
JOIN users u       ON u.id = i.user_id
LEFT JOIN categories c ON c.id = t.category_id
WHERE u.household_id = $1
  AND (i.user_id = $2 OR i.is_shared)
  AND a.is_active
  AND NOT t.excluded_from_reports
  AND NOT t.pending
  AND t.date >= $3 AND t.date <= $4
  AND NOT COALESCE(c.is_income, FALSE)
  AND NOT COALESCE(c.is_transfer, FALSE)
  AND t.amount > 0
  AND t.category_id = sqlc.arg('category_id')
GROUP BY 1
ORDER BY 1;

-- name: GetTopMerchantsInCategory :many
-- Who the money in this category actually goes to.
--
-- Canonicalised exactly as GetTopMerchants is — grouped by RESOLVED key, named
-- by the entity's canonical name or the most recent descriptor — so every row
-- links straight to the merchant detail view and a merchant billing under two
-- descriptors appears once. That mutual navigability is the point: a category
-- page whose merchants were dead text would answer "how much" and then stop.
SELECT
    COALESCE(
        me.canonical_name,
        (array_agg(COALESCE(t.merchant_name, t.name) ORDER BY t.date DESC))[1]
    )::text                                                AS merchant,
    COALESCE(ma.entity_id::text, t.merchant_key, '')::text AS merchant_key,
    SUM(t.amount)::numeric                                 AS total,
    COUNT(*)::bigint                                       AS transaction_count
FROM transactions t
JOIN accounts a    ON a.id = t.account_id
JOIN plaid_items i ON i.id = a.plaid_item_id
JOIN users u       ON u.id = i.user_id
LEFT JOIN categories c ON c.id = t.category_id
LEFT JOIN merchant_aliases ma
       ON ma.household_id = $1
      AND ma.merchant_key = t.merchant_key
      AND ma.source <> 'suggested'
LEFT JOIN merchant_entities me ON me.id = ma.entity_id
WHERE u.household_id = $1
  AND (i.user_id = $2 OR i.is_shared)
  AND a.is_active
  AND NOT t.excluded_from_reports
  AND NOT t.pending
  AND t.date >= $3 AND t.date <= $4
  AND NOT COALESCE(c.is_income, FALSE)
  AND NOT COALESCE(c.is_transfer, FALSE)
  AND t.amount > 0
  AND t.category_id = sqlc.arg('category_id')
GROUP BY me.canonical_name, 2
ORDER BY total DESC
LIMIT sqlc.arg('lim');

-- name: ListCategoryTransactions :many
-- The charges behind the numbers above. Carries the RESOLVED merchant key per
-- row so each charge can link to its merchant, which is the reverse of what
-- ListMerchantTransactions does with categories.
SELECT
    t.id,
    t.date,
    t.amount,
    t.name,
    COALESCE(t.merchant_name, t.name)                      AS descriptor,
    COALESCE(ma.entity_id::text, t.merchant_key, '')::text AS resolved_merchant_key,
    COALESCE(me.canonical_name, t.merchant_name, t.name)   AS merchant,
    a.name                                                 AS account_name
FROM transactions t
JOIN accounts a    ON a.id = t.account_id
JOIN plaid_items i ON i.id = a.plaid_item_id
JOIN users u       ON u.id = i.user_id
LEFT JOIN categories c ON c.id = t.category_id
LEFT JOIN merchant_aliases ma
       ON ma.household_id = $1
      AND ma.merchant_key = t.merchant_key
      AND ma.source <> 'suggested'
LEFT JOIN merchant_entities me ON me.id = ma.entity_id
WHERE u.household_id = $1
  AND (i.user_id = $2 OR i.is_shared)
  AND a.is_active
  AND NOT t.excluded_from_reports
  AND NOT t.pending
  AND t.date >= $3 AND t.date <= $4
  AND NOT COALESCE(c.is_income, FALSE)
  AND NOT COALESCE(c.is_transfer, FALSE)
  AND t.amount > 0
  AND t.category_id = sqlc.arg('category_id')
ORDER BY t.date DESC, t.amount DESC
LIMIT sqlc.arg('lim');
