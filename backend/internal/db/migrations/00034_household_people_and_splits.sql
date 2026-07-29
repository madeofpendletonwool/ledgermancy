-- +goose Up
-- Household people, kid accounts, shared-goal contributions, and bill split.
--
-- The organizing idea: a PERSON and a LOGIN are different things.
--
-- `users` is credentials — email, password hash, a permission level. `people` is
-- who money can be ABOUT. A six-year-old with a 529 is a person; they are not a
-- login, and requiring them to be one would put fabricated credentials in the
-- auth table for every child in the app.
--
-- Every login belongs to a person. Not every person has a login.

-- --------------------------------------------------------------------------
-- 1. People
-- --------------------------------------------------------------------------

CREATE TABLE household_people (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id UUID NOT NULL REFERENCES households (id) ON DELETE CASCADE,

    -- Set iff this person can sign in. NULL is the normal, expected state for a
    -- young child. ON DELETE SET NULL, not CASCADE: revoking a teenager's login
    -- must not delete the person their 529 and their savings bonds point at.
    user_id      UUID UNIQUE REFERENCES users (id) ON DELETE SET NULL,

    display_name TEXT NOT NULL,

    -- The point of this table. Nullable because an adult may not want to give
    -- one, and every consumer must handle its absence rather than defaulting: a
    -- defaulted birthdate produces a confidently wrong retirement horizon.
    birthdate    DATE,

    -- Explicit, not inferred from age. Dependency is a household's own call and
    -- does not flip on an 18th birthday. Drives asset grouping and segregation,
    -- never permissions — permissions are `users.role`.
    is_dependent BOOLEAN NOT NULL DEFAULT FALSE,

    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX household_people_household_idx ON household_people (household_id);

-- Every existing user becomes a person carrying their display name. Birthdate
-- is left NULL — there is no source for it, and inventing one would feed a
-- fabricated age into every projection downstream.
INSERT INTO household_people (household_id, user_id, display_name)
SELECT household_id, id, display_name FROM users;

-- --------------------------------------------------------------------------
-- 2. Roles
-- --------------------------------------------------------------------------

-- 'owner' created the household; 'member' is a full adult member (today's
-- behaviour, and the default so no existing session changes); 'child' is a
-- deliberately reduced login a parent enables.
ALTER TABLE users ADD COLUMN role TEXT NOT NULL DEFAULT 'member'
    CHECK (role IN ('owner', 'member', 'child'));

-- The earliest user in each household becomes its owner. The id tiebreaker
-- makes this deterministic when two users share a created_at.
UPDATE users u SET role = 'owner'
WHERE u.id = (
    SELECT id FROM users
    WHERE household_id = u.household_id
    ORDER BY created_at ASC, id ASC
    LIMIT 1
);

-- An invite now carries the role it grants and, optionally, the existing person
-- it attaches to — that is how "enable a login for Ellie" works without
-- creating a second Ellie.
ALTER TABLE household_invites
    ADD COLUMN role TEXT NOT NULL DEFAULT 'member'
        CHECK (role IN ('owner', 'member', 'child')),
    ADD COLUMN person_id UUID REFERENCES household_people (id) ON DELETE CASCADE;

-- --------------------------------------------------------------------------
-- 3. Whose money is it
-- --------------------------------------------------------------------------

-- The person an account exists FOR: a 529's beneficiary, the minor on a UTMA,
-- the child on a Trump account, the kid whose custodial Roth this is.
--
-- This is the 529 sense of "beneficiary" — whose money this is — and NOT the
-- payable-on-death sense. The UI must say so; the two are routinely confused
-- and they mean opposite things about who controls the account today.
--
-- Deliberately NOT joint ownership. A joint checking account has two adult
-- owners and this column cannot express that; adult accounts keep working
-- exactly as they do now, shared through `plaid_items.is_shared`. NULL for
-- anything not held for a specific person, which is most accounts.
ALTER TABLE accounts ADD COLUMN beneficiary_person_id UUID
    REFERENCES household_people (id) ON DELETE SET NULL;

CREATE INDEX accounts_beneficiary_idx ON accounts (beneficiary_person_id)
    WHERE beneficiary_person_id IS NOT NULL;

-- Same, for things Plaid cannot see: savings bonds in a child's name, a
-- passbook account at a credit union, cash in a birthday envelope.
ALTER TABLE manual_assets ADD COLUMN person_id UUID
    REFERENCES household_people (id) ON DELETE SET NULL;

CREATE INDEX manual_assets_person_idx ON manual_assets (person_id)
    WHERE person_id IS NOT NULL;

-- Custodial and education accounts are not the household's money in the same
-- sense the household's brokerage is. ProjectRetirement already excludes a 529
-- from the nest egg; UTMA/UGMA needs the same treatment for a stronger reason —
-- it is irrevocably the child's property the moment it is funded. Coverdell,
-- a custodial Roth, and the Trump account join the list.
ALTER TABLE accounts DROP CONSTRAINT accounts_tax_treatment_check;
ALTER TABLE accounts ADD CONSTRAINT accounts_tax_treatment_check
    CHECK (tax_treatment IN (
        'taxable', 'trad_401k', 'roth_401k', 'trad_ira', 'roth_ira',
        '529', 'hsa', 'trust', 'other',
        'utma_ugma', 'coverdell', 'custodial_roth', 'trump'
    ));

-- --------------------------------------------------------------------------
-- 4. Allowance
-- --------------------------------------------------------------------------

-- The schedule. NULLs mean "not configured", not zero.
CREATE TABLE allowances (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    person_id     UUID NOT NULL UNIQUE REFERENCES household_people (id) ON DELETE CASCADE,
    amount        NUMERIC(20, 4) CHECK (amount >= 0),
    cadence       TEXT CHECK (cadence IN ('weekly', 'biweekly', 'monthly')),
    monthly_limit NUMERIC(20, 4) CHECK (monthly_limit >= 0),

    -- Whether the scheduled job posts credits automatically, or a parent does it
    -- by hand. Default off: money appearing without a parent's action is the
    -- wrong default for a tool whose point is teaching where money comes from.
    auto_post     BOOLEAN NOT NULL DEFAULT FALSE,

    -- The last period boundary auto_post paid for. The job keys idempotency on
    -- this, so a queue that delivers twice does not pay twice.
    last_posted_for DATE,

    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- An amount without a cadence is not a schedule, and a cadence without an
    -- amount pays nothing. Both or neither.
    CONSTRAINT allowances_amount_needs_cadence
        CHECK ((amount IS NULL) = (cadence IS NULL))
);

-- The ledger. This is the teaching surface: a running balance a child watches
-- go up when they earn and down when they spend.
--
-- SIGN CONVENTION — read this. `amount` is POSITIVE for money INTO the child's
-- balance and NEGATIVE for money out. That is the OPPOSITE of
-- `transactions.amount`, where positive means money out. The inversion is
-- deliberate: this table is a balance, not a spend feed, and a child's balance
-- reading negative-when-saving would be indefensible in a UI aimed at a
-- ten-year-old. Nothing joins these two tables; keep it that way.
--
-- These rows are NOT transactions. There is no account behind them, they are
-- not reconciled against anything, and they must never enter household spending
-- totals.
CREATE TABLE allowance_entries (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    person_id   UUID NOT NULL REFERENCES household_people (id) ON DELETE CASCADE,
    kind        TEXT NOT NULL
        CHECK (kind IN ('allowance', 'chore', 'gift', 'spend', 'correction')),
    amount      NUMERIC(20, 4) NOT NULL,
    occurred_on DATE NOT NULL DEFAULT CURRENT_DATE,
    note        TEXT,

    -- Who recorded it. A child may record their own spending; a parent records
    -- credits. NULL when the scheduled job posted it.
    created_by  UUID REFERENCES users (id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX allowance_entries_person_idx
    ON allowance_entries (person_id, occurred_on DESC);

-- --------------------------------------------------------------------------
-- 5. Shared goals and bill split
-- --------------------------------------------------------------------------

-- Who contributed what to a shared goal. Progress for an account-linked goal
-- still derives from the account balance; this records ATTRIBUTION, and the two
-- must not be conflated into a second source of truth for progress.
CREATE TABLE goal_contributions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    goal_id     UUID NOT NULL REFERENCES goals (id) ON DELETE CASCADE,
    person_id   UUID NOT NULL REFERENCES household_people (id) ON DELETE CASCADE,
    amount      NUMERIC(20, 4) NOT NULL,
    occurred_on DATE NOT NULL DEFAULT CURRENT_DATE,
    note        TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX goal_contributions_goal_idx ON goal_contributions (goal_id);

-- A transaction split across household members.
--
-- A split is an ATTRIBUTION OVERLAY. The transaction still happened once, on
-- one account, and household spending totals must be unchanged by splitting it.
-- Only per-person views consult this table.
CREATE TABLE transaction_splits (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    transaction_id UUID NOT NULL REFERENCES transactions (id) ON DELETE CASCADE,
    person_id      UUID NOT NULL REFERENCES household_people (id) ON DELETE CASCADE,

    -- The exact share. Percentages are a UI convenience; what is stored is the
    -- resolved amount, so shares always sum to the transaction exactly and no
    -- rounding drift accumulates.
    amount         NUMERIC(20, 4) NOT NULL,

    settled_at     TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (transaction_id, person_id)
);

CREATE INDEX transaction_splits_person_idx
    ON transaction_splits (person_id, settled_at);

-- A goal can belong to a person who has no login — a child's bike fund is the
-- canonical case. `scope='user'` is retained unchanged so nothing shipped
-- breaks.
ALTER TABLE goals DROP CONSTRAINT goals_scope_check;
ALTER TABLE goals DROP CONSTRAINT goals_user_scope;

ALTER TABLE goals ADD COLUMN person_id UUID
    REFERENCES household_people (id) ON DELETE CASCADE;

ALTER TABLE goals ADD CONSTRAINT goals_scope_check
    CHECK (scope IN ('user', 'person', 'household'));

ALTER TABLE goals ADD CONSTRAINT goals_scope_target CHECK (
       (scope = 'user'      AND user_id IS NOT NULL AND person_id IS NULL)
    OR (scope = 'person'    AND person_id IS NOT NULL AND user_id IS NULL)
    OR (scope = 'household' AND user_id IS NULL AND person_id IS NULL)
);

CREATE INDEX goals_person_idx ON goals (person_id) WHERE person_id IS NOT NULL;

-- --------------------------------------------------------------------------
-- 6. Deprecated age columns
-- --------------------------------------------------------------------------

-- These are stored INTEGERS: correct on the day they are typed and wrong every
-- year after. With a birthdate on household_people they become the fallback,
-- not the answer. Resolution order everywhere an age is needed:
--
--   1. the linked person's birthdate, aged to the projection's `now` parameter
--   2. the stored integer, when there is no person or no birthdate
--   3. neither — surface it (ok=false), never substitute
--
-- Left in place because doc 15 shipped reading them and an account with no
-- person linked still needs a source. Do not write them from new code.
COMMENT ON COLUMN projection_assumptions.current_age IS
    'DEPRECATED: prefer household_people.birthdate. A stored age decays.';
COMMENT ON COLUMN account_contributions.beneficiary_current_age IS
    'DEPRECATED: prefer accounts.beneficiary_person_id -> birthdate.';

-- --------------------------------------------------------------------------
-- 7. Triggers
-- --------------------------------------------------------------------------

-- +goose StatementBegin
DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['household_people', 'allowances'] LOOP
        EXECUTE format(
            'CREATE TRIGGER %I_set_updated_at BEFORE UPDATE ON %I
             FOR EACH ROW EXECUTE FUNCTION set_updated_at()', t, t);
    END LOOP;
END $$;
-- +goose StatementEnd

-- +goose Down
DROP INDEX IF EXISTS goals_person_idx;
ALTER TABLE goals DROP CONSTRAINT IF EXISTS goals_scope_target;
ALTER TABLE goals DROP CONSTRAINT IF EXISTS goals_scope_check;
ALTER TABLE goals DROP COLUMN IF EXISTS person_id;
ALTER TABLE goals ADD CONSTRAINT goals_scope_check
    CHECK (scope IN ('user', 'household'));
ALTER TABLE goals ADD CONSTRAINT goals_user_scope CHECK (
       (scope = 'user' AND user_id IS NOT NULL)
    OR (scope = 'household' AND user_id IS NULL)
);

DROP TABLE IF EXISTS transaction_splits, goal_contributions,
    allowance_entries, allowances CASCADE;

ALTER TABLE accounts DROP CONSTRAINT IF EXISTS accounts_tax_treatment_check;
ALTER TABLE accounts ADD CONSTRAINT accounts_tax_treatment_check
    CHECK (tax_treatment IN (
        'taxable', 'trad_401k', 'roth_401k', 'trad_ira', 'roth_ira',
        '529', 'hsa', 'trust', 'other'
    ));

ALTER TABLE manual_assets DROP COLUMN IF EXISTS person_id;
ALTER TABLE accounts DROP COLUMN IF EXISTS beneficiary_person_id;
ALTER TABLE household_invites DROP COLUMN IF EXISTS person_id;
ALTER TABLE household_invites DROP COLUMN IF EXISTS role;
ALTER TABLE users DROP COLUMN IF EXISTS role;

DROP TABLE IF EXISTS household_people CASCADE;
