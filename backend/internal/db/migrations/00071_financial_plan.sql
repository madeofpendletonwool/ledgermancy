-- +goose Up
--
-- The financial plan (MAD-258): the household's authored INTENT, beside the
-- computed POSITION every other page already shows.
--
-- The app can say where the household IS with exact decimals; it had nowhere to
-- hold "we keep the emergency fund at 3 months because the teaching income is
-- stable, surplus goes to the 529 until 2031, then the mortgage". Goals hold
-- targets without the reasoning; advisor threads hold conversations, not a
-- plan. These two tables are that home, plus one review stamp column.
--
-- Three rules from the design doc, each visible in the schema:
--
--   LINK, DON'T COPY. No column here restates a figure an engine owns (EF
--   target months, filing status, MAGI...). The plan is prose over live
--   objects; a restated number is stale the day after the next sync.
--
--   APPEND, DON'T OVERWRITE. plan_sections bodies are editable (a strategy is
--   a living text) but plan_decisions rows are NEVER edited or deleted once
--   confirmed — a superseding decision is a NEW row pointing back via
--   `supersedes`, so the history is the value. The one permitted UPDATE is
--   touching a row while it is still a proposal (see status), because a
--   proposal is not a decision yet.
--
--   SEALED LIKE THE TRANSCRIPTS. body columns are BYTEA under ENCRYPTION_KEY,
--   same as advisor_messages.content: plan prose is the most sensitive text in
--   the house — intentions, names, timelines. BYTEA also means the portable
--   continuity export withholds these columns BY TYPE (continuity.IsSensitive)
--   while the pg_dump recovers them whole.

-- 1. plan_sections — the living outline ------------------------------------
--
-- A fixed vocabulary of sections rather than a free wiki: a page-tree invites
-- drift and cannot be digested by the advisor briefing. Household-wide kinds
-- are strategy / income / estate / notes; 'person' is per-person notes and
-- rides household_people, so the kid's 529 reasoning attaches to the kid.

CREATE TABLE plan_sections (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id UUID        NOT NULL REFERENCES households (id) ON DELETE CASCADE,
    kind         TEXT        NOT NULL CHECK (kind IN ('strategy', 'income', 'estate', 'person', 'notes')),
    -- NULL for every kind except 'person'. The CHECK keeps the pairing exact:
    -- a person section without a person is an orphan nobody can render, and a
    -- household-wide section pinned to one person is a lie about its scope.
    person_id    UUID        REFERENCES household_people (id) ON DELETE CASCADE,
    -- Sealed with the same AES-GCM cipher as advisor_messages.content.
    body         BYTEA       NOT NULL,
    -- SET NULL rather than CASCADE: who last edited a section is history worth
    -- keeping even after the editor's login is gone.
    updated_by   UUID        REFERENCES users (id) ON DELETE SET NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((kind = 'person' AND person_id IS NOT NULL)
        OR (kind <> 'person' AND person_id IS NULL))
);

-- One section per slot. COALESCE rather than a plain (household_id, kind,
-- person_id) unique constraint because Postgres treats NULLs as distinct —
-- without this, two 'strategy' rows could coexist and "the" strategy would be
-- whichever the ORDER BY happened to return first. The upsert's ON CONFLICT
-- targets this same expression.
CREATE UNIQUE INDEX plan_sections_slot_idx
    ON plan_sections (household_id, kind,
                      COALESCE(person_id, '00000000-0000-0000-0000-000000000000'::uuid));

-- 2. plan_decisions — the append-only log ----------------------------------
--
-- What was decided, when, and why. status splits the log from the suggestion
-- tray: 'proposed' rows are drafts the advisor surface (or a household member)
-- staged for confirmation — editable, deletable — and 'confirmed' rows are the
-- log itself: immutable, replaced only by a newer row that names them in
-- `supersedes`. "Active" is therefore derived, never stored: a confirmed
-- decision is active while no confirmed row points at it.
--
-- `source` records where the wording came from, so a decision the chat drafted
-- and the household confirmed is distinguishable from one typed by hand.

CREATE TABLE plan_decisions (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id UUID        NOT NULL REFERENCES households (id) ON DELETE CASCADE,
    topic        TEXT        NOT NULL,
    -- Sealed; see the header note.
    body         BYTEA       NOT NULL,
    -- When the household DECIDED — not when the row was created, because a
    -- plan is often written down after the fact and backdated honestly.
    decided_at   DATE        NOT NULL,
    status       TEXT        NOT NULL DEFAULT 'confirmed' CHECK (status IN ('confirmed', 'proposed')),
    -- The decision this one replaces. Household scoping of the target is
    -- enforced by the API layer (it reads the target through the household-
    -- scoped query first); a FK cannot express that. Cycles are impossible:
    -- a row can only point at rows that already exist.
    supersedes   UUID        REFERENCES plan_decisions (id) ON DELETE SET NULL,
    source       TEXT        NOT NULL DEFAULT 'manual' CHECK (source IN ('manual', 'advisor')),
    created_by   UUID        REFERENCES users (id) ON DELETE SET NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX plan_decisions_household_idx ON plan_decisions (household_id, decided_at DESC);
CREATE INDEX plan_decisions_supersedes_idx ON plan_decisions (supersedes) WHERE supersedes IS NOT NULL;

-- 3. households.plan_reviewed_at — the "keep updated" mechanic --------------
--
-- A plan that is never re-read rots silently. The stamp is written by an
-- explicit "I have reviewed this" action (POST /api/plan/review) and read by
-- the plan_stale insight producer, which nudges once the newest of {review
-- stamp, any section edit, any confirmed decision} is older than six months.

ALTER TABLE households ADD COLUMN plan_reviewed_at TIMESTAMPTZ;

-- +goose Down
ALTER TABLE households DROP COLUMN IF EXISTS plan_reviewed_at;
DROP TABLE IF EXISTS plan_decisions;
DROP TABLE IF EXISTS plan_sections;
