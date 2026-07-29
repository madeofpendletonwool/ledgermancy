-- The document vault: metadata for encrypted blobs, and the links that attach
-- them to ledger records.
--
-- THE VISIBILITY RULE, used by every read below without exception:
--
--     household_id = @household_id AND (uploaded_by = @user_id OR is_shared)
--
-- It is applied on the *download* path too, not just the listing. A document id
-- must never be sufficient to fetch a blob — that is the whole security story
-- of this feature, and the reason GetDocument takes three parameters rather
-- than one. A miss returns no row, which the API turns into 404 rather than
-- 403: a 403 would confirm the id exists in some other household.

-- name: CreateDocument :one
INSERT INTO documents (
    household_id, uploaded_by, is_shared, title, doc_type,
    filename, mime_type, size_bytes, storage_key, content_hash,
    document_date, expires_at, retain_until, notes
) VALUES (
    $1, $2, $3, $4, $5,
    $6, $7, $8, $9, $10,
    $11, $12, $13, $14
)
RETURNING *;

-- name: GetDocument :one
-- The single scoped lookup behind metadata reads, downloads and deletes.
SELECT * FROM documents
WHERE id = @id
  AND household_id = @household_id
  AND (uploaded_by = @user_id OR is_shared);

-- name: ListDocuments :many
-- The vault listing, filtered in SQL so a household with a large vault does not
-- ship every row to the API to be discarded there.
--
-- Each filter is nullable and inert when unset, the pattern the transaction
-- list uses. `linked` is a tri-state: unset = everything, true = attached to
-- something, false = standalone only.
SELECT
    d.*,
    (SELECT COUNT(*) FROM document_links dl WHERE dl.document_id = d.id)::bigint AS link_count
FROM documents d
WHERE d.household_id = @household_id
  AND (d.uploaded_by = @user_id OR d.is_shared)
  AND (sqlc.narg(doc_type)::text IS NULL OR d.doc_type = sqlc.narg(doc_type)::text)
  AND (sqlc.narg(from_date)::date IS NULL OR COALESCE(d.document_date, d.created_at::date) >= sqlc.narg(from_date)::date)
  AND (sqlc.narg(to_date)::date IS NULL OR COALESCE(d.document_date, d.created_at::date) <= sqlc.narg(to_date)::date)
  AND (sqlc.narg(expiring_before)::date IS NULL
       OR (d.expires_at IS NOT NULL AND d.expires_at <= sqlc.narg(expiring_before)::date))
  AND (sqlc.narg(linked)::boolean IS NULL
       OR (EXISTS (SELECT 1 FROM document_links dl WHERE dl.document_id = d.id)) = sqlc.narg(linked)::boolean)
  AND (sqlc.narg(search)::text IS NULL
       OR d.title ILIKE '%' || sqlc.narg(search)::text || '%'
       OR d.filename ILIKE '%' || sqlc.narg(search)::text || '%')
ORDER BY COALESCE(d.document_date, d.created_at::date) DESC, d.created_at DESC
LIMIT @row_limit;

-- name: UpdateDocumentMetadata :one
-- Metadata only. The bytes, storage_key, content_hash and size are written once
-- at upload and never edited — there is no revision history, so a document that
-- needs different content is a new document.
UPDATE documents
SET title         = @title,
    doc_type      = @doc_type,
    is_shared     = @is_shared,
    document_date = @document_date,
    expires_at    = @expires_at,
    retain_until  = @retain_until,
    notes         = @notes,
    updated_at    = now()
WHERE id = @id
  AND household_id = @household_id
  AND (uploaded_by = @user_id OR is_shared)
RETURNING *;

-- name: DeleteDocument :one
-- Returns the storage key so the caller can unlink the blob after the row is
-- gone. Row first, blob second: an orphaned blob is wasted space, whereas a row
-- pointing at a blob that no longer exists is a broken download.
DELETE FROM documents
WHERE id = @id
  AND household_id = @household_id
  AND (uploaded_by = @user_id OR is_shared)
RETURNING storage_key;

-- name: HouseholdStorageUsed :one
-- Bytes stored for the whole household, quota's numerator. Deliberately NOT
-- visibility-filtered: a quota is a property of the deployment's disk, and a
-- member must not be able to exceed it by marking uploads private.
SELECT COALESCE(SUM(size_bytes), 0)::bigint AS bytes_used,
       COUNT(*)::bigint                      AS document_count
FROM documents
WHERE household_id = @household_id;

-- name: FindDocumentByHash :one
-- Dedupe: the same bytes uploaded twice. Advisory only — the API reports the
-- existing document rather than refusing, since a receipt legitimately re-sent
-- for a second transaction is a link, not a duplicate.
SELECT * FROM documents
WHERE household_id = @household_id
  AND content_hash = @content_hash
  AND (uploaded_by = @user_id OR is_shared)
LIMIT 1;

-- --------------------------------------------------------------------------
-- Links
-- --------------------------------------------------------------------------

-- name: CreateDocumentLink :one
-- Exactly one target is non-NULL; the table's CHECK constraint enforces it, so
-- a caller passing two is a 400 from the database rather than a silent
-- half-link. Callers MUST have verified the target belongs to the household
-- first (see the Target*InHousehold lookups below) — this insert cannot.
INSERT INTO document_links (document_id, transaction_id, manual_asset_id, account_id, goal_id)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT DO NOTHING
RETURNING *;

-- name: DeleteDocumentLink :exec
-- Scoped through the document, so a link can only be cut by someone who can see
-- the document it belongs to.
DELETE FROM document_links dl
USING documents d
WHERE dl.id = @id
  AND d.id = dl.document_id
  AND d.household_id = @household_id
  AND (d.uploaded_by = @user_id OR d.is_shared);

-- name: ListDocumentLinks :many
-- Every link for a set of documents, with a human label for the target resolved
-- in SQL. One query per listing rather than one per row.
SELECT
    dl.id,
    dl.document_id,
    dl.transaction_id,
    dl.manual_asset_id,
    dl.account_id,
    dl.goal_id,
    CASE
        WHEN dl.transaction_id  IS NOT NULL THEN 'transaction'
        WHEN dl.manual_asset_id IS NOT NULL THEN 'manual_asset'
        WHEN dl.account_id      IS NOT NULL THEN 'account'
        ELSE 'goal'
    END::text AS target_kind,
    COALESCE(
        t.merchant_name, t.name,
        ma.name,
        a.name,
        g.name,
        ''
    )::text AS target_label,
    t.date AS transaction_date,
    t.amount AS transaction_amount
FROM document_links dl
LEFT JOIN transactions  t  ON t.id  = dl.transaction_id
LEFT JOIN manual_assets ma ON ma.id = dl.manual_asset_id
LEFT JOIN accounts      a  ON a.id  = dl.account_id
LEFT JOIN goals         g  ON g.id  = dl.goal_id
WHERE dl.document_id = ANY (@document_ids::uuid[])
ORDER BY dl.created_at;

-- name: ListDocumentsForTarget :many
-- The attachments on one ledger record — what the paperclip on a transaction
-- row expands into. Visibility-filtered like every other read, so a member's
-- private receipt on a shared transaction stays private.
SELECT d.*
FROM documents d
JOIN document_links dl ON dl.document_id = d.id
WHERE d.household_id = @household_id
  AND (d.uploaded_by = @user_id OR d.is_shared)
  AND (
        (sqlc.narg(transaction_id)::uuid  IS NOT NULL AND dl.transaction_id  = sqlc.narg(transaction_id)::uuid)
     OR (sqlc.narg(manual_asset_id)::uuid IS NOT NULL AND dl.manual_asset_id = sqlc.narg(manual_asset_id)::uuid)
     OR (sqlc.narg(account_id)::uuid      IS NOT NULL AND dl.account_id      = sqlc.narg(account_id)::uuid)
     OR (sqlc.narg(goal_id)::uuid         IS NOT NULL AND dl.goal_id         = sqlc.narg(goal_id)::uuid)
  )
ORDER BY d.created_at DESC;

-- name: CountDocumentsForTransactions :many
-- Paperclip indicators for a page of transactions, in one round trip.
SELECT dl.transaction_id, COUNT(*)::bigint AS document_count
FROM document_links dl
JOIN documents d ON d.id = dl.document_id
WHERE dl.transaction_id = ANY (@transaction_ids::uuid[])
  AND d.household_id = @household_id
  AND (d.uploaded_by = @user_id OR d.is_shared)
GROUP BY dl.transaction_id;

-- --------------------------------------------------------------------------
-- Link-target ownership checks
--
-- A link target is a user-supplied id. Each of these returns a row only when
-- the target is one the caller may see, and the API refuses the link when it
-- does not — otherwise "attach document to transaction X" is a probe for
-- whether X exists in someone else's household.
-- --------------------------------------------------------------------------

-- name: TargetTransactionInHousehold :one
SELECT t.id
FROM transactions t
JOIN accounts a    ON a.id = t.account_id
JOIN plaid_items i ON i.id = a.plaid_item_id
JOIN users u       ON u.id = i.user_id
WHERE t.id = @id
  AND u.household_id = @household_id
  AND (i.user_id = @user_id OR i.is_shared);

-- name: TargetManualAssetInHousehold :one
SELECT id FROM manual_assets WHERE id = @id AND household_id = @household_id;

-- name: TargetAccountInHousehold :one
SELECT a.id
FROM accounts a
JOIN plaid_items i ON i.id = a.plaid_item_id
JOIN users u       ON u.id = i.user_id
WHERE a.id = @id
  AND u.household_id = @household_id
  AND (i.user_id = @user_id OR i.is_shared);

-- name: TargetGoalInHousehold :one
SELECT id FROM goals
WHERE id = @id AND household_id = @household_id
  AND (scope = 'household' OR user_id = @user_id);

-- name: MatchTransactionsForReceipt :many
-- Candidate transactions a receipt might belong to, for the OCR confirmation
-- step. Deliberately a *proposal*: the match is offered for a person to accept,
-- and nothing here writes a link.
--
-- Matching on amount and a date window only. A receipt's merchant string and a
-- bank descriptor rarely agree well enough to filter on, and a name mismatch
-- that hid the right transaction would be worse than a short list to pick from.
--
-- Both date columns are considered. `date` is when the charge posted;
-- `authorized_date` is when the card was actually swiped — which is the date
-- printed on the receipt. A card that posts three days later has a receipt
-- matching its authorized_date exactly and its date not at all, so checking
-- only one of them means leaning on the window to paper over a gap the data
-- already answers precisely.
--
-- Pending rows are excluded, and that is load-bearing rather than tidiness:
-- when a pending charge posts, DeletePendingSupersededBy removes the pending
-- row, and document_links cascades on delete. A receipt attached to a pending
-- transaction would have its link silently vanish a few days later — the worst
-- possible outcome, because nothing tells the user it happened.
SELECT
    t.id,
    t.date,
    t.authorized_date,
    t.amount,
    COALESCE(t.merchant_name, t.name)::text AS label
FROM transactions t
JOIN accounts a    ON a.id = t.account_id
JOIN plaid_items i ON i.id = a.plaid_item_id
JOIN users u       ON u.id = i.user_id
WHERE u.household_id = @household_id
  AND (i.user_id = @user_id OR i.is_shared)
  AND a.is_active
  AND NOT t.pending
  AND (
        t.date BETWEEN @from_date::date AND @to_date::date
     OR (t.authorized_date IS NOT NULL
         AND t.authorized_date BETWEEN @from_date::date AND @to_date::date)
  )
  AND ABS(t.amount - @amount::numeric) <= 0.02
-- Closest by whichever date sits nearer the receipt, so an exact
-- authorized_date hit outranks a posted date several days off.
ORDER BY LEAST(
    ABS(t.date - @on_date::date),
    ABS(COALESCE(t.authorized_date, t.date) - @on_date::date)
), t.date DESC
LIMIT 5;

-- name: SaveDocumentExtraction :one
-- Caches what OCR read, so matching can be re-run later without decrypting the
-- image or sending it anywhere a second time.
--
-- Scoped like every other write here. Nothing computes from these columns: they
-- fill a form the user confirms and give the matcher an amount to compare.
UPDATE documents
SET extracted_at         = now(),
    extracted_merchant   = @extracted_merchant,
    extracted_amount     = @extracted_amount,
    extracted_date       = @extracted_date,
    extracted_confidence = @extracted_confidence,
    extracted_notes      = @extracted_notes,
    updated_at           = now()
WHERE id = @id
  AND household_id = @household_id
  AND (uploaded_by = @user_id OR is_shared)
RETURNING *;

-- name: ListReceiptsAwaitingMatch :many
-- Receipts that were read but never attached to a transaction — the working set
-- for the re-match pass.
--
-- Shared only. The insight feed is a household surface, so a member's private
-- receipt must not raise a nudge in it; the same rule as ListExpiringDocuments
-- and ListActiveHouseholdGoals.
--
-- The NOT EXISTS is what makes this self-limiting: attaching a receipt to any
-- transaction removes it from the set permanently, so a matched receipt is
-- never reconsidered.
SELECT id, title, extracted_merchant, extracted_amount, extracted_date, created_at
FROM documents d
WHERE d.household_id = @household_id
  AND d.is_shared
  AND d.extracted_amount IS NOT NULL
  AND d.extracted_amount > 0
  AND NOT EXISTS (
      SELECT 1 FROM document_links dl
      WHERE dl.document_id = d.id AND dl.transaction_id IS NOT NULL
  )
  -- Old receipts stop being worth nudging about. A charge that never arrived in
  -- this long is not going to.
  AND d.created_at >= @since::timestamptz
ORDER BY d.created_at DESC
LIMIT 50;

-- --------------------------------------------------------------------------
-- Insight feed
-- --------------------------------------------------------------------------

-- name: ListExpiringDocuments :many
-- Documents whose expiry falls inside the nudge window. Shared only: the
-- insight feed is a household surface, so a member's private policy must not
-- surface in it — the same reasoning as ListActiveHouseholdGoals.
SELECT id, title, doc_type, expires_at
FROM documents
WHERE household_id = @household_id
  AND is_shared
  AND expires_at IS NOT NULL
  AND expires_at >= @from_date::date
  AND expires_at <= @to_date::date
ORDER BY expires_at;
