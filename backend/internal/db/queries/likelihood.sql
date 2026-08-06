-- The likelihood layer (doc 33): plan tracking.
--
-- There is deliberately NO query here that reads or writes a simulation result.
-- The distribution is recomputed from the plan plus a deterministic seed every
-- time it is asked for, so there is nothing to persist and nothing to go stale.
--
-- Every read carries the same visibility scoping as the rest of the reporting
-- layer. A tracking row hangs off an allocation_plan, which is household-owned,
-- so the scope check is on the plan's household — see the joins below.

-- name: UpsertPlanTracking :one
-- One snapshot per plan per date. A second write for the same day REPLACES it
-- rather than failing: re-tracking an hour later is a correction, not a
-- conflict, and two rows for one date would make "drift since the last check"
-- ambiguous about which check.
--
-- The household join is the scope check. A plan id from another household
-- matches no row, so nothing is written and the handler sees pgx.ErrNoRows —
-- the same shape the manual-account writes use, and for the same reason:
-- ownership belongs inside the statement, not in a check beside it.
INSERT INTO plan_trackings (plan_id, as_of, expected_lump, expected_total, snapshot_inputs)
SELECT p.id,
       sqlc.arg('as_of'),
       sqlc.arg('expected_lump'),
       sqlc.arg('expected_total'),
       sqlc.arg('snapshot_inputs')
FROM allocation_plans p
WHERE p.id = sqlc.arg('plan_id')
  AND p.household_id = sqlc.arg('household_id')
ON CONFLICT (plan_id, as_of) DO UPDATE SET
    expected_lump   = EXCLUDED.expected_lump,
    expected_total  = EXCLUDED.expected_total,
    snapshot_inputs = EXCLUDED.snapshot_inputs
RETURNING *;

-- name: ListPlanTrackings :many
-- The drift sparkline's source. ASCENDING, because it is drawn as a line —
-- the index on (plan_id, as_of DESC) still serves it; a reverse scan is free.
SELECT t.*
FROM plan_trackings t
JOIN allocation_plans p ON p.id = t.plan_id
WHERE t.plan_id = sqlc.arg('plan_id')
  AND p.household_id = sqlc.arg('household_id')
ORDER BY t.as_of;

-- name: ListAccountBalanceHistoryInRange :many
-- Balance history for EVERY visible account over a window, for the plan-vs-
-- actual reconciler.
--
-- Per-household rather than per-account (ListAccountBalanceHistory is the
-- single-account view behind one chart) because the reconciler needs the whole
-- set at once: a plan touches five buckets, and five round trips to answer one
-- drift question is five chances for the window to shift underneath it.
--
-- Ascending by (account, date) so the caller can take the first and last row of
-- each account's run without sorting.
SELECT h.account_id, h.as_of, h.balance, h.reason
FROM account_balance_history h
JOIN account_access v ON v.account_id = h.account_id
WHERE v.household_id = sqlc.arg('household_id')
  AND (v.user_id = sqlc.arg('user_id') OR v.is_shared)
  AND h.as_of >= sqlc.arg('from_date')
  AND h.as_of <= sqlc.arg('to_date')
ORDER BY h.account_id, h.as_of;
