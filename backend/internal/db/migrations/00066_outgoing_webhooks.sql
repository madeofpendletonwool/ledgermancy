-- +goose Up
-- Outgoing webhooks: the app's outbound event bus.
--
-- Ledgermancy could already reach a phone (ntfy) and an inbox (SMTP). Neither
-- reaches Home Assistant, a Discord channel, or the three-line script somebody
-- wrote to append every big charge to a spreadsheet. This is the surface that
-- lets a household wire the app to whatever it already runs, without the app
-- having to ship an integration for each of them.
--
-- Numbering: 00065 was the last migration on main (api_tokens), so this takes
-- 00066. Goose refuses a migration numbered below the current version.
--
-- Three tables rather than one, because the lifecycle genuinely has three
-- nouns and a support question ("why didn't my webhook fire?") needs all three:
-- the SUBSCRIPTION (what should be sent, and where), the MESSAGE (one event
-- that should reach one subscriber), and the ATTEMPT (one HTTP request and
-- whatever came back). Collapsing attempts into the message would keep only the
-- last failure, which is exactly the one that explains least.

-- --------------------------------------------------------------------------
-- 1. webhooks — the subscription
-- --------------------------------------------------------------------------
CREATE TABLE webhooks (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id UUID NOT NULL REFERENCES households (id) ON DELETE CASCADE,

    -- The member who created it, and whose visibility the delivery filter uses.
    --
    -- This column is what keeps outgoing webhooks from being a hole in the
    -- household visibility model. A household's own events are not uniformly
    -- visible to everyone in it: an alert raised by a charge on a private
    -- account is shown only to the account's owner (see ListAlertEvents). A
    -- webhook is a standing subscription, so without an owner it would have to
    -- be treated as "the household" and would forward a partner's private
    -- spending to whatever the other partner wired up. Anchoring it to a user
    -- lets the enqueue predicate reuse the identical
    -- `(v.user_id = ... OR v.is_shared)` shape every other read uses — a
    -- webhook sees exactly what its creator sees, which is the same promise
    -- personal API tokens make.
    user_id      UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    -- What the user called it. Required for the same reason api_tokens.name is:
    -- an unlabelled row in a list of things that talk to the outside world is a
    -- row nobody dares delete.
    name         TEXT NOT NULL,

    -- Where deliveries are POSTed. http:// is allowed alongside https://, and
    -- that is deliberate: the flagship destination for a self-hosted finance app
    -- is a Home Assistant or a script on the same LAN, which is very often
    -- plain HTTP on a private address. Refusing it would leave the feature
    -- useful only to people who already have a public TLS endpoint, which is not
    -- who this is for. The signature below is what makes an unencrypted hop
    -- tamper-evident; the docs say plainly that it is not confidential.
    url          TEXT NOT NULL,

    -- The HMAC key, sealed with the same AES-256-GCM cipher as Plaid access
    -- tokens and the document vault. Never returned by the API after creation:
    -- a receiver that has lost its copy rotates rather than reads.
    secret_encrypted BYTEA NOT NULL,

    -- Off switch that keeps the delivery history. Deleting the row cascades the
    -- messages and attempts away, which is the wrong tool for "pause this while
    -- I fix my receiver".
    active       BOOLEAN NOT NULL DEFAULT TRUE,

    -- Which events this subscriber wants. An array rather than a join table:
    -- the set is small, closed, and always read in full alongside the row, and
    -- `= ANY (triggers)` is exactly the predicate the enqueue needs. Validated
    -- in Go against webhooks.Triggers — the check below only enforces that the
    -- subscription selects something, since a webhook subscribed to nothing is
    -- a row that can never fire and never says why.
    triggers     TEXT[] NOT NULL,

    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT webhooks_name_not_blank CHECK (btrim(name) <> ''),
    CONSTRAINT webhooks_url_is_http CHECK (url ~* '^https?://'),
    CONSTRAINT webhooks_triggers_not_empty CHECK (cardinality(triggers) > 0)
);

-- The list is "this household's webhooks, newest first", the only read that is
-- not by id. The enqueue predicate rides the same index prefix.
CREATE INDEX webhooks_household_idx ON webhooks (household_id, created_at DESC);

-- --------------------------------------------------------------------------
-- 2. webhook_messages — one event, one subscriber
-- --------------------------------------------------------------------------
--
-- The load-bearing rule for this table: the row is written BEFORE any delivery
-- is attempted, in the same pass that produced the event. That is the same
-- write-before-deliver discipline the digest follows, and it is what makes "a
-- failed delivery never loses the event" true rather than hoped for — if the
-- receiver is down, or the worker dies mid-request, or the process is killed
-- between producing and sending, the message is already durable and the sweep
-- will pick it up.
CREATE TABLE webhook_messages (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    webhook_id  UUID NOT NULL REFERENCES webhooks (id) ON DELETE CASCADE,

    -- Which trigger produced it. Denormalised from the webhook's array because
    -- the subscription can be edited afterwards, and "what was this message"
    -- must not change when somebody unticks a box a week later.
    trigger_type TEXT NOT NULL,

    -- The body, exactly as it will be signed and sent. Stored rather than
    -- rebuilt at delivery time so a retry days later sends the event that
    -- happened, not a re-derivation of it against data that has since moved.
    payload     JSONB NOT NULL,

    status      TEXT NOT NULL DEFAULT 'pending',

    -- How many HTTP requests have been made. Distinct from a retry counter in
    -- the queue: this survives the job row being cleaned up, so the inspector
    -- can still say "we tried five times" long afterwards.
    attempts    INTEGER NOT NULL DEFAULT 0,

    -- When the receiver finally accepted it. NULL for pending and failed.
    delivered_at TIMESTAMPTZ,

    -- Why it was given up on, for a dead-lettered message. Short, and shown in
    -- the inspector next to the attempts that produced it.
    last_error  TEXT,

    -- Idempotency for the producers. Every producer derives a stable key from
    -- the thing that happened (the insight id, the alert event id, ...), so a
    -- sweep that re-runs over the same events — and both the insight and alert
    -- sweeps do, by design — enqueues each event exactly once per subscriber
    -- instead of once per sweep. NOT NULL so the unique index below is total:
    -- the one producer with no natural key (the "send a test" button) supplies
    -- a random one, which is honest — every test press IS a new event.
    dedupe_key  TEXT NOT NULL,

    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT webhook_messages_status_known
        CHECK (status IN ('pending', 'sent', 'failed'))
);

-- The idempotency guarantee. Per webhook, not global: two subscribers to the
-- same event each get their own message, and neither can suppress the other's.
CREATE UNIQUE INDEX webhook_messages_dedupe_idx
    ON webhook_messages (webhook_id, dedupe_key);

-- The inspector's read: this webhook's messages, newest first.
CREATE INDEX webhook_messages_webhook_idx
    ON webhook_messages (webhook_id, created_at DESC);

-- The sweep's read: everything still owed a delivery, oldest first. Partial, so
-- it stays the size of the backlog rather than the size of the history — on a
-- healthy instance that is zero rows.
CREATE INDEX webhook_messages_pending_idx
    ON webhook_messages (created_at)
    WHERE status = 'pending';

-- --------------------------------------------------------------------------
-- 3. webhook_attempts — one HTTP request and its answer
-- --------------------------------------------------------------------------
--
-- This table exists so the answer to "why did my webhook fail?" is a fact
-- rather than a guess. It records what we sent (headers and body, so a receiver
-- author can replay the signature check by hand) and what came back (status,
-- headers, body) or why nothing did.
CREATE TABLE webhook_attempts (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    message_id UUID NOT NULL REFERENCES webhook_messages (id) ON DELETE CASCADE,

    -- 1-based, matching webhook_messages.attempts after this row is written.
    attempt    INTEGER NOT NULL,

    request_headers JSONB NOT NULL,
    request_body    TEXT  NOT NULL,

    -- All NULL when the request never completed (DNS failure, refused
    -- connection, timeout); `error` carries the reason in that case. A response
    -- and an error are mutually exclusive, and the inspector renders whichever
    -- is present.
    response_status  INTEGER,
    response_headers JSONB,
    response_body    TEXT,
    error            TEXT,

    -- Wall-clock time for the request, which is usually the first thing that
    -- explains a timeout.
    duration_ms INTEGER NOT NULL DEFAULT 0,

    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Attempts are only ever read for one message, in order.
CREATE INDEX webhook_attempts_message_idx ON webhook_attempts (message_id, attempt);

-- +goose Down
DROP TABLE IF EXISTS webhook_attempts;
DROP TABLE IF EXISTS webhook_messages;
DROP TABLE IF EXISTS webhooks;
