-- +goose Up
-- Tags: free-form labels that cut ACROSS categories.
--
-- A category answers "what kind of spending is this?" and a transaction has
-- exactly one. A tag answers "what was this FOR?" and a transaction can carry
-- several: "Summer Vacation" spans Food, Travel and Lodging; "Reimbursable"
-- spans every category an employer might pay back; "Kitchen Remodel" spans
-- fees, materials and appliances. That is the whole reason this is a second
-- axis rather than more categories — the two questions have different answers
-- for the same row, and forcing them into one bucket loses one of them.
--
-- Tags are first-class objects, not strings on a transaction. A tag carries a
-- description and an optional expected_amount so it can act as an ENVELOPE
-- ("the trip was meant to cost 3,000") that no category or merchant view can
-- express. A bare join table of text labels could not hold either.
--
-- SCOPE: a tag belongs to the HOUSEHOLD, not to the member who created it.
-- Firefly III makes tags per-user, but Ledgermancy is household-first
-- everywhere else (categories, merchants, budgets, goals) and a vacation is a
-- household's vacation. Making them per-user would mean two members tagging the
-- same trip produce two unrelated envelopes that never add up.
--
-- Household scope is NOT a visibility hole, because a tag is a label, not a
-- transaction. Which TAGGED TRANSACTIONS a member can see still runs through
-- the same account_access predicate as every other read
-- (household_id = $1 AND (user_id = $2 OR is_shared)), so a tag on a charge
-- from a private account is invisible to the other member exactly as the charge
-- itself is. Both members see the tag "Summer Vacation"; each one's total for it
-- counts only the rows they were already allowed to see.
CREATE TABLE tags (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id UUID NOT NULL REFERENCES households (id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    -- What the tag is for, in the user's own words. Nullable: most tags are
    -- self-explanatory and forcing a sentence would just produce empty strings.
    description  TEXT,
    -- The envelope's target, when the household set one ("this trip is meant to
    -- cost 3,000"). NUMERIC like every other money column in the schema —
    -- never a float, at any layer. NULL means "no target", which is a different
    -- fact from a target of zero: a progress bar renders for one and not the
    -- other.
    expected_amount NUMERIC(20, 4),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Unique per household, case-INSENSITIVELY. Plain uniqueness on (household_id,
-- name) would let "Vacation" and "vacation" both exist, which is not two tags —
-- it is one tag whose totals are silently split in half, and the user has no way
-- to see why. Case is preserved in the column (the user's capitalisation is
-- theirs); only the uniqueness test folds it.
CREATE UNIQUE INDEX tags_household_name_key ON tags (household_id, lower(name));

-- The many-to-many. The composite primary key IS the "a tag lands on a
-- transaction at most once" rule: re-applying a tag is a no-op rather than a
-- second row that would double every total that tag appears in.
--
-- Both sides cascade. Deleting a transaction drops its labels (a label on a row
-- that no longer exists is not history, it is a dangling pointer), and deleting
-- a tag unlabels its transactions without touching the transactions themselves.
CREATE TABLE transaction_tags (
    transaction_id UUID        NOT NULL REFERENCES transactions (id) ON DELETE CASCADE,
    tag_id         UUID        NOT NULL REFERENCES tags (id) ON DELETE CASCADE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (transaction_id, tag_id)
);

-- The primary key already serves "what tags does this transaction have?"
-- (transaction_id leads). This index serves the other direction — "what
-- transactions carry this tag?" — which is the by-tag report and the tag detail
-- list, i.e. the reads this feature exists for.
CREATE INDEX transaction_tags_tag_idx ON transaction_tags (tag_id);

-- +goose Down
DROP TABLE IF EXISTS transaction_tags;
DROP TABLE IF EXISTS tags;
