-- Scheduled digest: who is opted in, the dedupe guard, and the top unread
-- insights the digest carries. Opt-in is per user, so the sweep enumerates
-- users (not households) and resolves each user's household for the figures.

-- name: ListDigestEnabledUsers :many
-- Users who have turned the digest on, with their household and cadence. The
-- cadence is read out of JSONB as text (defaulting to weekly) so the sweep can
-- gate on it without a second per-user lookup.
SELECT
    u.id           AS user_id,
    u.household_id AS household_id,
    COALESCE(c.value #>> '{}', 'weekly')::text AS cadence
FROM users u
JOIN preferences e
  ON e.scope = 'user' AND e.user_id = u.id
 AND e.key = 'digest.enabled' AND e.value = 'true'::jsonb
LEFT JOIN preferences c
  ON c.scope = 'user' AND c.user_id = u.id AND c.key = 'digest.cadence';

-- name: DigestDeliveryExists :one
-- True once a digest for this (user, period) has been recorded, so the sweep and
-- the worker both skip a user already served this week/month.
SELECT EXISTS (
    SELECT 1 FROM digest_deliveries WHERE user_id = $1 AND period_key = $2
);

-- name: RecordDigestDelivery :exec
-- Insert-on-send. DO NOTHING makes a re-run harmless if two workers race.
INSERT INTO digest_deliveries (user_id, period_key)
VALUES ($1, $2)
ON CONFLICT (user_id, period_key) DO NOTHING;

-- name: ListUnreadInsightsForDigest :many
-- The top unread, non-dismissed insights for a household, in feed order, capped
-- for the digest body.
SELECT * FROM insights
WHERE household_id = $1 AND dismissed_at IS NULL AND read_at IS NULL
ORDER BY priority DESC, created_at DESC
LIMIT $2;
