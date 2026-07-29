-- +goose Up

-- Canonical merchants.
--
-- plaid.MerchantKey is a pure string transform with no memory: it collapses
-- store numbers and processor prefixes, but it cannot know that AMAZON.COM,
-- AMZN.COM/BILL and AMZ*ORDER are one business. These tables carry that
-- knowledge, layered on top of the key rather than replacing it.
--
-- Deliberately NOT denormalised onto transactions. An entity_id column there
-- would need backfilling on every merge and would drift out of date; joining
-- through merchant_aliases on the existing transactions.merchant_key index
-- keeps one source of truth.

-- A canonical business. Many raw merchant_keys map to one entity.
CREATE TABLE merchant_entities (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id        UUID        NOT NULL REFERENCES households (id) ON DELETE CASCADE,
    canonical_name      TEXT        NOT NULL,
    default_category_id UUID        REFERENCES categories (id) ON DELETE SET NULL,
    color               TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (household_id, canonical_name)
);

CREATE INDEX merchant_entities_household_idx ON merchant_entities (household_id);

-- The many-to-one mapping. A merchant_key belongs to at most one entity, which
-- is what the unique constraint below enforces.
CREATE TABLE merchant_aliases (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id UUID        NOT NULL REFERENCES households (id) ON DELETE CASCADE,
    entity_id    UUID        NOT NULL REFERENCES merchant_entities (id) ON DELETE CASCADE,
    merchant_key TEXT        NOT NULL,
    -- How this alias got here. 'suggested' rows are NOT active: they are pending
    -- user review and must not affect any report. Every reporting query treats
    -- an alias as live only when source <> 'suggested'. Get that wrong and
    -- unreviewed model guesses silently reshape spending history — the exact
    -- failure this design exists to prevent.
    source       TEXT        NOT NULL CHECK (source IN ('manual', 'fuzzy', 'llm', 'suggested')),
    confidence   NUMERIC(5, 4),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (household_id, merchant_key)
);

CREATE INDEX merchant_aliases_entity_idx ON merchant_aliases (entity_id);

-- Active aliases are what every report joins against, and they are the minority
-- of rows once a suggestion pass has run.
CREATE INDEX merchant_aliases_active_idx
    ON merchant_aliases (household_id, merchant_key)
    WHERE source <> 'suggested';

-- Rejected suggestions must be remembered, or the suggester proposes the same
-- merge on every pass and the review queue never empties. Pairs are stored with
-- key_a < key_b so a pair has exactly one representation.
CREATE TABLE merchant_merge_rejections (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id UUID        NOT NULL REFERENCES households (id) ON DELETE CASCADE,
    key_a        TEXT        NOT NULL,
    key_b        TEXT        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (household_id, key_a, key_b),
    CONSTRAINT merchant_merge_rejections_ordered CHECK (key_a < key_b)
);

-- +goose Down
DROP TABLE IF EXISTS merchant_merge_rejections;
DROP TABLE IF EXISTS merchant_aliases;
DROP TABLE IF EXISTS merchant_entities;
