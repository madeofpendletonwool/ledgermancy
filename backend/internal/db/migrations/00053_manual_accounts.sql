-- +goose Up
--
-- Manual accounts, manual investments, and auto-posting scheduled transactions
-- (doc 30).
--
-- Numbered 00053, not the 00047 doc 30 reserved. That reservation was written
-- when 00046 was the highest applied migration; 00048-00052 have landed since,
-- and goose runs in strict-ordering mode (db.go:70) — a 00047 arriving after
-- 00052 is already applied would refuse to start every instance that has run
-- them. Renumbering is the documented response; goose.WithAllowMissing() is
-- not. See docs/plans/README.md.
--
-- The problem this solves: an account that Plaid cannot link — TreasuryDirect,
-- a Voya retirement plan whose link fails every attempt, a private holding —
-- simply cannot exist. `accounts.plaid_item_id` is NOT NULL (00001:98), so
-- there is no row to hang a balance, a holding, or a contribution off. Doc 12
-- closed this for transactions (a manual row attaches to an account that
-- already exists) and explicitly deferred the account itself. This closes it.
--
-- The defining decision is to RELAX the existing tables rather than build a
-- parallel manual-account/manual-holding pair. Every report already filters on
-- visibility and is_active, never on Plaid identity, so a manual row that
-- satisfies the same constraints feeds TWR, allocation, dividends, fee drag and
-- net worth with no engine change at all.

-- --------------------------------------------------------------------------
-- 1. accounts — an account without a Plaid item
-- --------------------------------------------------------------------------

ALTER TABLE accounts ALTER COLUMN plaid_item_id    DROP NOT NULL;
ALTER TABLE accounts ALTER COLUMN plaid_account_id DROP NOT NULL;

-- UNIQUE on a nullable column already permits many NULLs in Postgres, but the
-- constraint is restated as a partial index so the intent is legible: Plaid
-- identity is unique WHERE it exists, and manual rows are simply not in it.
--
-- The sharp edge this opens, and why accounts_plaid_owner below closes it: a
-- NULL plaid_account_id does not collide with anything, so UpsertAccount's
-- ON CONFLICT (plaid_account_id) would stop matching and every sync would
-- INSERT the account again. Dropping NOT NULL for manual rows must therefore be
-- paired with a constraint that still forbids it for Plaid ones, or the feature
-- that lets an account exist without Plaid quietly duplicates every account
-- that has it.
ALTER TABLE accounts DROP CONSTRAINT IF EXISTS accounts_plaid_account_id_key;
DROP INDEX IF EXISTS accounts_plaid_account_id_key;
CREATE UNIQUE INDEX accounts_plaid_account_id_key
    ON accounts (plaid_account_id) WHERE plaid_account_id IS NOT NULL;

-- Ownership and visibility for manual rows.
--
-- Today every scoped read reaches the household through the item:
--   JOIN plaid_items i ON i.id = a.plaid_item_id JOIN users u ON u.id = i.user_id
-- A manual account has no item, so it needs to carry the same three facts
-- itself. The columns are NULLABLE and stay NULL on Plaid rows ON PURPOSE, and
-- doc 30's proposed backfill is deliberately NOT performed:
--
--   Copying user_id/is_shared onto every Plaid account would create a second
--   copy of a value the user can change (PATCH /plaid/items/{id}/sharing) and a
--   value every future sync rewrites. The two copies would drift, silently, in
--   the direction of leaking a private institution into a household view. Left
--   NULL, there is exactly one source of truth per row and the read predicate
--   COALESCE(a.user_id, i.user_id) resolves to the item for Plaid rows and to
--   the account for manual ones. Nothing about Plaid behaviour changes.
--
-- The CHECK constraints below make that either/or structural rather than a
-- convention: a Plaid row MUST carry its item and MUST NOT carry local
-- ownership; a manual row is the exact inverse.
ALTER TABLE accounts
    ADD COLUMN source       TEXT    NOT NULL DEFAULT 'plaid'
        CHECK (source IN ('plaid', 'manual')),
    ADD COLUMN user_id      UUID    REFERENCES users (id) ON DELETE CASCADE,
    ADD COLUMN is_shared    BOOLEAN,
    ADD COLUMN household_id UUID    REFERENCES households (id) ON DELETE CASCADE;

ALTER TABLE accounts
    ADD CONSTRAINT accounts_manual_owner
        CHECK (source <> 'manual' OR (
                   plaid_item_id IS NULL
               AND user_id       IS NOT NULL
               AND household_id  IS NOT NULL
               AND is_shared     IS NOT NULL)),
    ADD CONSTRAINT accounts_plaid_owner
        CHECK (source <> 'plaid' OR (
                   plaid_item_id    IS NOT NULL
               AND plaid_account_id IS NOT NULL
               AND user_id          IS NULL
               AND household_id     IS NULL
               AND is_shared        IS NULL));

CREATE INDEX accounts_manual_owner_idx ON accounts (user_id) WHERE source = 'manual';
CREATE INDEX accounts_household_id_idx ON accounts (household_id)
    WHERE household_id IS NOT NULL;

COMMENT ON COLUMN accounts.source IS
    'plaid — identity and balance owned by the institution. manual — owned by the user; balance writes go through account_balance_history.';

-- --------------------------------------------------------------------------
-- 1b. account_access — one definition of "who can see this account"
-- --------------------------------------------------------------------------

-- Before this migration, every scoped read spelled the rule out by hand:
--
--     JOIN plaid_items i ON i.id = a.plaid_item_id
--     JOIN users u       ON u.id = i.user_id
--     WHERE u.household_id = $1 AND (i.user_id = $2 OR i.is_shared)
--
-- in 85 places. An account that can be owned two different ways cannot be
-- resolved by an INNER JOIN through the item — a manual account has no item, so
-- every one of those 85 queries would silently return nothing for it. Rewriting
-- 85 copies of a COALESCE is how the copies drift, and a drifted copy of THIS
-- rule leaks one member's private institution into the other's view.
--
-- So the rule gets a name and exactly one definition. Callers join it and ask
-- two questions: is this my household, and may I see it.
--
-- A view, not a CTE: Postgres inlines a simple view into the calling query, so
-- the planner still sees one flat join tree and the existing indexes are used
-- exactly as before. A CTE would be an optimisation fence.
--
-- user_id and is_shared are non-NULL in practice for every row — the CHECK
-- constraints above guarantee a manual account has both, and a Plaid account
-- reaches them through an item whose user_id is NOT NULL. They are typed
-- nullable because COALESCE over an outer join cannot prove that; callers must
-- not read that nullability as "unowned".
CREATE VIEW account_access AS
SELECT
    a.id                                     AS account_id,
    COALESCE(a.household_id, u.household_id) AS household_id,
    COALESCE(a.user_id,      i.user_id)      AS user_id,
    COALESCE(a.is_shared,    i.is_shared)    AS is_shared,
    i.institution_name                       AS institution_name
FROM accounts a
LEFT JOIN plaid_items i ON i.id = a.plaid_item_id
LEFT JOIN users       u ON u.id = i.user_id;

COMMENT ON VIEW account_access IS
    'Visibility resolution for accounts of either source. Join this rather than reaching through plaid_items; a manual account has no item.';

-- --------------------------------------------------------------------------
-- 2. account_balance_history — the manual balance trail
-- --------------------------------------------------------------------------

-- Same shape as asset_valuations (00051): an authoritative CURRENT value on the
-- parent row, plus the dated history behind it, written in one transaction.
--
-- This is the first user-owned write path to accounts.current_balance. It is
-- reachable only for source='manual' rows — the queries that write it carry
-- that predicate — so doc 12's "Plaid owns the balance" rule survives intact
-- for every linked account.
CREATE TABLE account_balance_history (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id UUID          NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    as_of      DATE          NOT NULL,
    balance    NUMERIC(20,4) NOT NULL,
    -- manual          — the user typed a new balance.
    -- scheduled       — an auto-posted contribution moved it (§5). This is the
    --                   traceability link back to the obligation that caused it.
    -- holding_revalue — a position was repriced.
    -- fee / dividend  — a cost or a credit the user recorded by hand.
    reason     TEXT          NOT NULL DEFAULT 'manual'
        CHECK (reason IN ('manual', 'scheduled', 'holding_revalue', 'fee', 'dividend')),
    note       TEXT,
    created_at TIMESTAMPTZ   NOT NULL DEFAULT now(),
    -- One balance per account per day. A second write for the same day replaces
    -- it rather than failing: re-running the scheduled worker must be
    -- idempotent, and a user correcting a figure they entered an hour ago is
    -- not a conflict. Matches UpsertInvestmentSnapshot exactly.
    UNIQUE (account_id, as_of)
);

CREATE INDEX account_balance_history_account_idx
    ON account_balance_history (account_id, as_of DESC);

-- --------------------------------------------------------------------------
-- 3. securities and investment_transactions — identity without Plaid
-- --------------------------------------------------------------------------

ALTER TABLE securities ALTER COLUMN plaid_security_id DROP NOT NULL;
ALTER TABLE securities DROP CONSTRAINT IF EXISTS securities_plaid_security_id_key;
DROP INDEX IF EXISTS securities_plaid_security_id_key;
CREATE UNIQUE INDEX securities_plaid_security_id_key
    ON securities (plaid_security_id) WHERE plaid_security_id IS NOT NULL;

-- A manual security is keyed on its ticker instead. ticker_key is the
-- lowercased ticker, stored rather than derived so the partial unique index can
-- use it directly; two manual "VTI" rows are not two securities, they are one
-- security entered twice, and every allocation figure downstream would be
-- wrong.
--
-- Deliberately NOT unique across sources: a household that later links a
-- brokerage through Plaid will get Plaid's own VTI row, and forcing those to
-- collide would mean rewriting the holdings of an account the user still holds
-- manually. They coexist; the manual account keeps pointing at the manual
-- security.
ALTER TABLE securities
    ADD COLUMN source     TEXT NOT NULL DEFAULT 'plaid'
        CHECK (source IN ('plaid', 'manual')),
    ADD COLUMN ticker_key TEXT;

ALTER TABLE securities
    ADD CONSTRAINT securities_manual_ticker
        CHECK (source <> 'manual' OR (plaid_security_id IS NULL AND ticker_key IS NOT NULL)),
    ADD CONSTRAINT securities_plaid_identity
        CHECK (source <> 'plaid' OR plaid_security_id IS NOT NULL);

CREATE UNIQUE INDEX securities_manual_ticker_key
    ON securities (ticker_key) WHERE source = 'manual';

-- holdings needs no change: its only NOT NULL foreign keys are account_id and
-- security_id (00004:32-48), both satisfied above, and UNIQUE (account_id,
-- security_id) upserts a manual position exactly as it upserts a synced one.

ALTER TABLE investment_transactions
    ALTER COLUMN plaid_investment_transaction_id DROP NOT NULL;
ALTER TABLE investment_transactions
    DROP CONSTRAINT IF EXISTS investment_transactions_plaid_investment_transaction_id_key;
DROP INDEX IF EXISTS investment_transactions_plaid_investment_transaction_id_key;
CREATE UNIQUE INDEX investment_transactions_plaid_investment_transaction_id_key
    ON investment_transactions (plaid_investment_transaction_id)
    WHERE plaid_investment_transaction_id IS NOT NULL;

-- 'scheduled' separates a contribution the worker posted from one the user
-- typed, which matters when a user asks why a row exists. It does NOT change
-- how the row is counted: reporting.IsExternalFlow classifies on type/subtype
-- and has never looked at source, so a manual or scheduled row feeds TWR and
-- MWR through the identical path as a Plaid one.
ALTER TABLE investment_transactions
    ADD COLUMN source TEXT NOT NULL DEFAULT 'plaid'
        CHECK (source IN ('plaid', 'manual', 'scheduled'));

ALTER TABLE investment_transactions
    ADD CONSTRAINT investment_transactions_plaid_identity
        CHECK (source <> 'plaid' OR plaid_investment_transaction_id IS NOT NULL);

-- --------------------------------------------------------------------------
-- 4. transactions — a fourth provenance
-- --------------------------------------------------------------------------

-- 'scheduled' is a row the auto-post worker materialised from an obligation.
-- Kept distinct from 'manual' so the UI can mark it read-only: editing the
-- amount here would be undone by the next posting, and the honest affordance is
-- to edit the obligation.
ALTER TABLE transactions DROP CONSTRAINT transactions_source_check;
ALTER TABLE transactions ADD CONSTRAINT transactions_source_check
    CHECK (source IN ('plaid', 'csv', 'manual', 'scheduled'));

-- --------------------------------------------------------------------------
-- 5. recurring_obligations — from forecast to posting
-- --------------------------------------------------------------------------

-- Doc 13 built the bill calendar and deliberately never posted anything: an
-- obligation was a statement about the future, and materialising it would have
-- competed with the Plaid sync for the same reality. That reasoning holds for a
-- Plaid-linked account and fails for a manual one, where NOTHING will ever post
-- the row. These three columns are the opt-in.
ALTER TABLE recurring_obligations
    -- Off by default, so no existing obligation starts posting on deploy.
    ADD COLUMN auto_post          BOOLEAN NOT NULL DEFAULT FALSE,
    -- The idempotency cursor: the worker posts only occurrences strictly after
    -- this date. Cadence stays derived — anchor_date + n intervals is still the
    -- only source of truth for WHEN — this records only what has been posted.
    -- Same shape as plaid_items.sync_cursor, and the same rule: it advances in
    -- the transaction that wrote the rows, never before.
    ADD COLUMN last_posted_date   DATE,
    -- Which account the posting CREDITS, as distinct from account_id, which is
    -- the account the bill is PAID FROM (00019:42, used by the balance
    -- projection). For a Voya contribution those are different accounts:
    -- money leaves checking and arrives in the retirement plan. NULL means
    -- "same as account_id", which is the right default for an ordinary bill.
    ADD COLUMN posting_account_id UUID REFERENCES accounts (id) ON DELETE SET NULL;

-- An obligation cannot post without knowing where to.
ALTER TABLE recurring_obligations
    ADD CONSTRAINT recurring_obligations_auto_post_target
        CHECK (NOT auto_post OR account_id IS NOT NULL OR posting_account_id IS NOT NULL);

CREATE INDEX recurring_obligations_auto_post_idx
    ON recurring_obligations (last_posted_date) WHERE auto_post AND is_active;

-- Ties a materialised row back to the template that produced it. Nullable
-- because the overwhelming majority of transactions have no obligation, and
-- ON DELETE SET NULL because deleting a bill must not delete the history of
-- having paid it.
ALTER TABLE transactions
    ADD COLUMN obligation_id UUID REFERENCES recurring_obligations (id) ON DELETE SET NULL;

CREATE INDEX transactions_obligation_idx ON transactions (obligation_id)
    WHERE obligation_id IS NOT NULL;

-- The belt to the cursor's braces. The cursor alone is what doc 30 recommends,
-- and it is what Plaid sync relies on — but Plaid sync cannot double-charge a
-- retirement account by replaying, and this can. A unique index makes a
-- duplicate posting impossible rather than merely unlikely, at the cost of one
-- index, and turns a worker bug into a caught error instead of a wrong balance.
CREATE UNIQUE INDEX transactions_obligation_occurrence_key
    ON transactions (obligation_id, date) WHERE obligation_id IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS transactions_obligation_occurrence_key;
DROP INDEX IF EXISTS transactions_obligation_idx;
ALTER TABLE transactions DROP COLUMN IF EXISTS obligation_id;

DROP INDEX IF EXISTS recurring_obligations_auto_post_idx;
ALTER TABLE recurring_obligations
    DROP CONSTRAINT IF EXISTS recurring_obligations_auto_post_target;
ALTER TABLE recurring_obligations DROP COLUMN IF EXISTS posting_account_id;
ALTER TABLE recurring_obligations DROP COLUMN IF EXISTS last_posted_date;
ALTER TABLE recurring_obligations DROP COLUMN IF EXISTS auto_post;

DELETE FROM transactions WHERE source = 'scheduled';
ALTER TABLE transactions DROP CONSTRAINT IF EXISTS transactions_source_check;
ALTER TABLE transactions ADD CONSTRAINT transactions_source_check
    CHECK (source IN ('plaid', 'csv', 'manual'));

DELETE FROM investment_transactions WHERE source <> 'plaid';
ALTER TABLE investment_transactions
    DROP CONSTRAINT IF EXISTS investment_transactions_plaid_identity;
ALTER TABLE investment_transactions DROP COLUMN IF EXISTS source;
DROP INDEX IF EXISTS investment_transactions_plaid_investment_transaction_id_key;
ALTER TABLE investment_transactions
    ALTER COLUMN plaid_investment_transaction_id SET NOT NULL;
ALTER TABLE investment_transactions
    ADD CONSTRAINT investment_transactions_plaid_investment_transaction_id_key
    UNIQUE (plaid_investment_transaction_id);

DELETE FROM securities WHERE source <> 'plaid';
DROP INDEX IF EXISTS securities_manual_ticker_key;
ALTER TABLE securities DROP CONSTRAINT IF EXISTS securities_plaid_identity;
ALTER TABLE securities DROP CONSTRAINT IF EXISTS securities_manual_ticker;
ALTER TABLE securities DROP COLUMN IF EXISTS ticker_key;
ALTER TABLE securities DROP COLUMN IF EXISTS source;
DROP INDEX IF EXISTS securities_plaid_security_id_key;
ALTER TABLE securities ALTER COLUMN plaid_security_id SET NOT NULL;
ALTER TABLE securities ADD CONSTRAINT securities_plaid_security_id_key
    UNIQUE (plaid_security_id);

DROP TABLE IF EXISTS account_balance_history;

DROP VIEW IF EXISTS account_access;

DELETE FROM accounts WHERE source <> 'plaid';
DROP INDEX IF EXISTS accounts_household_id_idx;
DROP INDEX IF EXISTS accounts_manual_owner_idx;
ALTER TABLE accounts DROP CONSTRAINT IF EXISTS accounts_plaid_owner;
ALTER TABLE accounts DROP CONSTRAINT IF EXISTS accounts_manual_owner;
ALTER TABLE accounts DROP COLUMN IF EXISTS household_id;
ALTER TABLE accounts DROP COLUMN IF EXISTS is_shared;
ALTER TABLE accounts DROP COLUMN IF EXISTS user_id;
ALTER TABLE accounts DROP COLUMN IF EXISTS source;
DROP INDEX IF EXISTS accounts_plaid_account_id_key;
ALTER TABLE accounts ALTER COLUMN plaid_account_id SET NOT NULL;
ALTER TABLE accounts ALTER COLUMN plaid_item_id    SET NOT NULL;
ALTER TABLE accounts ADD CONSTRAINT accounts_plaid_account_id_key
    UNIQUE (plaid_account_id);
