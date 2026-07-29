-- +goose Up

-- Persist what OCR read off a receipt.
--
-- Extraction results were previously ephemeral: they lived in one HTTP
-- response and were gone when the dialog closed. That made the ordinary case
-- fail. You scan a receipt at the register, the card charge posts three days
-- later, and by then the only way to match them is to re-run OCR — which means
-- decrypting the image and uploading it to a third party a second time, to
-- re-derive a number that was read correctly the first time.
--
-- Storing the fields turns matching into deterministic SQL that can run any
-- time, for free, with no model call and nothing leaving the host. It is also
-- the smaller privacy footprint: one upload per receipt, ever.
--
-- These columns are a cache of a reading, not a source of truth. Nothing
-- computes from them: they populate a form the user confirms and an amount the
-- matcher compares against. No report, balance or total reads them.
ALTER TABLE documents
    -- When OCR last ran. NULL means it never has, which is what the UI branches
    -- on to decide between "Extract fields" and showing what it found.
    ADD COLUMN extracted_at         TIMESTAMPTZ,
    ADD COLUMN extracted_merchant   TEXT,
    -- The printed total, stored positive. Plaid's convention is positive =
    -- money out and a receipt is always money out, so this compares directly
    -- against transactions.amount with no sign juggling at the call site.
    ADD COLUMN extracted_amount     NUMERIC(20, 4),
    ADD COLUMN extracted_date       DATE,
    -- The model's own 0-1 rating, shown beside the fields. Never a threshold:
    -- nothing is auto-applied at any confidence.
    ADD COLUMN extracted_confidence NUMERIC(5, 4),
    ADD COLUMN extracted_notes      TEXT;

-- The re-match sweep's working set: receipts that were read but never attached
-- to anything. Partial, because it is a small minority of a vault and the
-- producer runs hourly per household.
CREATE INDEX documents_awaiting_match_idx
    ON documents (household_id)
    WHERE extracted_amount IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS documents_awaiting_match_idx;

ALTER TABLE documents
    DROP COLUMN IF EXISTS extracted_at,
    DROP COLUMN IF EXISTS extracted_merchant,
    DROP COLUMN IF EXISTS extracted_amount,
    DROP COLUMN IF EXISTS extracted_date,
    DROP COLUMN IF EXISTS extracted_confidence,
    DROP COLUMN IF EXISTS extracted_notes;
