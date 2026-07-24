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
SELECT
    COALESCE(t.merchant_name, t.name) AS merchant,
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
GROUP BY 1
ORDER BY total DESC
LIMIT $5;

-- name: GetRecurringMerchants :many
-- Heuristic subscription/recurring detection: a merchant that recurs on a
-- roughly regular weekly-to-monthly cadence. No AI — just the shape of the
-- history. Same spend definition and visibility scoping as every other report.
--
-- A merchant qualifies when it has at least three charges over the window, the
-- average gap between them is weekly-to-monthly, those gaps are fairly regular
-- (low spread relative to the mean), and the charges span enough time that a
-- short coincidental burst does not look like a subscription (see the minimum
-- span below). COALESCE wraps the averaged columns so they are non-null Go types
-- — the WHERE already guarantees a value.
--
-- A merchant the household has explicitly marked "not recurring" is excluded
-- outright via recurring_overrides, so every consumer of this query (report
-- table, insight producers, recap, chat) honours the suppression at once.
WITH tx AS (
    SELECT
        t.merchant_key,
        COALESCE(t.merchant_name, t.name) AS merchant,
        t.date,
        t.amount
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
      AND t.merchant_key IS NOT NULL
      AND t.amount > 0
      AND NOT COALESCE(c.is_income, FALSE)
      AND NOT COALESCE(c.is_transfer, FALSE)
      AND t.date >= $3
      AND NOT EXISTS (
          SELECT 1 FROM recurring_overrides ro
          WHERE ro.household_id = $1
            AND ro.merchant_key = t.merchant_key
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
        MAX(date)                                                  AS last_seen,
        MIN(date)                                                  AS first_seen,
        AVG(gap) FILTER (WHERE gap IS NOT NULL)                    AS avg_gap,
        COALESCE(STDDEV_POP(gap) FILTER (WHERE gap IS NOT NULL), 0) AS gap_stddev
    FROM gaps
    GROUP BY merchant_key
)
SELECT
    merchant_key,
    merchant,
    n::bigint                       AS occurrences,
    COALESCE(avg_amount, 0)::numeric AS average_amount,
    last_seen::date                 AS last_seen,
    COALESCE(avg_gap, 0)::numeric    AS avg_gap_days
FROM agg
WHERE n >= 3
  AND avg_gap IS NOT NULL
  AND avg_gap BETWEEN 6 AND 40
  AND gap_stddev <= avg_gap * 0.5
  -- Minimum span between first and last charge (days). A real subscription
  -- persists across cycles; a coincidental cluster of a few charges within a
  -- few weeks does not. 45 days clears a 3-charge monthly subscription (~60-day
  -- span) while dropping a short burst at one merchant.
  AND (last_seen - first_seen) >= 45
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
INSERT INTO recurring_overrides (household_id, merchant_key, merchant_label)
VALUES ($1, $2, $3)
ON CONFLICT (household_id, merchant_key) DO NOTHING;

-- name: UnsuppressRecurringMerchant :exec
-- Restore a merchant to the recurring detector.
DELETE FROM recurring_overrides
WHERE household_id = $1 AND merchant_key = $2;

-- name: ListRecurringOverrides :many
-- The household's suppressed merchants, for the "restore" UI.
SELECT merchant_key, merchant_label, created_at
FROM recurring_overrides
WHERE household_id = $1
ORDER BY merchant_label, merchant_key;

-- name: GetMerchantSpendBaseline :one
-- Typical spend at one merchant for this household, EXCLUDING the flagged
-- transaction, so "you normally spend ~$X" is a real prior rather than one
-- skewed by the charge that triggered the alert. All arithmetic stays in SQL;
-- the model only quotes the result. Same visibility scoping as every report.
SELECT
    COALESCE(AVG(t.amount), 0)::numeric AS typical_amount,
    COUNT(*)::bigint                    AS visit_count,
    COALESCE(MIN(t.amount), 0)::numeric AS min_amount,
    COALESCE(MAX(t.amount), 0)::numeric AS max_amount
FROM transactions t
JOIN accounts a    ON a.id = t.account_id
JOIN plaid_items i ON i.id = a.plaid_item_id
JOIN users u       ON u.id = i.user_id
WHERE u.household_id = @household_id
  AND (i.user_id = @user_id OR i.is_shared)
  AND a.is_active
  AND NOT t.excluded_from_reports
  AND NOT t.pending
  AND t.merchant_key = @merchant_key::text
  AND t.id <> @exclude_tx::uuid
  AND t.amount > 0;

-- name: GetRecurringAmountTrend :many
-- Price-creep detection for recurring merchants: split each merchant's charges
-- into an older half and a newer half by date and compare the averages. The
-- split and the difference are computed here in SQL; the caller only formats and
-- explains, never subtracts. Same tx CTE (visibility + spend filters) as
-- GetRecurringMerchants so both agree on what counts as a charge.
WITH tx AS (
    SELECT
        t.merchant_key,
        COALESCE(t.merchant_name, t.name) AS merchant,
        t.date,
        t.amount
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
    merchant_key,
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
SELECT
    b.id          AS budget_id,
    b.amount      AS budgeted,
    c.id          AS category_id,
    c.name        AS category_name,
    c.slug        AS category_slug,
    c.color       AS category_color,
    COALESCE((
        SELECT SUM(t.amount)
        FROM transactions t
        JOIN accounts a    ON a.id = t.account_id
        JOIN plaid_items i ON i.id = a.plaid_item_id
        JOIN users u       ON u.id = i.user_id
        WHERE u.household_id = b.household_id
          AND (i.user_id = $2 OR i.is_shared)
          AND a.is_active
          AND NOT t.excluded_from_reports
          AND NOT t.pending
          AND t.category_id = b.category_id
          AND t.amount > 0
          AND t.date >= $3 AND t.date <= $4
    ), 0)::numeric AS spent
FROM budgets b
JOIN categories c ON c.id = b.category_id
WHERE b.household_id = $1
ORDER BY c.sort_order, c.name;

-- name: UpsertBudget :one
-- One monthly budget per category per household; setting it again updates it.
INSERT INTO budgets (household_id, category_id, amount, owner_scope, period)
VALUES ($1, $2, $3, 'household', 'monthly')
ON CONFLICT (household_id, category_id, owner_scope, period)
    WHERE user_id IS NULL
DO UPDATE SET amount = EXCLUDED.amount
RETURNING *;

-- name: DeleteBudget :exec
DELETE FROM budgets WHERE id = $1 AND household_id = $2;

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
