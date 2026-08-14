-- Scheduled digest: who is a candidate, the dedupe guards, the top unread
-- insights the digest carries, and the stored entries that back the in-app
-- surface. Opt-in is per user, so the sweep enumerates users (not households)
-- and resolves each user's household for the figures.

-- name: ListDigestCandidateUsers :many
-- Every adult who might be due a digest, with their household, cadence and the
-- three per-surface switches resolved in one pass.
--
-- This deliberately does NOT filter on an opt-in the way its predecessor
-- (ListDigestEnabledUsers) did. The in-app digest defaults ON and needs no
-- notification channel, so gating the whole sweep on the push preference is
-- exactly the bug doc 25 exists to fix. The caller skips a user for whom all
-- three switches are off.
--
-- Preference semantics, matching what the API stores as raw JSONB:
--   digest.enabled — push. Defaults FALSE: sending someone an unrequested push
--                    is not a safe default, and this is the pre-existing key.
--   digest.in_app  — the in-app entry. Defaults TRUE, so "what happened with my
--                    money this week" exists without configuring anything.
--   digest.email   — SMTP. Defaults FALSE, and inert unless the operator has
--                    configured a server at all.
--
-- Children are excluded. Every financial surface in the app is adult-only, and
-- a child login can write its own user-scoped preferences — so without this
-- filter a child could turn a household spending recap on for themselves.
SELECT
    u.id           AS user_id,
    u.household_id AS household_id,
    u.email        AS email,
    COALESCE(c.value #>> '{}', 'weekly')::text          AS cadence,
    COALESCE(p.value = 'true'::jsonb, FALSE)::boolean   AS push_enabled,
    COALESCE(a.value <> 'false'::jsonb, TRUE)::boolean  AS in_app_enabled,
    COALESCE(m.value = 'true'::jsonb, FALSE)::boolean   AS email_enabled
FROM users u
LEFT JOIN preferences c
  ON c.scope = 'user' AND c.user_id = u.id AND c.key = 'digest.cadence'
LEFT JOIN preferences p
  ON p.scope = 'user' AND p.user_id = u.id AND p.key = 'digest.enabled'
LEFT JOIN preferences a
  ON a.scope = 'user' AND a.user_id = u.id AND a.key = 'digest.in_app'
LEFT JOIN preferences m
  ON m.scope = 'user' AND m.user_id = u.id AND m.key = 'digest.email'
WHERE u.role <> 'child';

-- name: DigestDeliveryExists :one
-- True once a PUSH for this (user, period) has been recorded, so the worker
-- skips a user already pushed to this week/month.
SELECT EXISTS (
    SELECT 1 FROM digest_deliveries WHERE user_id = $1 AND period_key = $2
);

-- name: DigestPeriodSatisfied :one
-- True when this (user, period) has produced anything at all — a recorded push
-- OR a stored in-app entry. The sweep's cheap pre-check: a push-only user is
-- covered by the first, an in-app-only user by the second, and neither surface
-- can be silently starved because the other one happens to be off.
SELECT (
    EXISTS (SELECT 1 FROM digest_deliveries d WHERE d.user_id = $1 AND d.period_key = $2)
    OR
    EXISTS (SELECT 1 FROM digest_entries    e WHERE e.user_id = $1 AND e.period_key = $2)
)::boolean AS satisfied;

-- name: RecordDigestDelivery :exec
-- Insert-on-send. DO NOTHING makes a re-run harmless if two workers race.
INSERT INTO digest_deliveries (user_id, period_key)
VALUES ($1, $2)
ON CONFLICT (user_id, period_key) DO NOTHING;

-- name: ListUnreadInsightsForDigest :many
-- The top unread, non-dismissed insights for a household, in feed order, capped
-- for the digest body.
--
-- Retracted rows are excluded alongside dismissed ones, and this surface is the
-- one where it matters most: a digest is a mail nobody can take back. Sending
-- "your Sprout Childcare payment is overdue" hours after the payment posted —
-- which is precisely what an unretracted insight would do here, since a
-- withdrawn claim is unread by definition — is worse than saying nothing.
SELECT * FROM insights
WHERE household_id = $1
  AND dismissed_at IS NULL
  AND retracted_at IS NULL
  AND read_at IS NULL
ORDER BY priority DESC, created_at DESC
LIMIT $2;

-- --------------------------------------------------------------------------
-- Stored entries: the in-app surface.
--
-- Every read here is scoped by user_id, never by household. A digest entry is
-- one member's view of the household's money, and another member reading it
-- would defeat the visibility scoping the figures were computed under.
-- --------------------------------------------------------------------------

-- name: InsertDigestEntry :execrows
-- Write-once. DO NOTHING (rather than DO UPDATE) is the immutability guarantee:
-- once a period's digest is stored, nothing — a second sweep, a retry, a manual
-- "send one now" — rewrites what it said. Returns 0 rows when the period was
-- already stored, which the worker treats as "nothing to do", not an error.
INSERT INTO digest_entries (
    household_id, user_id, cadence, period_key,
    period_start, period_end, label, payload, narrative
) VALUES (
    @household_id, @user_id, @cadence, @period_key,
    @period_start, @period_end, @label, @payload, sqlc.narg('narrative')
)
ON CONFLICT (user_id, period_key) DO NOTHING;

-- name: ListDigestEntries :many
-- The caller's digests, newest first. Paginated by offset rather than a cursor:
-- entries arrive one per period, so the list grows by ~52 rows a year and never
-- reaches the size where offset paging costs anything.
SELECT * FROM digest_entries
WHERE user_id = $1
ORDER BY created_at DESC
LIMIT $2 OFFSET $3;

-- name: CountDigestEntries :one
-- Total and unread counts, so the client can page and badge without walking the
-- whole table.
SELECT
    COUNT(*)::bigint                                     AS total,
    COUNT(*) FILTER (WHERE read_at IS NULL)::bigint      AS unread
FROM digest_entries
WHERE user_id = $1;

-- name: GetDigestEntry :one
-- One entry, scoped to its owner so an id alone never reads another member's.
SELECT * FROM digest_entries
WHERE id = $1 AND user_id = $2;

-- name: MarkDigestEntryRead :exec
-- Idempotent, and a no-op for an id that is not the caller's — the same shape
-- as MarkInsightRead. A second mark leaves the original read time alone, so
-- "when did I first read this" survives a re-visit.
UPDATE digest_entries
SET read_at = now()
WHERE id = $1 AND user_id = $2 AND read_at IS NULL;
