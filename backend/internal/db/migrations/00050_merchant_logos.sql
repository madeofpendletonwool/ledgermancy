-- +goose Up
--
-- The cache behind the opt-in merchant logo fetcher (MAD-38).
--
-- Two things are cached here, and the second is the one that matters:
--
--   1. The domain a merchant name resolved to, decided once by the AI provider.
--   2. The logo bytes Logo.dev returned for that domain.
--
-- A row exists for every merchant the fetcher has ever CONSIDERED, including
-- the ones it gave up on — that is what `state = 'none'` is for. Without a
-- negative row, every pass would re-ask the model about the same cash-only
-- corner shop and re-request the same 404 forever. With one, a merchant costs
-- at most one model call and one outbound request, ever, which is the same
-- economy merchant_category_map buys for categorisation.
--
-- Bytes live in the database rather than the document vault on purpose. They
-- are small, they are not the user's data (they are public brand imagery), and
-- keeping them here means they ride along in pg_dump instead of becoming a
-- second thing to back up. The size ceiling is enforced in the fetcher, not
-- here: a CHECK on octet_length would fail an INSERT where the app can more
-- usefully record 'none' and move on.
CREATE TABLE merchant_logos (
    household_id UUID NOT NULL REFERENCES households (id) ON DELETE CASCADE,
    -- The RESOLVED merchant key, per the resolution rule in queries/merchants.sql:
    -- the entity UUID as text when the household has merged the merchant, the raw
    -- descriptor key otherwise. Resolution is idempotent, so a merge simply makes
    -- the old row unreachable and the new key gets its own.
    merchant_key TEXT NOT NULL,
    -- The display name the resolution was made from, kept for diagnostics: a
    -- wrong logo is only explicable next to the name that produced it.
    merchant_name TEXT NOT NULL,
    -- The domain the model returned. NULL when it declined to guess, which is
    -- the answer we asked for rather than a failure.
    domain TEXT,
    -- Sniffed from the bytes we stored, never echoed from the response header.
    content_type TEXT,
    image BYTEA,
    -- 'found' — image and content_type are present.
    -- 'none'  — no logo, for any reason: no domain, a 404, a refused type, or
    --           bytes over the ceiling. The UI shows its monogram and the
    --           fetcher never asks again.
    state TEXT NOT NULL CHECK (state IN ('found', 'none')),
    checked_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (household_id, merchant_key),
    -- The states are meaningfully different rows, not a flag on one shape.
    CONSTRAINT merchant_logos_found_has_image
        CHECK (state <> 'found' OR (image IS NOT NULL AND content_type IS NOT NULL))
);

-- +goose Down
DROP TABLE IF EXISTS merchant_logos;
