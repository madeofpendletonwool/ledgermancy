-- Outgoing webhooks: subscriptions, the messages they are owed, and the
-- delivery attempts made for each.
--
-- Two things in here are load-bearing and should be read before editing:
--
--   * The enqueue statements are INSERT ... SELECT over `webhooks`. That is not
--     a style choice. Scoping lives in the SELECT's WHERE, so a message can
--     only ever be created for a subscriber in the household the event belongs
--     to — there is no application-side loop that could forget the predicate.
--   * EnqueueAlertEventWebhookMessages carries the household visibility rule on
--     top of that: an alert raised by a charge on a PRIVATE account reaches only
--     a webhook whose owner may see that transaction. It is the same
--     `(v.user_id = ... OR v.is_shared)` shape ListAlertEvents uses.
--
-- Every enqueue is ON CONFLICT DO NOTHING against (webhook_id, dedupe_key), so
-- the periodic sweeps that re-examine the same events are free to run as often
-- as they like without duplicating deliveries.

-- --------------------------------------------------------------------------
-- Subscriptions
-- --------------------------------------------------------------------------

-- name: ListWebhooks :many
-- The settings list. secret_encrypted is deliberately absent: nothing above the
-- delivery worker has any use for it, and a sealed credential has no business
-- being carried through a handler.
SELECT id, user_id, name, url, active, triggers, created_at, updated_at
FROM webhooks
WHERE household_id = $1
ORDER BY created_at DESC;

-- name: GetWebhook :one
-- household_id is in the predicate, so a caller cannot read another household's
-- subscription even with a valid id. Used as the guard before every messages or
-- attempts read.
SELECT id, user_id, name, url, active, triggers, created_at, updated_at
FROM webhooks
WHERE id = $1 AND household_id = $2;

-- name: CreateWebhook :one
INSERT INTO webhooks (household_id, user_id, name, url, secret_encrypted, active, triggers)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id, user_id, name, url, active, triggers, created_at, updated_at;

-- name: UpdateWebhook :one
-- The secret is not editable here. Rotating it is a separate, deliberate act
-- (RotateWebhookSecret) because it breaks every receiver at once, and burying
-- that inside "I renamed my webhook" would be a trap.
UPDATE webhooks
SET name = $3, url = $4, active = $5, triggers = $6, updated_at = now()
WHERE id = $1 AND household_id = $2
RETURNING id, user_id, name, url, active, triggers, created_at, updated_at;

-- name: RotateWebhookSecret :execrows
UPDATE webhooks
SET secret_encrypted = $3, updated_at = now()
WHERE id = $1 AND household_id = $2;

-- name: DeleteWebhook :execrows
-- execrows lets the handler tell "not yours" from "deleted" without a second
-- query. The messages and attempts go with it by cascade.
DELETE FROM webhooks WHERE id = $1 AND household_id = $2;

-- --------------------------------------------------------------------------
-- Producing messages (write-before-deliver)
-- --------------------------------------------------------------------------

-- name: EnqueueWebhookMessages :many
-- One message per active subscriber in this household that asked for this
-- trigger. Returns the ids so the caller can queue a delivery job for each.
--
-- The event is durable the moment this returns, which is the entire point: from
-- here on a receiver being down costs a retry, never the event.
INSERT INTO webhook_messages (webhook_id, trigger_type, payload, dedupe_key)
SELECT w.id, sqlc.arg(trigger_type), sqlc.arg(payload), sqlc.arg(dedupe_key)
FROM webhooks w
WHERE w.household_id = sqlc.arg(household_id)
  AND w.active
  AND sqlc.arg(trigger_type)::text = ANY (w.triggers)
ON CONFLICT (webhook_id, dedupe_key) DO NOTHING
RETURNING id;

-- name: EnqueueAlertEventWebhookMessages :many
-- EnqueueWebhookMessages, plus the alert visibility rule.
--
-- An aggregate alert (budget_threshold, low_leftover) carries no transaction and
-- is household-visible, so it reaches every subscriber. A transaction-linked
-- alert (big_spend, unusual_merchant) reaches a subscriber only when its owner
-- could see that transaction in the app: their own item, or a shared one.
-- Without this, wiring up a webhook would be a way to read a partner's private
-- account — the one thing the visibility model exists to prevent.
INSERT INTO webhook_messages (webhook_id, trigger_type, payload, dedupe_key)
SELECT w.id, sqlc.arg(trigger_type), sqlc.arg(payload), sqlc.arg(dedupe_key)
FROM webhooks w
WHERE w.household_id = sqlc.arg(household_id)
  AND w.active
  AND sqlc.arg(trigger_type)::text = ANY (w.triggers)
  AND EXISTS (
      SELECT 1
      FROM alert_events e
      LEFT JOIN transactions t   ON t.id = e.transaction_id
      LEFT JOIN accounts a       ON a.id = t.account_id
      LEFT JOIN account_access v ON v.account_id = a.id
      WHERE e.id = sqlc.arg(alert_event_id)
        AND (e.transaction_id IS NULL OR v.user_id = w.user_id OR v.is_shared)
  )
ON CONFLICT (webhook_id, dedupe_key) DO NOTHING
RETURNING id;

-- name: EnqueueWebhookMessageForWebhook :one
-- The "send a test" path: one named subscriber rather than a household fan-out.
-- household_id stays in the predicate so the id alone cannot reach across
-- households, and `active` is deliberately NOT required — testing a paused
-- webhook while fixing its receiver is the whole reason to pause it.
INSERT INTO webhook_messages (webhook_id, trigger_type, payload, dedupe_key)
SELECT w.id, sqlc.arg(trigger_type), sqlc.arg(payload), sqlc.arg(dedupe_key)
FROM webhooks w
WHERE w.id = sqlc.arg(webhook_id) AND w.household_id = sqlc.arg(household_id)
RETURNING id;

-- --------------------------------------------------------------------------
-- Delivering
-- --------------------------------------------------------------------------

-- name: GetWebhookMessageForDelivery :one
-- Everything one delivery needs, in one round trip: the body to send and sign,
-- and the subscription's current url, secret and enabled state.
--
-- url/active/secret are read HERE rather than baked into the job args, so a
-- webhook edited between enqueue and delivery is honoured — a user who fixed a
-- typo in the URL should not have to wait out the backlog queued against the
-- old one. The payload is the opposite: it was frozen at enqueue time and is
-- never rebuilt.
SELECT
    m.id, m.webhook_id, m.trigger_type, m.payload, m.status, m.attempts,
    m.dedupe_key, m.created_at,
    w.household_id, w.url, w.secret_encrypted, w.active
FROM webhook_messages m
JOIN webhooks w ON w.id = m.webhook_id
WHERE m.id = $1;

-- name: BeginWebhookAttempt :one
-- Claims the next attempt number and stamps it on the message BEFORE the
-- request goes out, so a worker killed mid-flight still leaves evidence that a
-- request was started. Returns the number the attempt row should carry.
UPDATE webhook_messages
SET attempts = attempts + 1
WHERE id = $1
RETURNING attempts;

-- name: RecordWebhookAttempt :one
-- The delivery record. Bodies are truncated by the caller before they get here
-- — a receiver that answers with a 40 KB HTML error page should cost a few
-- hundred bytes of history, not forty kilobytes per retry.
INSERT INTO webhook_attempts (
    message_id, attempt, request_headers, request_body,
    response_status, response_headers, response_body, error, duration_ms
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING id, message_id, attempt, request_headers, request_body,
          response_status, response_headers, response_body, error,
          duration_ms, created_at;

-- name: MarkWebhookMessageSent :exec
UPDATE webhook_messages
SET status = 'sent', delivered_at = now(), last_error = NULL
WHERE id = $1;

-- name: MarkWebhookMessageFailed :exec
-- The dead letter. The row stays forever (until retention collects it) and
-- keeps every attempt behind it, because "it silently stopped working three
-- weeks ago" is the failure mode this whole table exists to make visible.
UPDATE webhook_messages
SET status = 'failed', last_error = $2
WHERE id = $1;

-- name: ListPendingWebhookMessages :many
-- The sweep's backlog: messages still owed a delivery whose job is not obviously
-- in flight. Bounded, oldest first, and served by the partial index — on a
-- healthy instance this returns nothing at all.
--
-- The age cutoff is what keeps the sweep from racing the queue: a message
-- enqueued a second ago already has a job, and re-enqueuing it would only be
-- collapsed by the job's uniqueness anyway.
SELECT id FROM webhook_messages
WHERE status = 'pending' AND created_at < $1
ORDER BY created_at
LIMIT $2;

-- --------------------------------------------------------------------------
-- Inspecting
-- --------------------------------------------------------------------------

-- name: ListWebhookMessages :many
-- The message list for one subscription. The handler has already proved the
-- webhook belongs to the caller's household via GetWebhook; the predicate here
-- is by webhook alone.
SELECT id, webhook_id, trigger_type, payload, status, attempts,
       delivered_at, last_error, created_at
FROM webhook_messages
WHERE webhook_id = $1
ORDER BY created_at DESC
LIMIT $2;

-- name: ListWebhookAttempts :many
-- Attempts for one message, oldest first — the order they happened, which is
-- the order somebody debugging wants to read them in. webhook_id is in the
-- predicate so a message id from another subscription cannot be read through a
-- webhook the caller does own.
SELECT a.id, a.message_id, a.attempt, a.request_headers, a.request_body,
       a.response_status, a.response_headers, a.response_body, a.error,
       a.duration_ms, a.created_at
FROM webhook_attempts a
JOIN webhook_messages m ON m.id = a.message_id
WHERE a.message_id = $1 AND m.webhook_id = $2
ORDER BY a.attempt;

-- name: DeleteOldWebhookMessages :execrows
-- Retention. Delivery history is a debugging aid with a short useful life, and
-- an instance that fires a few hundred events a day would otherwise grow this
-- table without bound. Attempts go with the message by cascade.
DELETE FROM webhook_messages WHERE created_at < $1;
