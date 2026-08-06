-- The advisor surface's own storage (doc 31): saved conversations and the
-- action items a household accepted out of them.
--
-- Two scoping rules run through every query here and neither is optional.
-- Threads are HOUSEHOLD-scoped and visibility-scoped: `household_id = $1 AND
-- (is_shared OR user_id = $2)`, the same shape the account queries use, so a
-- private thread is invisible to the spouse and a thread from another household
-- does not exist at all. Action items are household-scoped only — they are the
-- household's decisions, and there is no private action item.
--
-- Message bodies are BYTEA and arrive here already sealed. Nothing in this file
-- knows the plaintext; the API layer seals and opens with the same AES-GCM
-- cipher that holds Plaid tokens and document bytes.

-- name: CreateAdvisorThread :one
INSERT INTO advisor_threads (household_id, user_id, title, is_shared)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: ListAdvisorThreads :many
-- The sidebar. Ordered by updated_at so the thread the household last spoke in
-- is the one at the top, and carrying the message count so the UI can tell an
-- empty thread from an unfetched one without a second round trip.
SELECT t.*, (
    SELECT count(*) FROM advisor_messages m WHERE m.thread_id = t.id
) AS message_count
FROM advisor_threads t
WHERE t.household_id = $1
  AND (t.is_shared OR t.user_id = $2)
ORDER BY t.updated_at DESC;

-- name: GetAdvisorThread :one
-- Scoped identically to the list, so an id guessed from another household — or
-- a spouse's private thread — returns no rows rather than a body.
SELECT * FROM advisor_threads
WHERE id = $1
  AND household_id = $2
  AND (is_shared OR user_id = $3);

-- name: RenameAdvisorThread :one
UPDATE advisor_threads
SET title = $4, updated_at = now()
WHERE id = $1
  AND household_id = $2
  AND (is_shared OR user_id = $3)
RETURNING *;

-- name: TouchAdvisorThread :exec
-- Bumps updated_at when a turn is persisted, which is what keeps the sidebar in
-- conversation order. Separate from the insert so the message write stays a
-- plain append.
UPDATE advisor_threads SET updated_at = now()
WHERE id = $1 AND household_id = $2;

-- name: DeleteAdvisorThread :execrows
-- execrows, not exec: the handler needs to distinguish "deleted" from "there was
-- nothing here you could see", and returning 204 for the second would tell a
-- caller they had removed another household's thread.
DELETE FROM advisor_threads
WHERE id = $1
  AND household_id = $2
  AND (is_shared OR user_id = $3);

-- name: InsertAdvisorMessage :one
-- content and tool_trace are already sealed by the caller. tool_trace is NULL
-- for a user turn and for an assistant turn that called no tools.
INSERT INTO advisor_messages (thread_id, role, content, tool_trace)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: ListAdvisorMessages :many
-- Chronological, and joined back through advisor_threads so the scope check
-- cannot be skipped by passing a thread id straight in.
SELECT m.*
FROM advisor_messages m
JOIN advisor_threads t ON t.id = m.thread_id
WHERE m.thread_id = $1
  AND t.household_id = $2
  AND (t.is_shared OR t.user_id = $3)
ORDER BY m.created_at, m.id;

-- name: CreateAdvisorActionItem :one
INSERT INTO advisor_action_items (household_id, title, detail, source, due_date)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: ListAdvisorActionItems :many
-- Open first, then the closed ones newest-first: the tray is a working list, and
-- a done item from March should not sit above something still outstanding.
SELECT * FROM advisor_action_items
WHERE household_id = $1
  AND (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status')::text)
ORDER BY (status = 'open') DESC, created_at DESC;

-- name: UpdateAdvisorActionItemStatus :one
-- Status only — a tray toggle must not become an edit surface for the title an
-- option computed. completed_at is set by the same statement that sets 'done'
-- and cleared on any move back out of it, so the two can never disagree.
UPDATE advisor_action_items
SET status       = $3,
    completed_at = CASE WHEN $3 = 'done' THEN now() ELSE NULL END
WHERE id = $1 AND household_id = $2
RETURNING *;

-- name: UpdateHouseholdProfile :one
-- The two profile columns doc 31 adds. NULL clears a field, which is a real
-- answer: "I have not told you my filing status" is not "single".
UPDATE households
SET filing_status       = $2,
    risk_drawdown_floor = $3,
    updated_at          = now()
WHERE id = $1
RETURNING *;
