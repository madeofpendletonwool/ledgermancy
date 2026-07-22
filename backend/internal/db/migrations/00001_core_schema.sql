-- +goose Up
-- Core schema: households, users, sessions, and the Plaid-backed ledger.
--
-- Money columns are NUMERIC(20,4) everywhere. Never float. The extra two
-- decimal places beyond cents leave room for fractional-share and FX values
-- without ever introducing binary floating-point error.

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- --------------------------------------------------------------------------
-- Households and users
-- --------------------------------------------------------------------------

CREATE TABLE households (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name       TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE users (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id  UUID        NOT NULL REFERENCES households (id) ON DELETE CASCADE,
    email         TEXT        NOT NULL,
    password_hash TEXT        NOT NULL,
    display_name  TEXT        NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Emails are compared case-insensitively; store them lowercased and enforce
-- global uniqueness so a login never has to guess which household to check.
CREATE UNIQUE INDEX users_email_key ON users (lower(email));
CREATE INDEX users_household_id_idx ON users (household_id);

-- Invitations let one spouse bring the other into an existing household
-- without opening public registration.
CREATE TABLE household_invites (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id UUID        NOT NULL REFERENCES households (id) ON DELETE CASCADE,
    email        TEXT        NOT NULL,
    token_hash   TEXT        NOT NULL UNIQUE,
    invited_by   UUID        REFERENCES users (id) ON DELETE SET NULL,
    expires_at   TIMESTAMPTZ NOT NULL,
    accepted_at  TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX household_invites_household_id_idx ON household_invites (household_id);

-- Sessions are server-side. Only a hash of the cookie token is stored, so a
-- database leak does not hand out live sessions.
CREATE TABLE sessions (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash TEXT        NOT NULL UNIQUE,
    user_agent TEXT,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX sessions_user_id_idx    ON sessions (user_id);
CREATE INDEX sessions_expires_at_idx ON sessions (expires_at);

-- --------------------------------------------------------------------------
-- Plaid items and accounts
-- --------------------------------------------------------------------------

CREATE TABLE plaid_items (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id                UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    plaid_item_id          TEXT        NOT NULL UNIQUE,
    access_token_encrypted BYTEA       NOT NULL,
    institution_id         TEXT,
    institution_name       TEXT,
    -- Which Plaid products this item is authorized for. Modules read this to
    -- decide what to sync, so transactions can ship long before investments.
    products               TEXT[]      NOT NULL DEFAULT '{transactions}',
    status                 TEXT        NOT NULL DEFAULT 'active',
    -- Cursor for /transactions/sync. NULL means "never synced".
    sync_cursor            TEXT,
    last_synced_at         TIMESTAMPTZ,
    -- Backfill progress, surfaced in the UI so the user knows when a full
    -- year of history has actually landed.
    backfill_complete      BOOLEAN     NOT NULL DEFAULT FALSE,
    error_code             TEXT,
    -- Whether the other household member can see this item's data.
    is_shared              BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT plaid_items_status_check
        CHECK (status IN ('active', 'login_required', 'revoked', 'error'))
);

CREATE INDEX plaid_items_user_id_idx ON plaid_items (user_id);

CREATE TABLE accounts (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    plaid_item_id     UUID        NOT NULL REFERENCES plaid_items (id) ON DELETE CASCADE,
    plaid_account_id  TEXT        NOT NULL UNIQUE,
    name              TEXT        NOT NULL,
    official_name     TEXT,
    mask              TEXT,
    -- depository | investment | loan | credit | brokerage | other
    type              TEXT        NOT NULL,
    subtype           TEXT,
    current_balance   NUMERIC(20, 4),
    available_balance NUMERIC(20, 4),
    credit_limit      NUMERIC(20, 4),
    currency          TEXT        NOT NULL DEFAULT 'USD',
    -- Excluded accounts stay linked but drop out of all reporting totals.
    is_active         BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX accounts_plaid_item_id_idx ON accounts (plaid_item_id);
CREATE INDEX accounts_type_idx          ON accounts (type);

-- --------------------------------------------------------------------------
-- Categories
-- --------------------------------------------------------------------------

-- household_id NULL means a system default seeded from Plaid's Personal
-- Finance Category taxonomy; non-NULL rows are household customizations.
CREATE TABLE categories (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id UUID        REFERENCES households (id) ON DELETE CASCADE,
    parent_id    UUID        REFERENCES categories (id) ON DELETE SET NULL,
    name         TEXT        NOT NULL,
    slug         TEXT        NOT NULL,
    icon         TEXT,
    color        TEXT,
    -- Fixed costs (rent, insurance) vs. discretionary. Drives the
    -- fixed-vs-discretionary split used in the reports.
    is_fixed     BOOLEAN     NOT NULL DEFAULT FALSE,
    -- Income categories are summed separately from spending.
    is_income    BOOLEAN     NOT NULL DEFAULT FALSE,
    -- Transfers between own accounts must never count as spending or income.
    is_transfer  BOOLEAN     NOT NULL DEFAULT FALSE,
    sort_order   INTEGER     NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One slug per household, and one per system-default set. NULLS NOT DISTINCT
-- makes the NULL household_id rows collide with each other as intended.
CREATE UNIQUE INDEX categories_household_slug_key
    ON categories (household_id, slug) NULLS NOT DISTINCT;

-- --------------------------------------------------------------------------
-- Transactions
-- --------------------------------------------------------------------------

CREATE TABLE transactions (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id             UUID        NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    -- NULL for CSV-imported rows, which have no Plaid identity.
    plaid_transaction_id   TEXT UNIQUE,
    -- Positive = money out, negative = money in. This matches Plaid's sign
    -- convention; keeping it avoids a whole class of conversion bugs.
    amount                 NUMERIC(20, 4) NOT NULL,
    currency               TEXT        NOT NULL DEFAULT 'USD',
    date                   DATE        NOT NULL,
    authorized_date        DATE,
    name                   TEXT        NOT NULL,
    merchant_name          TEXT,
    -- Normalized merchant key used to look up the categorization cache.
    merchant_key           TEXT,
    pending                BOOLEAN     NOT NULL DEFAULT FALSE,
    -- Links a posted transaction back to the pending row it replaced.
    pending_transaction_id TEXT,
    plaid_pfc_primary      TEXT,
    plaid_pfc_detailed     TEXT,
    category_id            UUID        REFERENCES categories (id) ON DELETE SET NULL,
    -- How category_id was decided: manual | rule | cache | plaid | llm
    category_source        TEXT,
    is_recurring           BOOLEAN     NOT NULL DEFAULT FALSE,
    -- Let a user drop one-off noise (e.g. a reimbursed charge) from reports.
    excluded_from_reports  BOOLEAN     NOT NULL DEFAULT FALSE,
    notes                  TEXT,
    -- plaid | csv | manual
    source                 TEXT        NOT NULL DEFAULT 'plaid',
    -- Untouched Plaid payload so anything derived can be recomputed later.
    raw                    JSONB,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT transactions_source_check
        CHECK (source IN ('plaid', 'csv', 'manual')),
    CONSTRAINT transactions_category_source_check
        CHECK (category_source IS NULL
               OR category_source IN ('manual', 'rule', 'cache', 'plaid', 'llm'))
);

-- The workhorse index: nearly every report scans one account over a date range.
CREATE INDEX transactions_account_date_idx ON transactions (account_id, date DESC);
CREATE INDEX transactions_date_idx         ON transactions (date DESC);
CREATE INDEX transactions_category_id_idx  ON transactions (category_id);
CREATE INDEX transactions_merchant_key_idx ON transactions (merchant_key);
CREATE INDEX transactions_pending_idx      ON transactions (pending) WHERE pending;

-- --------------------------------------------------------------------------
-- Categorization rules and cache
-- --------------------------------------------------------------------------

-- Explicit user rules, highest deterministic priority after a manual override.
CREATE TABLE category_rules (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id UUID        NOT NULL REFERENCES households (id) ON DELETE CASCADE,
    -- contains | equals | regex, applied to merchant name or description
    match_type   TEXT        NOT NULL DEFAULT 'contains',
    pattern      TEXT        NOT NULL,
    category_id  UUID        NOT NULL REFERENCES categories (id) ON DELETE CASCADE,
    priority     INTEGER     NOT NULL DEFAULT 0,
    enabled      BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT category_rules_match_type_check
        CHECK (match_type IN ('contains', 'equals', 'regex'))
);

CREATE INDEX category_rules_household_priority_idx
    ON category_rules (household_id, priority DESC);

-- Merchant -> category cache. LLM answers are written here so the same
-- merchant is never sent to a model twice.
CREATE TABLE merchant_category_map (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id UUID        NOT NULL REFERENCES households (id) ON DELETE CASCADE,
    merchant_key TEXT        NOT NULL,
    category_id  UUID        NOT NULL REFERENCES categories (id) ON DELETE CASCADE,
    -- manual | llm | rule
    source       TEXT        NOT NULL DEFAULT 'llm',
    confidence   NUMERIC(5, 4),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (household_id, merchant_key),
    CONSTRAINT merchant_category_map_source_check
        CHECK (source IN ('manual', 'llm', 'rule'))
);

-- --------------------------------------------------------------------------
-- Budgets
-- --------------------------------------------------------------------------

CREATE TABLE budgets (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id UUID        NOT NULL REFERENCES households (id) ON DELETE CASCADE,
    category_id  UUID        NOT NULL REFERENCES categories (id) ON DELETE CASCADE,
    -- 'household' applies to everyone; otherwise the budget is personal.
    owner_scope  TEXT        NOT NULL DEFAULT 'household',
    user_id      UUID        REFERENCES users (id) ON DELETE CASCADE,
    period       TEXT        NOT NULL DEFAULT 'monthly',
    amount       NUMERIC(20, 4) NOT NULL,
    -- Month this budget takes effect; NULL means "always".
    effective_from DATE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT budgets_owner_scope_check
        CHECK (owner_scope IN ('household', 'user')),
    CONSTRAINT budgets_period_check
        CHECK (period IN ('monthly', 'yearly')),
    -- A personal budget must name its owner; a household one must not.
    CONSTRAINT budgets_user_scope_check
        CHECK ((owner_scope = 'user' AND user_id IS NOT NULL)
            OR (owner_scope = 'household' AND user_id IS NULL))
);

CREATE INDEX budgets_household_id_idx ON budgets (household_id);

-- --------------------------------------------------------------------------
-- Alerts
-- --------------------------------------------------------------------------

CREATE TABLE alerts (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id UUID        NOT NULL REFERENCES households (id) ON DELETE CASCADE,
    -- big_spend | budget_threshold | unusual_merchant | low_leftover
    type         TEXT        NOT NULL,
    -- Type-specific thresholds, kept as JSONB so new alert kinds do not
    -- require a migration.
    config       JSONB       NOT NULL DEFAULT '{}',
    enabled      BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX alerts_household_id_idx ON alerts (household_id);

CREATE TABLE alert_events (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    alert_id       UUID        NOT NULL REFERENCES alerts (id) ON DELETE CASCADE,
    transaction_id UUID        REFERENCES transactions (id) ON DELETE CASCADE,
    triggered_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    payload        JSONB       NOT NULL DEFAULT '{}',
    read_at        TIMESTAMPTZ
);

CREATE INDEX alert_events_alert_id_idx     ON alert_events (alert_id, triggered_at DESC);
CREATE INDEX alert_events_unread_idx       ON alert_events (triggered_at DESC) WHERE read_at IS NULL;

-- --------------------------------------------------------------------------
-- updated_at maintenance
-- --------------------------------------------------------------------------

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION set_updated_at() RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose StatementBegin
DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY[
        'households', 'users', 'plaid_items', 'accounts', 'categories',
        'transactions', 'category_rules', 'merchant_category_map',
        'budgets', 'alerts'
    ] LOOP
        EXECUTE format(
            'CREATE TRIGGER %I_set_updated_at BEFORE UPDATE ON %I
             FOR EACH ROW EXECUTE FUNCTION set_updated_at()', t, t);
    END LOOP;
END $$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE IF EXISTS alert_events, alerts, budgets, merchant_category_map,
    category_rules, transactions, categories, accounts, plaid_items,
    sessions, household_invites, users, households CASCADE;
DROP FUNCTION IF EXISTS set_updated_at();
