# 21 — Household people, kid accounts, shared goals, bill split

*(TODO.md "Next major initiatives" #9.)*

## Context

The household model is the app's structural advantage over single-user
competitors like Rocket Money, and it is under-used. What exists today:

- `households` / `users` with `users.household_id` (`00001_core_schema.sql:14-29`)
  and invite-only joining via `household_invites`.
- Per-institution sharing (`plaid_items.is_shared`), enforced as
  `(i.user_id = $2 OR i.is_shared)` in every reporting query.
- `goals.scope` already accepts `'household'` (`00012_goals.sql:12-14`).

What does not exist:

- **Any concept of a person who is not a login.** `users` requires
  `email NOT NULL` (globally unique on `lower(email)`) and
  `password_hash NOT NULL`. There is no way to say "this 529 is for my
  six-year-old" without minting credentials for a six-year-old.
- **Any way to attach an account to the person it is for.** A 529, a UTMA, a
  custodial Roth, a Trump account, and a pile of savings bonds are all *for* a
  specific child. The schema can record none of that, so all of it lands in the
  household's undifferentiated asset pile.
- **A birthdate anywhere.** The app instead stores ages, which decay:
  `projection_assumptions.current_age` (`00021:38`) and
  `account_contributions.beneficiary_current_age` (`00021:79`) are both correct
  on the day they are typed and wrong every year after. Nobody re-enters them,
  so doc 15's projections quietly drift — and `networth/limits.go` already has
  age-gated catch-up thresholds (`CatchupAge = 50`, `SuperCatchupMin/Max`,
  `HSACatchupAge = 55`) reading an age the app has no reliable source for.
- **Any role on `users`.** Every member is equal. Kid logins are impossible
  without one, and this is the doc that adds it.
- **Multi-member goal contributions.** A household goal exists but has no notion
  of *who funded what*.
- **Any shared-expense tracking.** "Who paid for what this month" has no in-app
  answer, which is the single most common friction in a two-person household.

### The distinction this doc is built on

A **person** and a **login** are different things, and fusing them is why the
first draft of this doc could only model children as restricted users.

- A **person** is someone in the household who money can be *about*. They have a
  name and a birthdate. A newborn with a 529 is a person. They need no
  credentials and may never sign in.
- A **login** (`users`) is a set of credentials with a permission level. Adults
  have one. A teenager may be given one — deliberately, by a parent, as a
  teaching tool. A toddler will not be.

Every login belongs to a person. Not every person has a login. Everything below
follows from that.

## AI vs deterministic split

**Deterministic:** every split, balance, contribution, allowance, age, and
horizon figure. Exact decimal; ages from dates, never from stored integers.

**AI:** none required. A natural-language "split this 60/40 with Sam" is a
plausible later extension through the existing `ParseRule` pattern; not this doc.

## Prerequisites

None strictly. Parallel-safe with everything else in waves 3–5.

**Note for doc 16:** that doc needs an owner/admin check for its continuity
endpoint and observes that no role exists. This doc adds `users.role`. If 16 is
in flight, coordinate — 16 should consume the role added here rather than
inventing a parallel mechanism, and whichever lands second adopts the other's.

**Note for doc 15 (shipped):** this doc supersedes the two stored-age columns
without dropping them. See "Ages come from birthdates" below; the resolution
order is load-bearing and doc 15's engine changes.

**Note for doc 26:** savings bonds held for a child attach to a person through
`manual_assets.person_id`, added here. 26 adds the bond valuation itself. The
two are independent — neither blocks the other, and if 26 lands first its bond
assets simply have no person until this doc lands.

## Data model

**Reserved migration: `00025_household_people_and_splits.sql`.**

```sql
-- --------------------------------------------------------------------------
-- 1. People
-- --------------------------------------------------------------------------

-- Someone in the household that money can be about. Distinct from `users`,
-- which is credentials. A child with a 529 is a person; they are not a login,
-- and requiring them to be one would put fabricated credentials in the auth
-- table for every kid in the app.
CREATE TABLE household_people (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id UUID NOT NULL REFERENCES households (id) ON DELETE CASCADE,

    -- Set iff this person can sign in. NULL is the normal, expected state for a
    -- young child. ON DELETE SET NULL, not CASCADE: revoking a teenager's login
    -- must not delete the person their 529 and their bonds point at.
    user_id      UUID UNIQUE REFERENCES users (id) ON DELETE SET NULL,

    display_name TEXT NOT NULL,

    -- The point of this table. Nullable because an adult may not want to give
    -- one, and every consumer must handle its absence rather than defaulting.
    -- A defaulted birthdate produces a confidently wrong retirement horizon.
    birthdate    DATE,

    -- Explicit, not inferred from age. Dependency is a household's own call and
    -- does not flip on an 18th birthday. Drives asset segregation and grouping,
    -- never permissions — permissions are `users.role`.
    is_dependent BOOLEAN NOT NULL DEFAULT FALSE,

    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX household_people_household_idx ON household_people (household_id);

-- Backfill: every existing user becomes a person carrying their display name.
-- Birthdate is left NULL — there is no source for it and inventing one would
-- feed a fabricated age into every projection.
INSERT INTO household_people (household_id, user_id, display_name)
SELECT household_id, id, display_name FROM users;

-- --------------------------------------------------------------------------
-- 2. Roles
-- --------------------------------------------------------------------------

-- 'owner' created the household; 'member' is a full adult member (today's
-- behaviour); 'child' is a deliberately reduced login a parent enables.
ALTER TABLE users ADD COLUMN role TEXT NOT NULL DEFAULT 'member'
    CHECK (role IN ('owner','member','child'));

-- The earliest user in each household becomes its owner.
UPDATE users u SET role = 'owner'
WHERE u.id = (
    SELECT id FROM users
    WHERE household_id = u.household_id
    ORDER BY created_at ASC, id ASC LIMIT 1
);

-- An invite now carries the role it grants and, optionally, the existing person
-- it attaches to — that is how "enable a login for Ellie" works without
-- creating a second Ellie.
ALTER TABLE household_invites
    ADD COLUMN role TEXT NOT NULL DEFAULT 'member'
        CHECK (role IN ('owner','member','child')),
    ADD COLUMN person_id UUID REFERENCES household_people (id) ON DELETE CASCADE;

-- --------------------------------------------------------------------------
-- 3. Whose money is it
-- --------------------------------------------------------------------------

-- The person an account exists FOR: a 529's beneficiary, the minor on a UTMA,
-- the child on a Trump account, the kid whose custodial Roth this is.
--
-- This is the 529 sense of "beneficiary" — whose money this is — and NOT the
-- payable-on-death sense. Say so in the UI label; the two are routinely
-- confused and mean opposite things about who controls the account today.
--
-- Deliberately NOT joint ownership. A joint checking account has two adult
-- owners and this column cannot express that; adult accounts keep working
-- exactly as they do now, shared through `plaid_items.is_shared`. Leave it NULL
-- for anything that is not held for a specific person.
ALTER TABLE accounts ADD COLUMN beneficiary_person_id UUID
    REFERENCES household_people (id) ON DELETE SET NULL;

CREATE INDEX accounts_beneficiary_idx ON accounts (beneficiary_person_id)
    WHERE beneficiary_person_id IS NOT NULL;

-- Same, for things Plaid cannot see: savings bonds in a child's name, a
-- passbook account at a credit union, cash in a birthday envelope.
ALTER TABLE manual_assets ADD COLUMN person_id UUID
    REFERENCES household_people (id) ON DELETE SET NULL;

-- Custodial and education accounts are not the household's money in the same
-- sense the household's brokerage is. `retirement.go:57` already excludes a 529
-- from the nest egg; UTMA/UGMA needs the same treatment for a stronger reason —
-- it is irrevocably the child's property the moment it is funded. Coverdell and
-- the Trump account join the list. Limits for the new codes go in
-- `networth/limits.go` beside the existing ones, per tax year.
--
-- Same naming caveat as `goals_scope_check` below: 00020 declared this inline,
-- so confirm the generated name with `\d accounts` first.
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
    amount        NUMERIC(20,4) CHECK (amount >= 0),
    cadence       TEXT CHECK (cadence IN ('weekly','biweekly','monthly')),
    monthly_limit NUMERIC(20,4) CHECK (monthly_limit >= 0),
    -- Whether the scheduled job posts credits automatically, or a parent does it
    -- by hand. Default off: money appearing without a parent's action is the
    -- wrong default for a tool whose point is teaching where money comes from.
    auto_post     BOOLEAN NOT NULL DEFAULT FALSE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
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
CREATE TABLE allowance_entries (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    person_id   UUID NOT NULL REFERENCES household_people (id) ON DELETE CASCADE,
    -- allowance | chore | gift | spend | correction
    kind        TEXT NOT NULL CHECK (kind IN ('allowance','chore','gift','spend','correction')),
    amount      NUMERIC(20,4) NOT NULL,
    occurred_on DATE NOT NULL DEFAULT CURRENT_DATE,
    note        TEXT,
    -- Who recorded it. A child may record their own spending; a parent records
    -- credits. NULL if the scheduled job posted it.
    created_by  UUID REFERENCES users (id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX allowance_entries_person_idx
    ON allowance_entries (person_id, occurred_on DESC);

-- --------------------------------------------------------------------------
-- 5. Shared goals and bill split
-- --------------------------------------------------------------------------

-- Who contributed what to a shared goal. Progress for an account-linked goal
-- still derives from the account balance; this records attribution, and the two
-- must not be conflated (see Backend).
CREATE TABLE goal_contributions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    goal_id     UUID NOT NULL REFERENCES goals (id) ON DELETE CASCADE,
    person_id   UUID NOT NULL REFERENCES household_people (id) ON DELETE CASCADE,
    amount      NUMERIC(20,4) NOT NULL,
    occurred_on DATE NOT NULL DEFAULT CURRENT_DATE,
    note        TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX goal_contributions_goal_idx ON goal_contributions (goal_id);

-- A transaction split across household members.
CREATE TABLE transaction_splits (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    transaction_id UUID NOT NULL REFERENCES transactions (id) ON DELETE CASCADE,
    person_id      UUID NOT NULL REFERENCES household_people (id) ON DELETE CASCADE,
    -- The exact share. Percentages are a UI convenience; what is stored is the
    -- resolved amount, so shares always sum to the transaction exactly and no
    -- rounding drift accumulates.
    amount         NUMERIC(20,4) NOT NULL,
    settled_at     TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (transaction_id, person_id)
);
CREATE INDEX transaction_splits_person_idx ON transaction_splits (person_id, settled_at);

-- A goal can belong to a person who has no login — a child's bike fund is the
-- canonical case. `scope='user'` is retained unchanged so nothing shipped
-- breaks.
--
-- NOTE: `goals_user_scope` is explicitly named in `00012_goals.sql:25`, so that
-- name is reliable. `goals_scope_check` is NOT — it is an inline column check
-- and the name below is only Postgres's usual generated form. Confirm with
-- `\d goals` before writing this migration.
ALTER TABLE goals DROP CONSTRAINT goals_scope_check;
ALTER TABLE goals DROP CONSTRAINT goals_user_scope;
ALTER TABLE goals ADD COLUMN person_id UUID
    REFERENCES household_people (id) ON DELETE CASCADE;
ALTER TABLE goals ADD CONSTRAINT goals_scope_check
    CHECK (scope IN ('user','person','household'));
ALTER TABLE goals ADD CONSTRAINT goals_scope_target CHECK (
       (scope = 'user'      AND user_id IS NOT NULL AND person_id IS NULL)
    OR (scope = 'person'    AND person_id IS NOT NULL AND user_id IS NULL)
    OR (scope = 'household' AND user_id IS NULL AND person_id IS NULL)
);
```

### Ages come from birthdates

`projection_assumptions.current_age` and
`account_contributions.beneficiary_current_age` are **not dropped** — doc 15
shipped reading them, and an account with no person linked still needs a source.
They become the fallback, not the answer. Resolution order, everywhere an age is
needed:

1. The linked person's `birthdate`, aged to the projection's `now` parameter.
2. The stored integer, if there is no person or no birthdate.
3. Neither: the same `ok=false` path `AnnualLimitFor` already uses for an
   unconfigured tax year. Surface it; never substitute.

Mark both columns deprecated in a comment in this migration and stop writing
them from any new code path. A decaying value left undocumented in the schema
will be read by someone.

`networth.ProjectRetirement` takes `now` as a parameter by design — derive ages
against that parameter, not against the clock, or the tests become
calendar-dependent (doc 15's note, and it applies harder here).

## Backend

### Roles

Add a role check to `backend/internal/auth/middleware.go` alongside the existing
identity plumbing. Enforced server-side on every route — a client-side role check
is decoration.

`child` can: sign in, see their own allowance balance and ledger, record their
own spending, see their own goals and progress, and see the balance and holdings
of accounts where they are the `beneficiary_person_id` — read-only.

`child` cannot: see household net worth, any other person's transactions,
accounts, goals or allowance, the household split ledger, or any projection over
household assets. Cannot link or unlink institutions, cannot invite, cannot
change household settings, cannot change their own allowance or limits.

Audit the **existing** handlers when adding this. A role that is checked on new
routes only is worse than no role, because it implies protection that isn't
there. Enumerate every mutating route and decide explicitly.

### Enabling a login for a person

Reuse `household_invites` with the new `role` and `person_id` columns. Accepting
an invite bound to a `person_id` creates the user and sets
`household_people.user_id` rather than creating a second person.

**`users.email` stays required, and a child login needs a real address.** The
temptation is to allow a synthetic one (`ellie@household.local`) so a parent can
just type a password. Do not — password reset and every security notification in
`00006` would fail silently against an address that does not exist, and the
failure surfaces at exactly the worst moment. A parent giving a nine-year-old a
login uses a plus-address they control (`parent+ellie@gmail.com`), which is a
real deliverable address. Say this in the invite UI rather than leaving them to
discover it.

Do not force MFA enrolment on a `child` role.

### Ages, and what they unlock

Once a birthdate exists, several things that are currently unanswerable become
deterministic. Wire these as part of this doc — they are the reason the column
is worth adding, and each is a small change:

- **529 horizon** derives from the beneficiary's birthdate rather than a typed
  age that was right once (`retirement.go:344`, `educationHorizonPassed`).
- **Catch-up contributions.** `AnnualLimitFor` already takes an `age` and
  `limits.go` already encodes `CatchupAge`, the SECURE 2.0 60–63 band, and
  `HSACatchupAge`. Today the age it receives is a decaying integer.
- **"When you're 65"** instead of "in 30 years" throughout the retirement UI.
  Same numbers, and far easier to sanity-check against your own life.
- **RMDs and Social Security claiming age** — doc 15 lists RMDs as not modelled
  and doc 28 wants claiming-age scenarios. Both need a birthdate before they can
  be attempted at all. Not built here; unblocked here.

### Kid accounts, and the teaching surface

The child-facing view is the feature, not a byproduct. Three things, all
deterministic:

- **Allowance balance and ledger.** Sum `allowance_entries.amount`. Never store
  a balance. This is play money and a record — it is not a bank balance, is not
  reconciled against any real account, and the UI must not let a child think it
  is. Label it as what it is.
- **Goal progress.** A `scope='person'` goal with a target and a bar. The
  existing goals machinery does this already; it just needs the person scope.
- **Growth over their horizon.** For an account where the child is the
  beneficiary: "at 18, on current contributions, this is projected to be $X."
  Reuse `networth.ProjectRetirement`'s education-horizon path
  (`retirement.go:48`, `isEducation`) — do not write a second compounding
  engine. Every figure carries its assumptions, same as every other projection
  in the app.

The scheduled allowance post, when `auto_post` is on, belongs in `jobs/jobs.go`
beside the existing periodic work. It must be idempotent on
`(person_id, period)` — a job that runs twice must not pay twice.

**Allowance entries never enter household spending totals.** They are not
transactions and there is no account behind them. This is the same rule as
splits below, and breaking it inflates household spend by the child's pocket
money.

### Shared goals

Extend the goals handlers so a `scope='household'` goal accepts contributions
from any adult member, with a per-person view and a funding history.

Keep the `00012` header rule intact: **progress is derived, never stored.** For
an account-linked goal, progress remains the account balance;
`goal_contributions` records *attribution* and must not become a second,
divergent source of truth for progress. For an unlinked goal, contributions are
the natural progress source — document which applies where, because a reader will
otherwise assume the wrong one.

### Bill split and the household ledger

- Split a transaction: equal, custom percentages, or exact amounts.
- **Store resolved amounts, not percentages.** A 3-way split of $100 is
  33.34/33.33/33.33, and the remainder has to land somewhere deterministic.
  Resolve in the API, store exact decimals, and **assert the shares sum to the
  transaction amount** — a split that does not sum is a silent reporting error.
- A running balance per person pair: "Sam owes Alex $240." Derived by summing
  unsettled splits — do not store a balance.
- Settle: stamp `settled_at`. No money moves; the app records that it did.

**Splits must not double-count in reports.** This is the trap. A split is an
*attribution* overlay: the transaction still happened once, on one account, and
household spending totals must be unchanged by splitting it. Only per-person
views consult splits. Anything else and the app starts inflating spend the same
way un-typed transfers used to.

### Net worth and whose money it is

With `beneficiary_person_id` and `manual_assets.person_id` populated, net worth
gains a per-person lens. Two rules:

- **The household total does not change.** A child's 529 was already in the
  household's assets and stays there. This is a *breakdown*, exactly like
  `net_worth_snapshots.breakdown`, not a new sum.
- **Custodial money is segregated from the retirement nest egg.** `529` already
  is (`retirement_test.go:243`). `utma_ugma`, `coverdell`, `custodial_roth` and
  `trump` join it. Counting a child's irrevocable property as retirement savings
  overstates the household's position by the whole balance, and it is the kind
  of error nobody catches because the number moves in the flattering direction.

## Frontend

- `Household.tsx` — a **People** section: add a person, set birthdate, mark
  dependent. Role management (owner only). "Enable a login" on a person, which
  issues the role-bound invite. Allowance schedule and limit editing.
- `Accounts.tsx` — a "for whom" selector on an account, alongside the existing
  tax-treatment tagging. Suggest nothing: an unassigned account stays
  unassigned, same rule `SuggestTaxTreatment` follows.
- `NetWorth.tsx` — a per-person breakdown, with custodial assets visibly
  separated rather than folded into the household total.
- `Retirement.tsx` — ages rendered as ages ("at 67") wherever a birthdate is
  known, with the derived basis shown.
- `Goals.tsx` — contributor breakdown and "who funded what" history on shared
  goals; person-scoped goals for kids.
- `Transactions.tsx` — a split action on a row, a split indicator, and a "shared
  expenses" filter.
- A **household ledger** view: who owes whom, with settle actions.
- A **child view** that is genuinely reduced, not the adult UI with sections
  hidden — hidden sections leak through URLs. Its own route tree, its own
  navigation, and it should read like it was built for a kid: their balance,
  their goals, their savings growing.

## Verification

- `go test -p 1 ./...`.
- **Backfill:** one `household_people` row per pre-existing user, name carried,
  `user_id` linked, `birthdate` NULL. Exactly one owner per household —
  including a single-user household and one with identical `created_at` values
  (the `id` tiebreaker).
- **A person with no login is fully usable:** create a person with no `user_id`,
  attach a 529 and a manual asset, set an allowance, post entries, create a
  person-scoped goal. No row in `users`.
- **Age derivation.** A linked birthdate beats a stored `current_age`; with no
  birthdate the stored value is used; with neither, `ok=false` propagates rather
  than a default. Assert against a fixed `now`, not the clock. Include a
  birthday-boundary case (the day before and the day of).
- **The double-counting assertions**, each an exact decimal:
  - household spending totals before and after splitting a transaction are
    byte-identical;
  - household spending totals are unchanged by any number of
    `allowance_entries`;
  - household net worth is unchanged by assigning a `beneficiary_person_id`.
- Splits sum exactly to the transaction amount, including the 3-way $100 case
  and a $0.01 transaction.
- **Custodial segregation:** a `utma_ugma` account is excluded from the
  retirement nest egg, mirroring `TestEducationAccountNotInNestEgg`.
- **Role enforcement, per route.** A `child` session gets 403/404 on every
  restricted endpoint, tested individually rather than by sampling. Include the
  pre-existing routes. Assert a child *can* read their own beneficiary account
  and *cannot* read a sibling's.
- Allowance balance nets correctly across mixed credit/spend entries; the
  auto-post job run twice in one period posts once.
- Ledger balance nets correctly across mixed unsettled and settled splits.
- Deleting a user with `role='child'` leaves their `household_people` row, their
  accounts' `beneficiary_person_id`, and their allowance history intact.
- Frontend `tsc -b`, `vite build`, `oxlint`.

## Out of scope

- Actually moving money between members.
- Splitting with people outside the household.
- Joint ownership of an account by two adults. `beneficiary_person_id` is
  single-valued on purpose; adult accounts keep working as they do today.
- Usernames as a login identifier instead of emails. It would make child logins
  tidier and it is a real auth change with its own blast radius.
- Plaid-linking an account in a child's name. Custodial accounts at a broker are
  linked by the parent and tagged with `beneficiary_person_id`; that is the
  supported path.
- Chore assignment and completion tracking. The `chore` entry kind records that
  money was earned for one; managing the chores themselves is a different app.
- Natural-language split parsing.
