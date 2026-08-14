-- +goose Up
-- Per-object change history: who changed what, when.
--
-- Every state-changing write on a transaction, budget, or goal records a
-- field-level diff row here — the actor, the field, the old and new values, and
-- a timestamp. The "History" panel on each detail view reads it back. This is
-- the object-mutation counterpart to auth_events: auth_events answers "was that
-- login me?", object_changes answers "who moved this transaction into another
-- category?".
--
-- Two design choices are load-bearing:
--
--  object_id is a plain UUID, NOT a foreign key. History is append-only and must
--  survive the object's deletion, so deleting a transaction leaves a record that
--  it once existed and how it changed. The read path re-resolves visibility by
--  joining back to the source table, so a deleted object's history simply reads
--  as "not visible" once the source row is gone.
--
--  actor_user_id is likewise a plain UUID with no FK. An actor is a historical
--  fact, not a living reference: the person who made the change at time T stays
--  recorded as that user even after their account is removed. Making it a plain
--  UUID also keeps the append-only trigger below honest — an ON DELETE SET NULL
--  from a users cascade is an UPDATE, and would otherwise fire it.
--
--  household_id is ALSO a plain UUID with no FK, for the same trigger reason: a
--  household's ON DELETE CASCADE is a DELETE on every referencing row, and the
--  append-only guard below fires BEFORE DELETE. Households are never removed
--  through the app, so this never orphans a row in production — and making the
--  column plain is what lets the guard be absolute rather than riddled with
--  cascade exceptions. The read path carries household_id from the actor's
--  identity at write time, so it is always correct even without the constraint.

CREATE TABLE object_changes (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id  UUID        NOT NULL,
    -- 'transaction' | 'budget' | 'goal'. More kinds land as handlers are wired;
    -- the CHECK is widened then.
    object_kind   TEXT        NOT NULL,
    object_id     UUID        NOT NULL,
    actor_user_id UUID,
    field         TEXT        NOT NULL,
    -- NULL is meaningful: old_value NULL on a create, new_value NULL on a clear.
    old_value     JSONB,
    new_value     JSONB,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT object_changes_kind_check
        CHECK (object_kind IN ('transaction', 'budget', 'goal'))
);

-- The History panel lists one object's changes newest-first.
CREATE INDEX object_changes_object_idx
    ON object_changes (household_id, object_kind, object_id, created_at DESC);
-- Cross-object "what did I do" reads stay household-scoped (visibility is
-- re-resolved per kind at read time; this index only serves the lookup order).
CREATE INDEX object_changes_actor_idx
    ON object_changes (household_id, actor_user_id, created_at DESC);

-- Append-only, enforced at the database rather than by convention. A correction
-- is a NEW row, never an edit; making UPDATE and DELETE raise here means the
-- invariant does not depend on every caller remembering it. Because no column
-- above carries a foreign key, no referential CASCADE can fire this trigger —
-- the guard is absolute, with no cascade-shaped exception to argue about.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION object_changes_append_only() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'object_changes is append-only: UPDATE and DELETE are not allowed';
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER object_changes_no_update
    BEFORE UPDATE OR DELETE ON object_changes
    FOR EACH ROW EXECUTE FUNCTION object_changes_append_only();

-- +goose Down
DROP TRIGGER IF EXISTS object_changes_no_update ON object_changes;
DROP FUNCTION IF EXISTS object_changes_append_only();
DROP TABLE IF EXISTS object_changes;
