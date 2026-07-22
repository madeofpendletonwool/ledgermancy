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
-- average gap between them is weekly-to-monthly, and those gaps are fairly
-- regular (low spread relative to the mean). COALESCE wraps the averaged
-- columns so they are non-null Go types — the WHERE already guarantees a value.
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
gaps AS (
    SELECT
        merchant_key,
        merchant,
        amount,
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
ORDER BY COALESCE(avg_amount, 0) * (30.0 / GREATEST(avg_gap, 1)) DESC;

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
