# 18 — Encrypted document vault

*(TODO.md "Next major initiatives" #7.)*

## Context

Self-hosters chose self-hosting to own their data. Today the app owns
*transaction* data and nothing else — receipts, tax documents, warranty PDFs,
insurance policies, closing paperwork all live scattered across a NAS, a cloud
drive, and an email inbox. None of it sits next to the financial record it
belongs to.

Consolidating them is the most on-brand feature in the backlog and one a cloud
competitor structurally cannot match: the app already has an AES-GCM cipher
(`backend/internal/crypto/crypto.go` — `Seal`/`Open`) protecting Plaid tokens, and
a household ownership model to scope access. A vault is those two things applied
to blobs.

It is also a **prerequisite for doc 23** (pre-tax income / paystub importer),
which needs somewhere encrypted to put a paystub PDF before OCR can run over it,
and which is the single largest hole in the app's data model.

## AI vs deterministic split

**Deterministic:** storage, encryption, linking, retention arithmetic, quotas.

**AI:** optional receipt/document OCR, off by default. Extracted fields are
**always** queued for user confirmation before anything is written — the
suggestion-then-confirm pattern from `categorize/llm.go` and doc 17. A model must
never silently create a transaction from a photo.

## Prerequisites

None. Parallel-safe with all of waves 3–4.

**Doc 23 depends on this.** If both are in flight, land the storage layer,
encryption, and upload/download endpoints first, and say so — 23 can build its
paystub-specific schema on top while the UI is still being finished.

**Interacts with doc 16 (continuity).** Documents become a second thing that must
be backed up, and they are *not* in `pg_dump` if stored on a volume. Coordinate:
16's runbook and backup sidecar need a line about the document volume. Flag it
there rather than assuming.

## Data model

**Reserved migration: `00024_documents.sql`.**

```sql
CREATE TABLE documents (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id  UUID NOT NULL REFERENCES households (id) ON DELETE CASCADE,
    uploaded_by   UUID REFERENCES users (id) ON DELETE SET NULL,
    -- Private documents must not appear in a household listing. Mirrors the
    -- per-institution sharing model (plaid_items.is_shared).
    is_shared     BOOLEAN NOT NULL DEFAULT TRUE,

    title         TEXT NOT NULL,
    doc_type      TEXT NOT NULL CHECK (doc_type IN (
                      'receipt','tax','warranty','insurance',
                      'contract','statement','other')),
    -- Original filename and MIME are user-supplied; treat both as untrusted.
    filename      TEXT NOT NULL,
    mime_type     TEXT NOT NULL,
    size_bytes    BIGINT NOT NULL,
    -- Where the ciphertext lives, relative to the configured backend root.
    -- NEVER derived from the user-supplied filename.
    storage_key   TEXT NOT NULL UNIQUE,
    -- SHA-256 of the PLAINTEXT, for dedupe and integrity verification.
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

-- A document links to zero or more records. Standalone is allowed and normal.
CREATE TABLE document_links (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    document_id UUID NOT NULL REFERENCES documents (id) ON DELETE CASCADE,
    -- Exactly one target column is non-NULL.
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
```

## Backend

New package `backend/internal/documents/`.

### Storage backends

An interface with two implementations:

- **Local filesystem** (default) under a mounted volume — matches the Compose
  deploy model.
- **S3-compatible** (optional), for off-host durability.

`storage_key` is a generated UUID path, never the user's filename. Path traversal
via a crafted filename is the obvious attack and this removes the class entirely.

### Encryption

Ciphertext at rest via the existing `crypto.Cipher`. Three things to get right:

- **`Seal`/`Open` are whole-buffer, in-memory operations.** A 50 MB PDF becomes
  ~50 MB of Go heap per concurrent request. Enforce a **hard per-file size cap**
  (10–25 MB is ample for receipts and statements) and reject above it rather than
  discovering the limit as an OOM under load. If large files are wanted later,
  that is a chunked-streaming redesign, not a bigger cap.
- **Verify `content_hash` on read.** Decrypt, hash, compare. GCM already
  authenticates, but the hash catches a storage-layer mixup where the wrong blob
  is served intact.
- **The `ENCRYPTION_KEY` dependency now covers documents too.** Losing it loses
  the vault, not just Plaid tokens. This raises the stakes on doc 16 — cross-
  reference it in `DEPLOYING.md`.

### Endpoints

Under `/api/documents` in `server.go`. Household-scoped ownership on **every**
route including download — a document ID must never be sufficient to fetch a
blob. Respect `is_shared` for other household members.

- Upload (multipart), list/filter, metadata update, link/unlink, delete.
- Download: stream decrypted bytes with `Content-Disposition: attachment` and a
  sanitised filename. Serve a conservative `Content-Type` — echoing a
  user-supplied MIME back invites stored-XSS via an HTML "receipt". Set
  `X-Content-Type-Options: nosniff`.
- Enforce a per-household quota, configured in `config/config.go`.

### Retention and expiry

Retention defaults per `doc_type` — tax: 7 years; warranty: until `expires_at`;
insurance: renewal + N months. Compute `retain_until` on write; it is advisory,
surfaced in the UI. **Never auto-delete.** A finance app that silently deletes a
user's tax document has failed at its one job.

An insight producer (existing `Producer` interface, `insights/insights.go:38`)
fires on approaching `expires_at` — an expiring warranty or a policy renewal is
genuinely useful and rides the existing feed and push path for free.

### Optional OCR

Off by default, behind a config flag. Extract amount / date / merchant from a
receipt, then **queue for confirmation**: pre-fill a manual transaction form or
propose a match against an existing synced transaction. Never auto-apply. The
image is sent to the configured AI provider — say so plainly in the UI and in
`docs/security.md`, since it is user data leaving the host.

## Frontend

A **Documents** route, plus contextual affordances:

- Upload with drag-and-drop, type picker, date, optional link target.
- List with filters (type, date, linked/standalone, expiring soon).
- Preview for images and PDFs; download for everything.
- "Attach document" on a transaction row, a manual asset, and a goal; a paperclip
  indicator where one is attached.
- Storage-used-against-quota indicator.

## Verification

- `go test -p 1 ./...`.
- **Round-trip:** upload → stored ciphertext on disk is not the plaintext
  (assert it directly) → download returns byte-identical plaintext.
- **Ownership:** a document ID from another household returns 404 on download,
  not 403. A non-shared document is invisible to another member.
- A filename of `../../etc/passwd` is stored safely and downloads with a
  sanitised name.
- An HTML file uploaded as a "receipt" downloads as an attachment with a
  conservative content type and `nosniff`, and does not render inline.
- Size cap rejects cleanly above the limit, with no partial row written.
- Quota enforcement at the boundary.
- Corrupted ciphertext fails closed with a clear error, never partial output.
- Frontend `tsc -b`, `vite build`, `oxlint`.

## Out of scope

- Full-text search inside documents. Wants an index and probably OCR over
  everything; separate work.
- Versioning / revision history. One document, one blob.
- Sharing outside the household. There is no external sharing model and this is
  not the doc to invent one.
- Auto-deletion at retention expiry. Advisory only, permanently.
