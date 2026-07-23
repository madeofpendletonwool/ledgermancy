-- Insights: the proactive feed's storage. Rows are raised by deterministic
-- detectors (see backend/internal/insights) and read back here in feed order.
-- Every insight is household-scoped; visibility of the underlying figures is
-- enforced upstream in the detection queries, not here.

-- name: UpsertInsight :one
-- Raise or refresh one insight. The generation job runs repeatedly, so this
-- upserts on the stable (household_id, dedupe_key): a re-run with changed facts
-- refreshes title/body/data/priority/period and bumps created_at so the
-- refreshed insight re-surfaces at the top of the feed.
--
-- read_at is cleared on refresh (fresh facts are worth seeing again), but
-- dismissed_at is deliberately preserved: once a user dismisses an insight it
-- stays gone even as the detector keeps re-detecting the same fact.
INSERT INTO insights (
    household_id, kind, priority, title, body, data, period, dedupe_key
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (household_id, dedupe_key) DO UPDATE SET
    kind       = EXCLUDED.kind,
    priority   = EXCLUDED.priority,
    title      = EXCLUDED.title,
    body       = EXCLUDED.body,
    data       = EXCLUDED.data,
    period     = EXCLUDED.period,
    created_at = now(),
    read_at    = NULL
RETURNING *, (xmax = 0) AS inserted;

-- name: ListInsights :many
-- Feed order. With include_dismissed false (the unread feed) dismissed rows are
-- hidden; with it true (the "show dismissed" view) everything is returned.
SELECT * FROM insights
WHERE household_id = $1
  AND (dismissed_at IS NULL OR @include_dismissed::bool)
ORDER BY priority DESC, created_at DESC;

-- name: MarkInsightRead :exec
UPDATE insights
SET read_at = now()
WHERE id = $1 AND household_id = $2 AND read_at IS NULL;

-- name: DismissInsight :exec
UPDATE insights
SET dismissed_at = now()
WHERE id = $1 AND household_id = $2;
