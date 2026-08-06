-- +goose Up
--
-- The advisor surface (doc 31): conversation persistence, tracked action items,
-- and the two household-profile columns a wave-6 engine actually consumes.
--
-- Numbered 00054, not the 00052 doc 31 reserved. That reservation was written
-- when 00046 was the highest applied migration; 00048-00053 have landed since
-- (00052 is cpi_series, 00053 is manual accounts), and goose runs in
-- strict-ordering mode (db.go) — a 00052 arriving after 00053 is already
-- applied would refuse to start every instance that has run them. Renumbering
-- is the documented response, as doc 30's migration also records. See
-- docs/plans/README.md.
--
-- The through-line: the chat is currently stateless, so a household narrating
-- its money starts from nothing every time. An advisor relationship is
-- cumulative. That is the whole of what this migration buys — plus the record
-- of what the household decided to DO about any of it.

-- --------------------------------------------------------------------------
-- 1. households — profile columns, each with a named consumer
-- --------------------------------------------------------------------------
--
-- A column with no consumer is not a profile, it is a form field. Both columns
-- below name the engine that reads them, and `households.state` is deliberately
-- absent: AnnualLimitFor honestly declines to cap a 529 rather than inventing a
-- state limit (limits.go), and no wave-6 doc computes a state bracket. Whichever
-- doc first builds a state-dependent engine adds the column with it.

ALTER TABLE households
    -- Consumed by doc 32's Roth/IRA eligibility check: the MAGI phase-out
    -- thresholds are keyed by filing status, and without it the allocator
    -- cannot tell "capped at $7,500" from "not allowed to contribute at all".
    -- This doc ships the column and the `eligibility` field that will key on
    -- it; doc 32 owns the phase-out table itself.
    ADD COLUMN filing_status       TEXT
        CHECK (filing_status IN ('single', 'married_joint', 'married_separate', 'hoh')),
    -- Consumed by doc 33's guardrail rule as the personal drawdown limit.
    -- A percent, so 20.00 means "a 20% drawdown is my floor".
    ADD COLUMN risk_drawdown_floor NUMERIC(5, 2);

-- --------------------------------------------------------------------------
-- 2. advisor_threads — saved conversations
-- --------------------------------------------------------------------------
--
-- Household-scoped, with is_shared following the existing account-visibility
-- convention: a thread is visible to the whole household unless its author
-- marked it private, and a private thread is invisible to the spouse.

CREATE TABLE advisor_threads (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id UUID        NOT NULL REFERENCES households (id) ON DELETE CASCADE,
    -- SET NULL rather than CASCADE: a departing member's threads belong to the
    -- household that paid for the reasoning in them, not to the login.
    user_id      UUID REFERENCES users (id) ON DELETE SET NULL,
    title        TEXT        NOT NULL,
    is_shared    BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX advisor_threads_household_idx ON advisor_threads (household_id, updated_at DESC);

-- --------------------------------------------------------------------------
-- 3. advisor_messages — the turns, sealed
-- --------------------------------------------------------------------------
--
-- Two decisions here carry the weight.
--
-- content is BYTEA because a transcript is the most sensitive text this app
-- holds — a household narrating its salary, its debts, whether it can afford
-- the house — and the repo already seals Plaid tokens and every document byte
-- under ENCRYPTION_KEY. BYTEA also means the portable continuity export
-- withholds these columns BY TYPE (continuity.IsSensitive), which is the right
-- default for a transcript riding in a plain JSON file the user may email
-- themselves. The pg_dump still carries them, so a restore recovers them whole.
--
-- tool_trace exists because persisting only role and prose quietly breaks the
-- one rule the chat has. chatSystemPrompt requires that every number the model
-- states come verbatim from a tool result; a reloaded history of figures with no
-- provenance lets the model re-read its own six-week-old "$4,120 safe to spend"
-- as though it were current and sourced. Storing the tool calls and results
-- beside the turn is what makes the reload honest — and it is also what lets a
-- reloaded thread re-render its inline charts, since the chart frame is the
-- tool result.

CREATE TABLE advisor_messages (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    thread_id  UUID        NOT NULL REFERENCES advisor_threads (id) ON DELETE CASCADE,
    role       TEXT        NOT NULL CHECK (role IN ('user', 'assistant')),
    -- Sealed with the same AES-GCM cipher as Plaid tokens and document bytes.
    content    BYTEA       NOT NULL,
    -- The tool calls and results behind an assistant turn. Same cipher; NULL for
    -- a user turn.
    tool_trace BYTEA,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX advisor_messages_thread_idx ON advisor_messages (thread_id, created_at);

-- --------------------------------------------------------------------------
-- 4. advisor_action_items — what the household decided to do
-- --------------------------------------------------------------------------
--
-- The advisor proposes, the user accepts, and it is TRACKED. Never executed:
-- transfers and payments are permanently out of scope, and no column here is a
-- step towards one. Doc 33 extends this with plan-vs-actual reconciliation.

CREATE TABLE advisor_action_items (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id UUID        NOT NULL REFERENCES households (id) ON DELETE CASCADE,
    title        TEXT        NOT NULL,
    detail       TEXT,
    -- Where the item came from, so a tray row can link back to what proposed it.
    source       TEXT        NOT NULL CHECK (source IN ('option', 'allocation', 'thread', 'manual')),
    status       TEXT        NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'done', 'dismissed')),
    due_date     DATE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ
);

CREATE INDEX advisor_action_items_household_idx ON advisor_action_items (household_id, status);

-- +goose Down

DROP TABLE IF EXISTS advisor_action_items;
DROP TABLE IF EXISTS advisor_messages;
DROP TABLE IF EXISTS advisor_threads;

ALTER TABLE households DROP COLUMN IF EXISTS risk_drawdown_floor;
ALTER TABLE households DROP COLUMN IF EXISTS filing_status;
