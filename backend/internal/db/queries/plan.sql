-- The financial plan (MAD-258): sections, decisions, and the review stamp.
--
-- Every query is household-scoped, the same shape the advisor queries use. The
-- plan is a household surface — the one page a household is *expected* to share
-- with a spouse — so there is no is_private half here; scope is the household
-- and nothing finer.
--
-- body columns are BYTEA and arrive here already sealed. Nothing in this file
-- knows the plaintext; the API layer seals and opens with the same AES-GCM
-- cipher that holds advisor transcripts and document bytes.

-- name: UpsertPlanSection :one
-- One section per (household, kind, person) slot. The ON CONFLICT target is the
-- expression index plan_sections_slot_idx — COALESCE collapses a NULL person_id
-- to a sentinel so the household-wide kinds get one row each. An upsert rather
-- than insert-then-update because "save the strategy" is idempotent by intent:
-- the row IS the slot.
INSERT INTO plan_sections (household_id, kind, person_id, body, updated_by)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (household_id, kind,
              COALESCE(person_id, '00000000-0000-0000-0000-000000000000'::uuid))
DO UPDATE SET body       = excluded.body,
              updated_by = excluded.updated_by,
              updated_at = now()
RETURNING *;

-- name: ListPlanSections :many
-- Every section, with the person's display name for the per-person kind. The
-- person join is LEFT because person_id is NULL for every household-wide kind;
-- a person row deleted after the section was written would otherwise take the
-- section with it (ON DELETE CASCADE on the FK) — the join never dangles, but
-- LEFT keeps the query honest about that.
SELECT s.*, p.display_name AS person_name
FROM plan_sections s
LEFT JOIN household_people p ON p.id = s.person_id
WHERE s.household_id = $1
ORDER BY s.kind, p.display_name;

-- name: DeletePlanSection :execrows
-- execrows so the handler can 404 a cross-household delete rather than
-- reporting success vacuously — same reason DeleteAdvisorThread uses it.
DELETE FROM plan_sections
WHERE id = $1 AND household_id = $2;

-- name: InsertPlanDecision :one
-- body arrives sealed. status/supersedes/source are validated by the handler
-- before this runs (a proposal may not supersede; only a confirmed decision may
-- be superseded; the target must be this household's).
INSERT INTO plan_decisions (household_id, topic, body, decided_at, status, supersedes, source, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: ListPlanDecisions :many
-- The whole log — active, superseded, and proposed — because the history under
-- a superseded decision is the point of keeping it. `superseded` is derived,
-- never stored: a row is superseded exactly when some CONFIRMED row points at
-- it (a proposal naming a target does not retire it — the proposal is not a
-- decision yet, and if the proposal is discarded the old decision must still
-- be standing).
SELECT d.*,
       EXISTS (SELECT 1 FROM plan_decisions x
               WHERE x.supersedes = d.id AND x.status = 'confirmed') AS superseded
FROM plan_decisions d
WHERE d.household_id = $1
ORDER BY d.decided_at DESC, d.created_at DESC;

-- name: GetActivePlanDecisions :many
-- Confirmed decisions no confirmed row has replaced, newest decision first.
-- This is the advisor briefing's view: the plan the household currently holds,
-- not its history. Bounded because a briefing that quotes forty decisions is a
-- transcript, not a briefing.
SELECT d.*
FROM plan_decisions d
WHERE d.household_id = $1
  AND d.status = 'confirmed'
  AND NOT EXISTS (SELECT 1 FROM plan_decisions x
                  WHERE x.supersedes = d.id AND x.status = 'confirmed')
ORDER BY d.decided_at DESC, d.created_at DESC
LIMIT 10;

-- name: GetPlanDecision :one
-- Household-scoped fetch: the supersede path reads its target through this so a
-- decision from another household is "not found", never "superseded".
SELECT * FROM plan_decisions
WHERE id = $1 AND household_id = $2;

-- name: UpdateProposedPlanDecision :one
-- Editable only while still a proposal — the WHERE clause is the append-only
-- rule enforced by storage, not just by the handler. Zero rows means not-found
-- OR confirmed, and the handler checks which via GetPlanDecision first so the
-- 400 can name the real refusal.
UPDATE plan_decisions
SET topic      = $3,
    body       = $4,
    decided_at = $5
WHERE id = $1 AND household_id = $2 AND status = 'proposed'
RETURNING *;

-- name: ConfirmPlanDecision :one
-- The confirm half of suggestion-then-confirm. Status only: confirming does
-- not edit the wording, so a confirmed row has exactly one authoring moment.
UPDATE plan_decisions
SET status = 'confirmed'
WHERE id = $1 AND household_id = $2 AND status = 'proposed'
RETURNING *;

-- name: DeleteProposedPlanDecision :execrows
-- Discarding a suggestion. Proposed-only, for the same reason the UPDATE above
-- is: a confirmed decision is never deleted, only superseded.
DELETE FROM plan_decisions
WHERE id = $1 AND household_id = $2 AND status = 'proposed';

-- name: StampPlanReview :one
-- The review stamp. An explicit action rather than an updated_at side effect:
-- "I have re-read this and it still says what we mean" is not the same claim as
-- "something changed today", and the plan_stale producer reads it as exactly
-- that distinction.
UPDATE households
SET plan_reviewed_at = now()
WHERE id = $1
RETURNING *;

-- name: GetPlanReviewState :one
-- The staleness inputs, for the plan_stale producer: the review stamp and
-- whether the household has plan CONTENT at all (a proposal alone is not a
-- plan — has_content counts sections and confirmed decisions only, so a
-- household that saved one chat suggestion and never confirmed it is not
-- nudged about reviewing a plan it does not have).
SELECT
    h.plan_reviewed_at,
    EXISTS (SELECT 1 FROM plan_sections s WHERE s.household_id = h.id)
        OR EXISTS (SELECT 1 FROM plan_decisions d
                   WHERE d.household_id = h.id AND d.status = 'confirmed')
        AS has_content
FROM households h
WHERE h.id = $1;

-- name: LatestPlanSectionTouch :one
-- The newest section edit. A :one over a plain column with LIMIT 1 rather than
-- max(): sqlc infers max() over a NOT NULL column as NOT NULL even though it
-- is NULL when no rows match, and scanning that fails at runtime — the
-- documented trap in development.md. Here "no rows" arrives as pgx.ErrNoRows,
-- which the caller handles as "no content".
SELECT updated_at FROM plan_sections
WHERE household_id = $1
ORDER BY updated_at DESC LIMIT 1;

-- name: LatestPlanDecisionTouch :one
-- Same shape, for the newest confirmed decision.
SELECT created_at FROM plan_decisions
WHERE household_id = $1 AND status = 'confirmed'
ORDER BY created_at DESC LIMIT 1;
