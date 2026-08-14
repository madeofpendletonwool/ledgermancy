-- +goose Up
-- Transaction links: an explicit, typed connection between two transactions.
--
-- The app already knows a great deal about a single charge — its category, its
-- tags, its merchant, its documents — and nothing at all about how one charge
-- RELATES to another. That gap is why an $80 refund and the $80 charge it
-- cancels are two unrelated rows: the household knows they are one event, and
-- the ledger has nowhere to record that.
--
-- A link is that record, and it is an ANNOTATION. Adding one never rewrites
-- either transaction's amount, date or category. Everything a link changes is
-- a READ — see the netting note on link_types.nets_spend below, and the
-- `net_refunds` argument in queries/reports.sql. That separation is the whole
-- safety story: a mis-linked pair is one DELETE away from being undone, with
-- no data to reconstruct, because none was ever overwritten.

-- --------------------------------------------------------------------------
-- link_types — the vocabulary of relationships
-- --------------------------------------------------------------------------

-- A link type is a NAMED, DIRECTED relationship. Directed is the load-bearing
-- word: "A refunds B" and "B refunds A" are different claims, and only one of
-- them is true. So a type carries two phrasings of the same edge —
--
--   outward   how the SOURCE end reads:  "refunds"
--   inward    how the TARGET end reads:  "is refunded by"
--
-- — and one row in transaction_links serves both directions. Storing the
-- reverse edge as a second row instead would mean two rows that can disagree,
-- and a netting query that counts the same refund twice.
--
-- A symmetric relationship ("relates to") simply has the same string on both
-- sides. That is a property of the vocabulary, not a second table.
--
-- household_id NULL means a SYSTEM type, exactly as it does on categories: the
-- row is reference data, shared by every household, and no household's CRUD
-- can reach it (every write below guards household_id = $1, which never
-- matches NULL). A household's own types sit beside them.
CREATE TABLE link_types (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id UUID REFERENCES households (id) ON DELETE CASCADE,
    -- The stable handle. Only ever read for the seeded rows — a household's
    -- custom type is addressed by id — but every type has one so the column
    -- can carry a real uniqueness rule rather than a nullable half-rule.
    slug         TEXT NOT NULL,
    name         TEXT NOT NULL,
    outward      TEXT NOT NULL,
    inward       TEXT NOT NULL,
    -- Whether a link of this type makes the SOURCE's inflow cancel the TARGET's
    -- spending, when the reader asks for netting. This is a column rather than
    -- a hardcoded `slug = 'refund'` in the reporting SQL so the query says what
    -- it means, and so a future system type (a chargeback, a returned payment)
    -- is a seed row rather than an edit to three reports.
    --
    -- It is NOT settable through the link-type API, and a household-defined
    -- type is always created with it false. Netting changes a reported spending
    -- figure; a free-text label a user invented must not be able to do that by
    -- accident, and there is no wording of that checkbox that would make the
    -- consequence clear at the moment it is ticked.
    nets_spend   BOOLEAN NOT NULL DEFAULT FALSE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- NULLS NOT DISTINCT so the three system slugs are unique among themselves, the
-- same way categories_household_slug_key handles the shared defaults. A
-- household may not shadow a system slug either, which keeps "refund" meaning
-- one thing everywhere.
CREATE UNIQUE INDEX link_types_household_slug_key
    ON link_types (household_id, slug) NULLS NOT DISTINCT;

-- Name uniqueness folds case, for the reason tags_household_name_key does:
-- "Duplicate" and "duplicate" are not two relationships, they are one
-- relationship the picker now lists twice.
CREATE UNIQUE INDEX link_types_household_name_key
    ON link_types (household_id, lower(name)) NULLS NOT DISTINCT;

-- The three the app ships with.
--
-- refund is the only one that nets, and its direction is the one users get
-- wrong if the wording is loose: the SOURCE is the credit, the TARGET is the
-- charge being cancelled. "This $80 credit REFUNDS that $80 charge."
--
-- relates_to is deliberately symmetric and deliberately meaningless to the
-- money: it is the "these two belong together and I will remember why" link,
-- and it must never move a figure.
--
-- paid_for is the asymmetric non-money link: "this reimbursement paid for that
-- dinner", "this transfer paid for that repair". It does not net, because the
-- two rows are usually on different sides of a transfer and netting them would
-- erase spending that genuinely happened.
INSERT INTO link_types (household_id, slug, name, outward, inward, nets_spend) VALUES
    (NULL, 'refund',     'Refund',     'refunds',   'is refunded by',   TRUE),
    (NULL, 'relates-to', 'Relates to', 'relates to', 'relates to',      FALSE),
    (NULL, 'paid-for',   'Paid for',   'paid for',  'was paid for by',  FALSE);

COMMENT ON COLUMN link_types.nets_spend IS
    'Reporting only: a link of this type lets the source inflow cancel the target spend when the reader asks for netting. Never settable by a household.';

-- --------------------------------------------------------------------------
-- transaction_links — the edges
-- --------------------------------------------------------------------------

-- No household_id column, matching document_links. Both ends are transactions,
-- and a transaction's visibility is already resolved through its account
-- (accounts → account_access). A household column here would be a SECOND
-- answer to "who may see this", and the day the two disagreed the link would
-- be the one that leaked. Every read and write in queries/transaction_links.sql
-- therefore scopes BOTH ends, not just the one the caller named.
--
-- Both foreign keys cascade: a link to a transaction that no longer exists is
-- not history, it is a dangling pointer.
CREATE TABLE transaction_links (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_transaction_id UUID NOT NULL REFERENCES transactions (id) ON DELETE CASCADE,
    target_transaction_id UUID NOT NULL REFERENCES transactions (id) ON DELETE CASCADE,
    -- Cascade rather than restrict. A link's entire meaning is its type, so a
    -- deleted type does not leave links behind with nothing to say; it leaves
    -- the two transactions exactly as they were, which is the correct
    -- non-destructive outcome. The system types cannot be deleted at all.
    link_type_id          UUID NOT NULL REFERENCES link_types (id) ON DELETE CASCADE,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- A transaction cannot refund itself, relate to itself, or pay for itself.
    -- Without this a self-link would net a charge against itself and delete it
    -- from every report.
    CONSTRAINT transaction_links_distinct_ends
        CHECK (source_transaction_id <> target_transaction_id)
);

-- ONE link per unordered PAIR, regardless of type or direction.
--
-- Stricter than Firefly III, which keys on (type, source, destination) and so
-- permits "A refunds B" and "B refunds A" to coexist. Here that pair is not
-- merely redundant, it is arithmetic: netting would subtract each row from the
-- other and make both disappear. The same goes for "A refunds B" alongside "A
-- relates to B" — a second edge over the same pair adds no fact and gives the
-- reader two rows in the panel for one relationship.
--
-- LEAST/GREATEST over the two uuids is what makes the rule direction-blind: the
-- index sees the same key whichever way round the caller inserted it, so the
-- duplicate is rejected by Postgres rather than by a Go pre-check that a
-- concurrent request could race past.
CREATE UNIQUE INDEX transaction_links_pair_key ON transaction_links (
    LEAST(source_transaction_id, target_transaction_id),
    GREATEST(source_transaction_id, target_transaction_id)
);

-- One index per direction. The pair index above cannot serve either lookup (its
-- leading expression is a function of both columns), and the panel reads a
-- transaction's links from BOTH ends — "what does this refund" and "what
-- refunds this" are the same question asked from opposite rows.
CREATE INDEX transaction_links_source_idx ON transaction_links (source_transaction_id);
CREATE INDEX transaction_links_target_idx ON transaction_links (target_transaction_id);

-- +goose Down
DROP TABLE IF EXISTS transaction_links;
DROP TABLE IF EXISTS link_types;
