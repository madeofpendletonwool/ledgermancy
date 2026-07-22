-- +goose Up
-- Investments, liabilities, manual assets, and net-worth snapshots.
--
-- These tables back the whole-picture view: what you own, what you owe, and how
-- the difference has moved over time.

-- --------------------------------------------------------------------------
-- Investments module
-- --------------------------------------------------------------------------

CREATE TABLE securities (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    plaid_security_id  TEXT NOT NULL UNIQUE,
    name               TEXT,
    ticker             TEXT,
    -- cash | etf | mutual fund | equity | fixed income | derivative | other
    type               TEXT,
    cusip              TEXT,
    isin               TEXT,
    close_price        NUMERIC(20, 6),
    close_price_as_of  DATE,
    currency           TEXT NOT NULL DEFAULT 'USD',
    -- Cash positions inside a brokerage are securities to Plaid but should not
    -- be shown as a holding in an allocation breakdown.
    is_cash_equivalent BOOLEAN NOT NULL DEFAULT FALSE,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX securities_ticker_idx ON securities (ticker);

CREATE TABLE holdings (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id        UUID NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    security_id       UUID NOT NULL REFERENCES securities (id) ON DELETE CASCADE,
    -- Quantity is NUMERIC, not an integer: fractional shares are normal, and a
    -- float here would drift the moment it was multiplied by a price.
    quantity          NUMERIC(28, 10) NOT NULL,
    cost_basis        NUMERIC(20, 4),
    institution_price NUMERIC(20, 6),
    institution_value NUMERIC(20, 4),
    currency          TEXT NOT NULL DEFAULT 'USD',
    as_of             DATE,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- One row per security per account; a re-sync updates in place.
    UNIQUE (account_id, security_id)
);

CREATE INDEX holdings_account_id_idx ON holdings (account_id);

CREATE TABLE investment_transactions (
    id                              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id                      UUID NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    security_id                     UUID REFERENCES securities (id) ON DELETE SET NULL,
    plaid_investment_transaction_id TEXT NOT NULL UNIQUE,
    -- buy | sell | cash | fee | transfer | cancel
    type                            TEXT NOT NULL,
    subtype                         TEXT,
    -- Positive = money out of the account (a buy), matching Plaid's convention
    -- for regular transactions so the two never disagree.
    amount                          NUMERIC(20, 4) NOT NULL,
    quantity                        NUMERIC(28, 10),
    price                           NUMERIC(20, 6),
    fees                            NUMERIC(20, 4),
    date                            DATE NOT NULL,
    name                            TEXT,
    currency                        TEXT NOT NULL DEFAULT 'USD',
    raw                             JSONB,
    created_at                      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX investment_transactions_account_date_idx
    ON investment_transactions (account_id, date DESC);

-- --------------------------------------------------------------------------
-- Liabilities module
-- --------------------------------------------------------------------------

CREATE TABLE liabilities (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id            UUID NOT NULL REFERENCES accounts (id) ON DELETE CASCADE UNIQUE,
    -- credit | student | mortgage
    kind                  TEXT NOT NULL,
    -- Stored as a percentage (6.25 means 6.25%), not a fraction.
    apr                   NUMERIC(9, 4),
    apr_type              TEXT,
    balance               NUMERIC(20, 4),
    minimum_payment       NUMERIC(20, 4),
    last_payment_amount   NUMERIC(20, 4),
    last_payment_date     DATE,
    next_payment_due_date DATE,
    origination_date      DATE,
    origination_principal NUMERIC(20, 4),
    interest_rate_percentage NUMERIC(9, 4),
    is_overdue            BOOLEAN,
    raw                   JSONB,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT liabilities_kind_check CHECK (kind IN ('credit', 'student', 'mortgage'))
);

-- --------------------------------------------------------------------------
-- Manual assets
-- --------------------------------------------------------------------------

-- Things Plaid cannot see but that belong in net worth: home equity, vehicles,
-- cash under the mattress.
CREATE TABLE manual_assets (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id UUID NOT NULL REFERENCES households (id) ON DELETE CASCADE,
    created_by   UUID REFERENCES users (id) ON DELETE SET NULL,
    name         TEXT NOT NULL,
    -- home | vehicle | cash | collectible | other | debt
    kind         TEXT NOT NULL DEFAULT 'other',
    value        NUMERIC(20, 4) NOT NULL,
    -- A manually tracked debt (a private loan, say) counts against net worth.
    is_liability BOOLEAN NOT NULL DEFAULT FALSE,
    as_of        DATE NOT NULL DEFAULT CURRENT_DATE,
    notes        TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX manual_assets_household_id_idx ON manual_assets (household_id);

-- --------------------------------------------------------------------------
-- Net worth snapshots
-- --------------------------------------------------------------------------

-- Snapshotted rather than derived on read.
--
-- Balances are a point-in-time value: Plaid reports what an account holds
-- *today* and keeps no history, so yesterday's net worth is unrecoverable once
-- the balance moves. Recording it as it goes is the only way to have a trend
-- at all — and the trend is the whole point.
CREATE TABLE net_worth_snapshots (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id      UUID NOT NULL REFERENCES households (id) ON DELETE CASCADE,
    as_of             DATE NOT NULL,
    assets_total      NUMERIC(20, 4) NOT NULL,
    liabilities_total NUMERIC(20, 4) NOT NULL,
    net_worth         NUMERIC(20, 4) NOT NULL,
    -- Per-bucket detail (cash, investments, manual, credit, loans) so the
    -- composition of a past snapshot survives even as accounts come and go.
    breakdown         JSONB NOT NULL DEFAULT '{}',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- One snapshot per household per day; re-running replaces it.
    UNIQUE (household_id, as_of)
);

CREATE INDEX net_worth_snapshots_household_date_idx
    ON net_worth_snapshots (household_id, as_of DESC);

-- +goose StatementBegin
DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY[
        'securities', 'holdings', 'investment_transactions',
        'liabilities', 'manual_assets'
    ] LOOP
        EXECUTE format(
            'CREATE TRIGGER %I_set_updated_at BEFORE UPDATE ON %I
             FOR EACH ROW EXECUTE FUNCTION set_updated_at()', t, t);
    END LOOP;
END $$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE IF EXISTS net_worth_snapshots, manual_assets, liabilities,
    investment_transactions, holdings, securities CASCADE;
