-- Alerts: household-configured rules and the events they raise.
--
-- Visibility model (this is load-bearing — getting it wrong leaks a partner's
-- private spending):
--
--   * Transaction-linked alerts (big_spend, unusual_merchant) are evaluated
--     over the whole household, but each event references its transaction. On
--     read, an event is only shown to a user who may see that transaction —
--     their own item, or a shared one. So a big spend on a private account
--     raises an event only its owner ever sees.
--   * Aggregate alerts (budget_threshold, low_leftover) are evaluated over
--     SHARED items only, so a private account can never move a shared-household
--     figure. Their events carry no transaction and are visible to everyone in
--     the household.
--
-- Money is compared and summed here in NUMERIC, never in the application.

-- name: ListAlerts :many
SELECT * FROM alerts WHERE household_id = $1 ORDER BY type;

-- name: ListEnabledAlerts :many
SELECT * FROM alerts WHERE household_id = $1 AND enabled ORDER BY type;

-- name: GetAlert :one
SELECT * FROM alerts WHERE id = $1 AND household_id = $2;

-- name: CreateAlert :one
INSERT INTO alerts (household_id, type, config, enabled)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: UpdateAlert :one
UPDATE alerts
SET config = $3, enabled = $4, updated_at = now()
WHERE id = $1 AND household_id = $2
RETURNING *;

-- name: DeleteAlert :exec
DELETE FROM alerts WHERE id = $1 AND household_id = $2;

-- --------------------------------------------------------------------------
-- Evaluation candidates
-- --------------------------------------------------------------------------

-- name: BigSpendCandidates :many
-- Single spend transactions at or above the threshold, within a recent window
-- (so a two-year backfill does not raise a flood of historical alerts), that
-- this alert has not already fired on.
SELECT t.id, t.amount, t.date,
       COALESCE(t.merchant_name, t.name) AS merchant
FROM transactions t
JOIN accounts a    ON a.id = t.account_id
JOIN plaid_items i ON i.id = a.plaid_item_id
JOIN users u       ON u.id = i.user_id
LEFT JOIN categories c ON c.id = t.category_id
WHERE u.household_id = $1
  AND a.is_active
  AND NOT t.excluded_from_reports
  AND NOT t.pending
  AND t.amount >= $2
  AND t.date >= $3
  AND NOT COALESCE(c.is_income, FALSE)
  AND NOT COALESCE(c.is_transfer, FALSE)
  AND NOT EXISTS (
      SELECT 1 FROM alert_events e
      WHERE e.alert_id = $4 AND e.transaction_id = t.id
  )
ORDER BY t.amount DESC
LIMIT 500;

-- name: UnusualMerchantCandidates :many
-- Merchants whose first-ever transaction in the household falls on or after the
-- cutoff — i.e. merchants that only just started appearing. One representative
-- (most recent) qualifying spend per merchant is returned so the event can link
-- to a real transaction. A merchant this alert already flagged is skipped via
-- the payload merchant_key, so a merchant is announced once, not once per new
-- charge.
WITH firsts AS (
    SELECT t.merchant_key, MIN(t.date) AS first_seen
    FROM transactions t
    JOIN accounts a    ON a.id = t.account_id
    JOIN plaid_items i ON i.id = a.plaid_item_id
    JOIN users u       ON u.id = i.user_id
    WHERE u.household_id = $1
      AND t.merchant_key IS NOT NULL
      AND NOT t.pending
    GROUP BY t.merchant_key
    HAVING MIN(t.date) >= $2
)
SELECT DISTINCT ON (t.merchant_key)
       t.id, t.amount, t.date, t.merchant_key,
       COALESCE(t.merchant_name, t.name) AS merchant
FROM transactions t
JOIN accounts a    ON a.id = t.account_id
JOIN plaid_items i ON i.id = a.plaid_item_id
JOIN users u       ON u.id = i.user_id
JOIN firsts f      ON f.merchant_key = t.merchant_key
LEFT JOIN categories c ON c.id = t.category_id
WHERE u.household_id = $1
  AND a.is_active
  AND NOT t.excluded_from_reports
  AND NOT t.pending
  AND t.amount >= $3
  AND NOT COALESCE(c.is_income, FALSE)
  AND NOT COALESCE(c.is_transfer, FALSE)
  AND NOT EXISTS (
      SELECT 1 FROM alert_events e
      WHERE e.alert_id = $4 AND e.payload->>'merchant_key' = t.merchant_key
  )
ORDER BY t.merchant_key, t.date DESC;

-- name: BudgetSpendShared :many
-- Spend against each household budget for a period, counting SHARED items only
-- so a private account cannot silently push a shared budget over its limit.
SELECT
    b.id          AS budget_id,
    b.amount      AS budgeted,
    c.slug        AS category_slug,
    c.name        AS category_name,
    COALESCE((
        SELECT SUM(t.amount)
        FROM transactions t
        JOIN accounts a    ON a.id = t.account_id
        JOIN plaid_items i ON i.id = a.plaid_item_id
        JOIN users u       ON u.id = i.user_id
        WHERE u.household_id = b.household_id
          AND i.is_shared
          AND a.is_active
          AND NOT t.excluded_from_reports
          AND NOT t.pending
          AND t.category_id = b.category_id
          AND t.amount > 0
          AND t.date >= $2 AND t.date <= $3
    ), 0)::numeric AS spent
FROM budgets b
JOIN categories c ON c.id = b.category_id
WHERE b.household_id = $1;

-- name: SharedMonthCashflow :one
-- Income and spending for a period over SHARED items only — the household's
-- common money, for the low-leftover check.
WITH visible AS (
    SELECT t.amount, c.is_income, c.is_transfer
    FROM transactions t
    JOIN accounts a    ON a.id = t.account_id
    JOIN plaid_items i ON i.id = a.plaid_item_id
    JOIN users u       ON u.id = i.user_id
    LEFT JOIN categories c ON c.id = t.category_id
    WHERE u.household_id = $1
      AND i.is_shared
      AND a.is_active
      AND NOT t.excluded_from_reports
      AND NOT t.pending
      AND t.date >= $2 AND t.date <= $3
)
SELECT
    COALESCE(SUM(-amount) FILTER (WHERE is_income), 0)::numeric AS income,
    COALESCE(SUM(amount)  FILTER (WHERE NOT COALESCE(is_income, FALSE)
                                    AND NOT COALESCE(is_transfer, FALSE)
                                    AND amount > 0), 0)::numeric AS spending
FROM visible;

-- --------------------------------------------------------------------------
-- Events
-- --------------------------------------------------------------------------

-- name: InsertAlertEvent :one
INSERT INTO alert_events (alert_id, transaction_id, payload)
VALUES ($1, $2, $3)
RETURNING *;

-- name: ListUnnotifiedAlertEvents :many
-- New alert events for a household that have not yet been dispatched for push.
-- Joined to the rule for its type so the dispatcher can match against each
-- member's notify.push_kinds. Oldest first, so pushes arrive in the order the
-- events were raised.
SELECT e.id, e.payload, al.type AS alert_type
FROM alert_events e
JOIN alerts al ON al.id = e.alert_id
WHERE al.household_id = $1
  AND e.notified_at IS NULL
ORDER BY e.triggered_at;

-- name: MarkAlertEventNotified :exec
-- Stamp an event as dispatched. Called once per event after its member
-- notifications are enqueued, so a re-run or overlapping sweep skips it.
UPDATE alert_events SET notified_at = now()
WHERE id = $1 AND notified_at IS NULL;

-- name: AlertEventExistsForPeriod :one
-- Dedup for aggregate alerts: has this alert already fired for this period
-- (and, for budgets, this category)? category_key is '' for whole-period
-- alerts like low_leftover.
SELECT EXISTS (
    SELECT 1 FROM alert_events
    WHERE alert_id = $1
      AND payload->>'period' = @period::text
      AND COALESCE(payload->>'category_slug', '') = @category_slug::text
) AS exists;

-- name: ListAlertEvents :many
-- Events visible to the requesting user: household aggregate events (no linked
-- transaction) plus transaction-linked events whose transaction the user may
-- see. Unread first, newest first.
SELECT
    e.id, e.alert_id, e.transaction_id, e.triggered_at, e.payload, e.read_at,
    al.type AS alert_type
FROM alert_events e
JOIN alerts al           ON al.id = e.alert_id
LEFT JOIN transactions t ON t.id = e.transaction_id
LEFT JOIN accounts a     ON a.id = t.account_id
LEFT JOIN plaid_items i  ON i.id = a.plaid_item_id
WHERE al.household_id = $1
  AND (e.transaction_id IS NULL OR i.user_id = $2 OR i.is_shared)
ORDER BY (e.read_at IS NULL) DESC, e.triggered_at DESC
LIMIT $3;

-- name: CountUnreadAlertEvents :one
SELECT COUNT(*)::bigint AS count
FROM alert_events e
JOIN alerts al           ON al.id = e.alert_id
LEFT JOIN transactions t ON t.id = e.transaction_id
LEFT JOIN accounts a     ON a.id = t.account_id
LEFT JOIN plaid_items i  ON i.id = a.plaid_item_id
WHERE al.household_id = $1
  AND e.read_at IS NULL
  AND (e.transaction_id IS NULL OR i.user_id = $2 OR i.is_shared);

-- name: MarkAlertEventRead :exec
UPDATE alert_events e
SET read_at = now()
WHERE e.id = $1
  AND e.read_at IS NULL
  AND e.alert_id IN (SELECT id FROM alerts WHERE household_id = $2)
  AND (
      e.transaction_id IS NULL
      OR EXISTS (
          SELECT 1 FROM transactions t
          JOIN accounts a    ON a.id = t.account_id
          JOIN plaid_items i ON i.id = a.plaid_item_id
          WHERE t.id = e.transaction_id AND (i.user_id = $3 OR i.is_shared)
      )
  );

-- name: MarkAllAlertEventsRead :exec
UPDATE alert_events e
SET read_at = now()
WHERE e.read_at IS NULL
  AND e.alert_id IN (SELECT id FROM alerts WHERE household_id = $1)
  AND (
      e.transaction_id IS NULL
      OR EXISTS (
          SELECT 1 FROM transactions t
          JOIN accounts a    ON a.id = t.account_id
          JOIN plaid_items i ON i.id = a.plaid_item_id
          WHERE t.id = e.transaction_id AND (i.user_id = $2 OR i.is_shared)
      )
  );
