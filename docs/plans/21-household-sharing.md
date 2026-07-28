# 21 — Shared goals, bill split, kid sub-accounts

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

- **Any role on `users`.** The table has `id, household_id, email,
  password_hash, display_name, created_at, updated_at` — no role, no permission
  level. Every member is equal. Kid sub-accounts are impossible without adding
  one, and this is the doc that adds it.
- **Multi-member goal contributions.** A household goal exists but has no notion
  of *who funded what*.
- **Any shared-expense tracking.** "Who paid for what this month" has no in-app
  answer, which is the single most common friction in a two-person household.

## AI vs deterministic split

**Deterministic:** every split, balance, and contribution figure. Exact decimal.

**AI:** none required. A natural-language "split this 60/40 with Sam" is a
plausible later extension through the existing `ParseRule` pattern; not this doc.

## Prerequisites

None strictly. Parallel-safe with everything else in waves 3–4.

**Note for doc 16:** that doc needs an owner/admin check for its continuity
endpoint and observes that no role exists. This doc adds `users.role`. If 16 is
in flight, coordinate — 16 should consume the role added here rather than
inventing a parallel mechanism, and whichever lands second adopts the other's.

## Data model

**Reserved migration: `00025_household_roles_and_splits.sql`.**

```sql
-- Roles. 'owner' is the account that created the household; 'member' is a full
-- adult member (today's behaviour); 'child' is limited-permission.
ALTER TABLE users ADD COLUMN role TEXT NOT NULL DEFAULT 'member'
    CHECK (role IN ('owner','member','child'));

-- Backfill: the earliest user in each household becomes its owner.
UPDATE users u SET role = 'owner'
WHERE u.id = (
    SELECT id FROM users
    WHERE household_id = u.household_id
    ORDER BY created_at ASC LIMIT 1
);

-- Who contributed what to a shared goal. Progress for an account-linked goal
-- still derives from the account balance; this records attribution, and the two
-- must not be conflated (see below).
CREATE TABLE goal_contributions (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    goal_id    UUID NOT NULL REFERENCES goals (id)  ON DELETE CASCADE,
    user_id    UUID NOT NULL REFERENCES users (id)  ON DELETE CASCADE,
    amount     NUMERIC(20,4) NOT NULL,
    occurred_on DATE NOT NULL DEFAULT CURRENT_DATE,
    note       TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX goal_contributions_goal_idx ON goal_contributions (goal_id);

-- A transaction split across members.
CREATE TABLE transaction_splits (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    transaction_id UUID NOT NULL REFERENCES transactions (id) ON DELETE CASCADE,
    user_id        UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- The exact share. Percentages are a UI convenience; what is stored is the
    -- resolved amount, so shares always sum to the transaction exactly and no
    -- rounding drift accumulates.
    amount         NUMERIC(20,4) NOT NULL,
    settled_at     TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (transaction_id, user_id)
);
CREATE INDEX transaction_splits_user_idx ON transaction_splits (user_id, settled_at);

-- Child spending limits. NULL = no limit.
CREATE TABLE child_allowances (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id           UUID NOT NULL UNIQUE REFERENCES users (id) ON DELETE CASCADE,
    monthly_amount    NUMERIC(20,4),
    monthly_limit     NUMERIC(20,4),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

**Store resolved amounts, not percentages.** A 3-way split of $100 is
33.34/33.33/33.33, and the remainder has to land somewhere deterministic. Resolve
in the API, store exact decimals, and **assert the shares sum to the transaction
amount** — a split that does not sum is a silent reporting error.

**Splits must not double-count in reports.** This is the trap. A split is an
*attribution* overlay: the transaction still happened once, on one account, and
household spending totals must be unchanged by splitting it. Only per-member
views consult splits. Anything else and the app starts inflating spend the same
way un-typed transfers used to.

## Backend

### Roles

Add a role check to `backend/internal/auth/middleware.go` alongside the existing
identity plumbing. Three levels, enforced server-side on every route — a
client-side role check is decoration.

`child` restrictions: cannot link or unlink institutions, cannot see household
aggregate net worth or other members' transactions, cannot invite, cannot change
household settings. Can see their own allowance, their own spending, and their
own goals.

Audit the **existing** handlers when adding this. A role that is checked on new
routes only is worse than no role, because it implies protection that isn't
there. Enumerate every mutating route and decide explicitly.

### Shared goals

Extend the goals handlers so a `scope='household'` goal accepts contributions
from any adult member, with a per-member view and a funding history.

Keep the `00012` header rule intact: **progress is derived, never stored.** For
an account-linked goal, progress remains the account balance;
`goal_contributions` records *attribution* and must not become a second,
divergent source of truth for progress. For an unlinked goal, contributions are
the natural progress source — document which applies where, because a reader will
otherwise assume the wrong one.

### Bill split and the household ledger

- Split a transaction: equal, custom percentages, or exact amounts.
- A running balance per member pair: "Sam owes Alex $240." Derived by summing
  unsettled splits — do not store a balance.
- Settle: stamp `settled_at`. No money moves; the app records that it did.

### Kid sub-accounts

Reuse the existing invite flow with a role. Allowance is a schedule and a limit,
not real money movement. If a child's spending is visible at all it is through a
shared institution the parent already linked.

## Frontend

- `Household.tsx` — role management (owner only), invite-with-role, allowance
  editing.
- Goals — contributor breakdown and "who funded what" history on shared goals.
- Transactions — a split action on a row, a split indicator, and a "shared
  expenses" filter.
- A **household ledger** view: who owes whom, with settle actions.
- A child-facing view that is genuinely reduced, not the adult UI with sections
  hidden — hidden sections leak through URLs.

## Verification

- `go test -p 1 ./...`.
- **The double-counting assertion:** household spending totals before and after
  splitting a transaction are byte-identical. Assert the exact decimal.
- Splits sum exactly to the transaction amount, including the 3-way $100 case
  and a $0.01 transaction.
- **Role enforcement, per route.** A `child` session gets 403/404 on every
  restricted endpoint, tested individually rather than by sampling. Include the
  pre-existing routes.
- The migration backfill picks exactly one owner per household, including a
  single-user household and one with identical `created_at` values.
- Ledger balance nets correctly across mixed unsettled and settled splits.
- Frontend `tsc -b`, `vite build`, `oxlint`.

## Out of scope

- Actually moving money between members.
- Splitting with people outside the household.
- Per-child bank account linking (children rarely have Plaid-linkable accounts).
- Natural-language split parsing.
