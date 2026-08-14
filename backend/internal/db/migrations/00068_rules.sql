-- +goose Up
-- Rules: user-editable IF-THEN over transactions.
--
-- WHAT THIS IS. A rule is one or more TRIGGERS joined by AND ("description
-- contains Starbucks" AND "amount is more than 10") and one or more ACTIONS
-- applied to every transaction that matches ("set category to Coffee", "add tag
-- Caffeine"). Rules fire when a transaction arrives and can be re-run against
-- everything already stored.
--
-- WHY IT IS A TABLE AND NOT MORE CODE. The app already automates three of these
-- decisions in three separate hard-coded places: internal/categorize resolves a
-- category, the ledger's "apply to all from this merchant" row action rewrites
-- history for one descriptor, and internal/alerts evaluates user config into
-- events. Only the third is user-editable, and none of them can express "and
-- also tag it, and also write a note". This table is the one surface where the
-- household states the automation it wants, in its own words, once.
--
-- Numbering: 00066 is taken by the unmerged `outgoing-webhooks` branch and
-- 00067 by the unmerged `transaction-links` branch, so this one takes 00068 to
-- stay above everything any of those can apply. Goose refuses a migration
-- numbered below the current version.
--
-- ORDER AGAINST internal/categorize — the thing most likely to be got wrong
-- later. These two do NOT compete, and the reason is stated here as well as in
-- the package doc comment because a schema reader deserves it too:
--
--   categorize answers ONE question, "which category?", from a fixed
--   precedence (manual > category_rules > merchant cache > Plaid's PFC > LLM).
--
--   rules answer a BROADER question, "what else should be true of this row?",
--   and run AFTER categorize has settled the category. A rule's set-category
--   action is the user overriding that answer deliberately, which is why it
--   still refuses to overwrite category_source = 'manual': a hand-filed row is
--   the one thing nothing automatic may touch.
--
-- Note the deliberate name collision hazard: `category_rules` (00001) is the
-- OLD, narrow, pattern-to-category table that internal/categorize reads. It is
-- untouched here and keeps working. `rules` is the new, general one. They are
-- not the same thing and one does not supersede the other yet.

-- A rule is HOUSEHOLD data, like a category, a tag or a budget. Not per-member:
-- "always tag Costco as Groceries" is a statement about the household's money,
-- and two members each maintaining a private half of the automation would mean
-- the same charge is filed differently depending on who synced it.
--
-- Household scope is not a visibility hole. A rule is a statement, not a
-- transaction: WHICH transactions it acts on still runs through the same
-- account_access predicate as every other read (see queries/rules.sql), so a
-- rule never becomes a way to read, or learn the existence of, a charge on the
-- other member's private account.
CREATE TABLE rules (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id UUID NOT NULL REFERENCES households (id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    -- What the rule is for, in the user's own words. Nullable: most rules read
    -- clearly enough from their triggers, and requiring a sentence would just
    -- produce empty strings.
    description  TEXT,
    -- An inactive rule is kept, not deleted. Turning automation off to see
    -- whether it was the cause of something is the normal way to debug it, and
    -- a delete would lose the rule the user spent time building.
    active       BOOLEAN NOT NULL DEFAULT TRUE,
    -- Higher runs first. Ordering is load-bearing rather than cosmetic: rules
    -- are applied in sequence against a transaction that later rules see the
    -- effect of, so "set category to Coffee" (priority 10) can be observed by a
    -- later rule triggering on "category is Coffee".
    priority     INTEGER NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The engine's read: this household's rules, in the order they run. created_at
-- is the tiebreaker so two rules at the same priority have a stable, explicable
-- order (the older one first) rather than whatever the planner returns.
CREATE INDEX rules_household_priority_idx
    ON rules (household_id, priority DESC, created_at);

-- One condition. Every trigger on a rule must hold for the rule to fire: the
-- join is AND, always, with no grouping and no OR.
--
-- That is a deliberate limit rather than an unfinished feature. An OR of two
-- conditions is two rules with the same actions, which a user can already write
-- and can read back later; a nested boolean tree is a query language, and this
-- is a form. Firefly III made the same call for the same reason, and its
-- strict/non-strict switch is the seam to widen here if the limit ever bites.
CREATE TABLE rule_triggers (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    rule_id    UUID NOT NULL REFERENCES rules (id) ON DELETE CASCADE,
    -- The trigger's kind: description_contains, amount_more, account_is, …
    --
    -- Deliberately NOT a CHECK constraint or a Postgres enum. The authoritative
    -- list lives in internal/rules (TriggerType), which validates every write,
    -- and the set is expected to grow — adding "has tag" should be a Go change
    -- with a test, not a migration that has to be deployed in lockstep with the
    -- code that emits the new string. The cost of that choice is that a row
    -- written around the API would simply never match; the engine treats an
    -- unknown type as "does not fire" rather than as an error, so an unreadable
    -- rule can never silently widen into one that matches everything.
    trigger_type TEXT NOT NULL,
    -- The operand, as text, because the operands are not one type: a
    -- description fragment, a decimal amount, a category UUID. Typed columns
    -- would mean five mostly-NULL columns and a CHECK per trigger type;
    -- internal/rules parses and validates the value against its type on write.
    -- Empty string for the types that take no operand (has_no_category).
    value      TEXT NOT NULL DEFAULT '',
    -- NOT. Inverting the trigger rather than adding a mirror type for each one
    -- ("description does not contain") halves the vocabulary the user has to
    -- learn and the engine has to implement.
    invert     BOOLEAN NOT NULL DEFAULT FALSE,
    -- Display order within the rule. Has no effect on matching — an AND is
    -- commutative — but the editor must render the rows back in the order the
    -- user arranged them.
    position   INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX rule_triggers_rule_idx ON rule_triggers (rule_id, position);

-- One thing to do to a matching transaction.
--
-- Actions run in `position` order and, unlike triggers, the order MATTERS:
-- "set notes" followed by "append notes" is not the same as the reverse.
CREATE TABLE rule_actions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    rule_id     UUID NOT NULL REFERENCES rules (id) ON DELETE CASCADE,
    -- set_category, add_tag, set_notes, append_notes. Same reasoning as
    -- trigger_type above: the list lives in internal/rules (ActionType) and is
    -- validated on write, so growing it is a code change rather than a
    -- migration.
    action_type TEXT NOT NULL,
    -- The operand: a category UUID, a tag UUID, or the note text.
    --
    -- A UUID lives here as text rather than as a real foreign key, and that is
    -- the one place this schema gives up referential integrity on purpose.
    -- Typed nullable FK columns (category_id, tag_id, …) would need one column
    -- per action type plus a "exactly one is non-NULL" CHECK, and every new
    -- action type would widen both. The cost is that deleting the target
    -- category leaves an action pointing at nothing — which the engine reports
    -- as a REFUSED action (see stop_on_fail below) rather than an error or, far
    -- worse, a silent no-op the user reads as success.
    value       TEXT NOT NULL DEFAULT '',
    -- When this action cannot be applied, abandon the REST of this rule's
    -- actions for this transaction.
    --
    -- "Cannot be applied" means refused, not "changed nothing": a set-category
    -- blocked because the row was filed by hand, or a target that no longer
    -- exists. An action that was already satisfied (the tag is on the row
    -- already) is a success, and does not stop anything — that is what makes
    -- re-running a rule a no-op instead of a progressively shorter rule.
    stop_on_fail BOOLEAN NOT NULL DEFAULT FALSE,
    position    INTEGER NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX rule_actions_rule_idx ON rule_actions (rule_id, position);

COMMENT ON TABLE rules IS
    'User-editable IF-THEN automation over transactions. Household-scoped. '
    'Runs AFTER internal/categorize has resolved a category, and never '
    'overwrites category_source = ''manual''.';

-- +goose Down

DROP TABLE IF EXISTS rule_actions;
DROP TABLE IF EXISTS rule_triggers;
DROP TABLE IF EXISTS rules;
