-- +goose Up

-- The encrypted document vault.
--
-- Only metadata lives here. The bytes live in the configured storage backend
-- (a mounted volume, or S3-compatible object storage), sealed with the same
-- AES-GCM key that protects Plaid access tokens. That split is deliberate:
-- Postgres stays small and pg_dump-able, and a blob is never readable from a
-- database dump alone.
--
-- The consequence — and it must be said out loud in DEPLOYING.md — is that a
-- restore now needs three things in agreement: the dump, the document volume,
-- and ENCRYPTION_KEY. Losing the key loses the vault, not just Plaid tokens.
CREATE TABLE documents (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id  UUID NOT NULL REFERENCES households (id) ON DELETE CASCADE,
    uploaded_by   UUID REFERENCES users (id) ON DELETE SET NULL,
    -- Private documents must not appear in a household listing. Mirrors the
    -- per-institution sharing model (plaid_items.is_shared). A divorce decree
    -- or a medical receipt is exactly the kind of thing that lands in a vault
    -- and must not be visible to the other member by default.
    is_shared     BOOLEAN NOT NULL DEFAULT TRUE,

    title         TEXT NOT NULL,
    doc_type      TEXT NOT NULL CHECK (doc_type IN (
                      'receipt','tax','warranty','insurance',
                      'contract','statement','other')),
    -- Original filename and MIME are user-supplied; treat both as untrusted.
    -- They are stored as given so a download can offer a recognisable name,
    -- and sanitised at every point of use rather than on the way in.
    filename      TEXT NOT NULL,
    mime_type     TEXT NOT NULL,
    size_bytes    BIGINT NOT NULL CHECK (size_bytes > 0),
    -- Where the ciphertext lives, relative to the configured backend root.
    -- NEVER derived from the user-supplied filename: it is a generated UUID
    -- path, which removes the path-traversal class entirely rather than
    -- filtering for it.
    storage_key   TEXT NOT NULL UNIQUE,
    -- SHA-256 of the PLAINTEXT, for dedupe and integrity verification. GCM
    -- already authenticates the bytes; this catches a storage-layer mixup
    -- where the wrong blob is served perfectly intact.
    content_hash  TEXT NOT NULL,

    document_date DATE,          -- the date on the document, not the upload date
    expires_at    DATE,          -- warranty/policy expiry; drives the nudge
    retain_until  DATE,          -- computed from doc_type retention policy
    notes         TEXT,

    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX documents_household_idx ON documents (household_id);
CREATE INDEX documents_expires_idx   ON documents (expires_at)
    WHERE expires_at IS NOT NULL;

-- A document links to zero or more records. Standalone is allowed and normal:
-- a tax return belongs to a year, not to a transaction.
CREATE TABLE document_links (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    document_id UUID NOT NULL REFERENCES documents (id) ON DELETE CASCADE,
    -- Exactly one target column is non-NULL. Separate typed columns rather
    -- than a (kind, id) pair so the foreign keys are real and a deleted
    -- transaction takes its attachment link with it.
    transaction_id  UUID REFERENCES transactions (id)   ON DELETE CASCADE,
    manual_asset_id UUID REFERENCES manual_assets (id)  ON DELETE CASCADE,
    account_id      UUID REFERENCES accounts (id)       ON DELETE CASCADE,
    goal_id         UUID REFERENCES goals (id)          ON DELETE CASCADE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT document_links_one_target CHECK (
        (transaction_id  IS NOT NULL)::int
      + (manual_asset_id IS NOT NULL)::int
      + (account_id      IS NOT NULL)::int
      + (goal_id         IS NOT NULL)::int = 1
    )
);

CREATE INDEX document_links_document_idx    ON document_links (document_id);
-- The lookups behind the paperclip indicator: "does this row have documents?"
CREATE UNIQUE INDEX document_links_transaction_idx
    ON document_links (transaction_id, document_id) WHERE transaction_id IS NOT NULL;
CREATE UNIQUE INDEX document_links_manual_asset_idx
    ON document_links (manual_asset_id, document_id) WHERE manual_asset_id IS NOT NULL;
CREATE UNIQUE INDEX document_links_account_idx
    ON document_links (account_id, document_id) WHERE account_id IS NOT NULL;
CREATE UNIQUE INDEX document_links_goal_idx
    ON document_links (goal_id, document_id) WHERE goal_id IS NOT NULL;

-- +goose Down
DROP TABLE IF EXISTS document_links;
DROP TABLE IF EXISTS documents;
