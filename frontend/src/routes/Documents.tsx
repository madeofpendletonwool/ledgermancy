import { useEffect, useMemo, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { createPortal } from 'react-dom'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, ApiError, DOCUMENT_TYPES } from '../lib/api'
import type {
  DocumentFilters,
  DocumentStorage,
  DocumentType,
  ReceiptExtraction,
  VaultDocument,
} from '../lib/api'
import { formatDate, formatMoney } from '../lib/money'
import { SkeletonCard, SkeletonBlock, SkeletonText, Reveal } from '../components/Skeleton'
import { EmptyState } from '../components/EmptyState'

/**
 * Documents — the encrypted vault.
 *
 * Receipts, tax returns, warranties and policies normally live scattered across
 * a NAS, a cloud drive and an inbox, none of it next to the financial record it
 * belongs to. This puts them beside the ledger, encrypted with the same key
 * that protects the bank connections.
 *
 * Two behaviours are deliberate and worth knowing while reading this file.
 * Preview never points an <img> or <iframe> at the download URL — it fetches
 * the bytes and builds an object URL with a type the server vouched for, so a
 * document cannot choose to render as HTML on this origin. And nothing here
 * ever deletes on a schedule: "keep until" is advice the user acts on.
 */

const TYPE_LABELS: Record<DocumentType, string> = {
  receipt: 'Receipt',
  tax: 'Tax',
  warranty: 'Warranty',
  insurance: 'Insurance',
  contract: 'Contract',
  statement: 'Statement',
  other: 'Other',
}

export function Documents() {
  const [filters, setFilters] = useState<DocumentFilters>({})
  const [uploading, setUploading] = useState(false)
  const [selected, setSelected] = useState<VaultDocument | null>(null)

  const storage = useQuery({
    queryKey: ['documents', 'storage'],
    queryFn: api.documentStorage,
  })

  const documents = useQuery({
    queryKey: ['documents', filters],
    queryFn: () => api.documents(filters),
  })

  // A missing vault is a deployment choice, not an error to apologise for.
  const unavailable =
    storage.error instanceof ApiError && storage.error.status === 503

  return (
    <div className="space-y-8">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <h1 className="text-2xl font-semibold">Documents</h1>
          <p className="mt-1 max-w-2xl text-mist-300">
            Receipts, tax returns, warranties and policies, encrypted at rest
            with the same key that protects your bank connections and kept
            beside the transactions they belong to.
          </p>
        </div>
        {!unavailable && (
          <button className="btn-primary" onClick={() => setUploading(true)}>
            Add document
          </button>
        )}
      </div>

      {unavailable ? (
        <section className="glass p-6">
          <h2 className="text-lg font-medium">The vault is not available</h2>
          <p className="mt-2 text-sm text-mist-300">
            Document storage is switched off, or its backend could not be
            opened at startup. Everything else in the app is unaffected.
          </p>
          {/* Deliberately points at the log rather than guessing. The server
              knows exactly why — an unwritable volume, a bucket refusing
              credentials — and sending an operator to re-read their .env when
              the real answer is one line in the log wastes their time. */}
          <p className="mt-3 text-sm text-mist-400">
            The reason is in the API log, on the line{' '}
            <code>document vault disabled</code>:
          </p>
          <pre className="mt-2 overflow-x-auto rounded-lg bg-black/30 p-3 text-xs text-mist-300">
            docker compose logs api | grep &quot;document vault&quot;
          </pre>
        </section>
      ) : (
        <>
          <StoragePanel storage={storage.data} />
          <FilterBar filters={filters} onChange={setFilters} />

          {documents.isPending ? (
            <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-3">
              {Array.from({ length: 3 }, (_, i) => (
                <SkeletonCard key={i} />
              ))}
            </div>
          ) : documents.data && documents.data.length > 0 ? (
            <Reveal>
              <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-3">
                {documents.data.map((doc) => (
                  <DocumentCard
                    key={doc.id}
                    document={doc}
                    onOpen={() => setSelected(doc)}
                  />
                ))}
              </div>
            </Reveal>
          ) : hasFilters(filters) ? (
            <EmptyState title="No documents match these filters" />
          ) : (
            <EmptyState
              title="Nothing filed yet"
              action={
                <button className="btn-primary" onClick={() => setUploading(true)}>
                  Add a document
                </button>
              }
            >
              Receipts, tax returns, warranties and policies belong beside the
              transactions they relate to.
            </EmptyState>
          )}
        </>
      )}

      {uploading && (
        <UploadModal
          maxBytes={storage.data?.max_file_bytes ?? 0}
          onClose={() => setUploading(false)}
        />
      )}
      {selected && (
        <DocumentModal
          documentId={selected.id}
          ocrEnabled={storage.data?.ocr_enabled ?? false}
          onClose={() => setSelected(null)}
        />
      )}
    </div>
  )
}

function hasFilters(filters: DocumentFilters): boolean {
  return Object.values(filters).some((v) => v !== undefined && v !== '')
}

// --- Storage --------------------------------------------------------------

function StoragePanel({ storage }: { storage?: DocumentStorage }) {
  if (!storage) return null

  const unlimited = storage.quota_bytes <= 0
  const pct = unlimited
    ? 0
    : Math.min(100, (storage.bytes_used / storage.quota_bytes) * 100)

  return (
    <section className="glass p-5">
      <div className="flex flex-wrap items-baseline justify-between gap-3">
        <div>
          <span className="text-sm text-mist-300">
            {storage.document_count}{' '}
            {storage.document_count === 1 ? 'document' : 'documents'} ·{' '}
            <span className="tabular">{formatBytes(storage.bytes_used)}</span>
            {!unlimited && (
              <>
                {' of '}
                <span className="tabular">
                  {formatBytes(storage.quota_bytes)}
                </span>
              </>
            )}
          </span>
        </div>
        <span className="text-xs text-mist-500">
          Up to {formatBytes(storage.max_file_bytes)} per file
        </span>
      </div>

      {!unlimited && (
        <div className="mt-3 h-1.5 overflow-hidden rounded-full bg-white/10">
          <div
            className={`h-full rounded-full transition-all ${
              pct > 90 ? 'bg-red-400' : 'bg-arcane-500'
            }`}
            style={{ width: `${pct}%` }}
          />
        </div>
      )}
    </section>
  )
}

/** Binary units, matching how the backend states its limits. */
function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`
  const units = ['KB', 'MB', 'GB', 'TB']
  let value = n / 1024
  let unit = 0
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024
    unit++
  }
  return `${value.toFixed(1)} ${units[unit]}`
}

// --- Filters ---------------------------------------------------------------

function FilterBar({
  filters,
  onChange,
}: {
  filters: DocumentFilters
  onChange: (next: DocumentFilters) => void
}) {
  const set = (patch: Partial<DocumentFilters>) =>
    onChange({ ...filters, ...patch })

  // Computed here rather than sent as a flag so the server keeps taking a plain
  // date and the "soon" horizon stays a UI decision.
  const soon = useMemo(() => {
    const d = new Date()
    d.setDate(d.getDate() + 30)
    return d.toISOString().slice(0, 10)
  }, [])

  const expiringOn = filters.expiring_before === soon

  return (
    <section className="glass flex flex-wrap items-end gap-3 p-4">
      <div className="min-w-[12rem] flex-1">
        <label className="label" htmlFor="doc-search">
          Search
        </label>
        <input
          id="doc-search"
          className="field"
          placeholder="Title or filename"
          value={filters.search ?? ''}
          onChange={(e) => set({ search: e.target.value })}
        />
      </div>

      <div>
        <label className="label" htmlFor="doc-type-filter">
          Type
        </label>
        <select
          id="doc-type-filter"
          className="field"
          value={filters.doc_type ?? ''}
          onChange={(e) =>
            set({ doc_type: (e.target.value || '') as DocumentType | '' })
          }
        >
          <option value="">All types</option>
          {DOCUMENT_TYPES.map((t) => (
            <option key={t} value={t}>
              {TYPE_LABELS[t]}
            </option>
          ))}
        </select>
      </div>

      <div>
        <label className="label" htmlFor="doc-linked">
          Attachment
        </label>
        <select
          id="doc-linked"
          className="field"
          value={filters.linked === undefined ? '' : String(filters.linked)}
          onChange={(e) =>
            set({
              linked: e.target.value === '' ? undefined : e.target.value === 'true',
            })
          }
        >
          <option value="">Any</option>
          <option value="true">Attached to a record</option>
          <option value="false">Standalone</option>
        </select>
      </div>

      <button
        className={`btn-ghost text-sm ${expiringOn ? 'border-arcane-500/60 text-mist-100' : ''}`}
        onClick={() => set({ expiring_before: expiringOn ? undefined : soon })}
      >
        Expiring soon
      </button>

      {hasFilters(filters) && (
        <button className="btn-ghost text-sm" onClick={() => onChange({})}>
          Clear
        </button>
      )}
    </section>
  )
}

// --- Listing ---------------------------------------------------------------

function DocumentCard({
  document: doc,
  onOpen,
}: {
  document: VaultDocument
  onOpen: () => void
}) {
  const expiry = expiryState(doc.expires_at)

  return (
    <button
      onClick={onOpen}
      className="glass flex flex-col gap-3 p-5 text-left transition hover:border-white/20"
    >
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <h3 className="truncate font-medium text-mist-100">{doc.title}</h3>
          <p className="mt-0.5 truncate text-xs text-mist-500">{doc.filename}</p>
        </div>
        <span className="shrink-0 rounded-lg bg-white/5 px-2 py-1 text-xs text-mist-300">
          {TYPE_LABELS[doc.doc_type]}
        </span>
      </div>

      <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-mist-500">
        <span className="tabular">
          {formatDate(doc.document_date ?? doc.created_at)}
        </span>
        <span className="tabular">{formatBytes(doc.size_bytes)}</span>
        {doc.link_count > 0 && (
          <span className="inline-flex items-center gap-1 text-mist-300">
            <PaperclipIcon />
            {doc.link_count}
          </span>
        )}
        {!doc.is_shared && (
          <span className="rounded bg-white/5 px-1.5 py-0.5">Private</span>
        )}
      </div>

      {expiry && (
        <span
          className={`text-xs ${expiry.urgent ? 'text-red-300' : 'text-rune-300'}`}
        >
          {expiry.text}
        </span>
      )}
    </button>
  )
}

/** Renders an expiry as a phrase, or null when there is nothing to say. */
function expiryState(
  expiresAt: string | null,
): { text: string; urgent: boolean } | null {
  if (!expiresAt) return null

  const days = daysUntil(expiresAt)
  if (days < 0) return { text: `Expired ${formatDate(expiresAt)}`, urgent: true }
  if (days === 0) return { text: 'Expires today', urgent: true }
  if (days <= 30)
    return { text: `Expires in ${days} ${days === 1 ? 'day' : 'days'}`, urgent: days <= 7 }
  return { text: `Expires ${formatDate(expiresAt)}`, urgent: false }
}

function daysUntil(iso: string): number {
  const [y, m, d] = iso.slice(0, 10).split('-').map(Number)
  const target = new Date(y, m - 1, d).getTime()
  const today = new Date()
  today.setHours(0, 0, 0, 0)
  return Math.round((target - today.getTime()) / 86_400_000)
}

// --- Upload ----------------------------------------------------------------

function UploadModal({
  maxBytes,
  onClose,
}: {
  maxBytes: number
  onClose: () => void
}) {
  const qc = useQueryClient()
  const [file, setFile] = useState<File | null>(null)
  const [title, setTitle] = useState('')
  const [docType, setDocType] = useState<DocumentType>('receipt')
  const [documentDate, setDocumentDate] = useState('')
  const [expiresAt, setExpiresAt] = useState('')
  const [notes, setNotes] = useState('')
  const [isShared, setIsShared] = useState(true)
  const [dragging, setDragging] = useState(false)
  const inputRef = useRef<HTMLInputElement>(null)

  const upload = useMutation({
    mutationFn: () =>
      api.uploadDocument({
        file: file!,
        title: title.trim() || file!.name,
        doc_type: docType,
        is_shared: isShared,
        document_date: documentDate || null,
        expires_at: expiresAt || null,
        notes: notes.trim() || null,
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['documents'] })
      onClose()
    },
  })

  // Rejected client-side purely so the user is not made to wait for an upload
  // the server will refuse anyway. The limit is enforced there regardless.
  const tooBig = !!file && maxBytes > 0 && file.size > maxBytes

  const choose = (chosen: File | null) => {
    setFile(chosen)
    if (chosen && !title.trim()) setTitle(chosen.name.replace(/\.[^.]+$/, ''))
  }

  return (
    <Modal title="Add document" onClose={onClose}>
      <div
        onDragOver={(e) => {
          e.preventDefault()
          setDragging(true)
        }}
        onDragLeave={() => setDragging(false)}
        onDrop={(e) => {
          e.preventDefault()
          setDragging(false)
          choose(e.dataTransfer.files[0] ?? null)
        }}
        onClick={() => inputRef.current?.click()}
        className={`cursor-pointer rounded-xl border-2 border-dashed p-6 text-center transition ${
          dragging
            ? 'border-arcane-500/60 bg-arcane-500/5'
            : 'border-white/10 hover:border-white/20'
        }`}
      >
        <input
          ref={inputRef}
          type="file"
          className="sr-only"
          onChange={(e) => choose(e.target.files?.[0] ?? null)}
        />
        {file ? (
          <div>
            <p className="font-medium text-mist-100">{file.name}</p>
            <p className="mt-1 text-xs text-mist-500 tabular">
              {formatBytes(file.size)}
            </p>
          </div>
        ) : (
          <div>
            <p className="text-mist-300">Drop a file here, or click to choose</p>
            <p className="mt-1 text-xs text-mist-500">
              Up to {formatBytes(maxBytes)}
            </p>
          </div>
        )}
      </div>

      {tooBig && (
        <p className="text-sm text-red-300">
          That file is {formatBytes(file!.size)} — larger than the{' '}
          {formatBytes(maxBytes)} limit for one document.
        </p>
      )}

      <div>
        <label className="label" htmlFor="upload-title">
          Title
        </label>
        <input
          id="upload-title"
          className="field"
          value={title}
          onChange={(e) => setTitle(e.target.value)}
          placeholder="2025 tax return"
        />
      </div>

      <div className="grid gap-4 sm:grid-cols-2">
        <div>
          <label className="label" htmlFor="upload-type">
            Type
          </label>
          <select
            id="upload-type"
            className="field"
            value={docType}
            onChange={(e) => setDocType(e.target.value as DocumentType)}
          >
            {DOCUMENT_TYPES.map((t) => (
              <option key={t} value={t}>
                {TYPE_LABELS[t]}
              </option>
            ))}
          </select>
        </div>
        <div>
          <label className="label" htmlFor="upload-date">
            Date on the document
          </label>
          <input
            id="upload-date"
            type="date"
            className="field"
            value={documentDate}
            onChange={(e) => setDocumentDate(e.target.value)}
          />
        </div>
      </div>

      <div>
        <label className="label" htmlFor="upload-expires">
          Expires (optional)
        </label>
        <input
          id="upload-expires"
          type="date"
          className="field"
          value={expiresAt}
          onChange={(e) => setExpiresAt(e.target.value)}
        />
        <p className="mt-1.5 text-xs text-mist-500">
          Warranties and policies with an expiry get a reminder in your feed
          before they run out.
        </p>
      </div>

      <div>
        <label className="label" htmlFor="upload-notes">
          Notes (optional)
        </label>
        <textarea
          id="upload-notes"
          className="field"
          rows={2}
          value={notes}
          onChange={(e) => setNotes(e.target.value)}
        />
      </div>

      <label className="flex cursor-pointer items-start gap-3 text-sm text-mist-300">
        <input
          type="checkbox"
          className="mt-0.5"
          checked={!isShared}
          onChange={(e) => setIsShared(!e.target.checked)}
        />
        <span>
          Keep this private
          <span className="mt-0.5 block text-xs text-mist-500">
            Other members of your household will not see it at all.
          </span>
        </span>
      </label>

      {upload.isError && (
        <p className="text-sm text-red-300">{(upload.error as Error).message}</p>
      )}

      <div className="flex justify-end gap-3">
        <button className="btn-ghost" onClick={onClose}>
          Cancel
        </button>
        <button
          className="btn-primary"
          disabled={!file || tooBig || upload.isPending}
          onClick={() => upload.mutate()}
        >
          {upload.isPending ? 'Uploading…' : 'Upload'}
        </button>
      </div>
    </Modal>
  )
}

// --- Detail ----------------------------------------------------------------

function DocumentModal({
  documentId,
  ocrEnabled,
  onClose,
}: {
  documentId: string
  ocrEnabled: boolean
  onClose: () => void
}) {
  const qc = useQueryClient()
  const [editing, setEditing] = useState(false)
  const [confirmDelete, setConfirmDelete] = useState(false)

  const detail = useQuery({
    queryKey: ['documents', 'detail', documentId],
    queryFn: () => api.document(documentId),
  })
  const doc = detail.data

  const remove = useMutation({
    mutationFn: () => api.deleteDocument(documentId),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['documents'] })
      onClose()
    },
  })

  const unlink = useMutation({
    mutationFn: (linkId: string) => api.unlinkDocument(documentId, linkId),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['documents'] })
    },
  })

  if (!doc) {
    return (
      <Modal title="Document" onClose={onClose}>
        <SkeletonText lines={4} />
      </Modal>
    )
  }

  if (editing) {
    return (
      <EditModal
        document={doc}
        onClose={() => setEditing(false)}
        onSaved={() => {
          setEditing(false)
          qc.invalidateQueries({ queryKey: ['documents'] })
        }}
      />
    )
  }

  return (
    <Modal title={doc.title} onClose={onClose}>
      <Preview document={doc} />

      <dl className="grid grid-cols-1 gap-x-4 gap-y-3 text-sm sm:grid-cols-2">
        <Field label="Type">{TYPE_LABELS[doc.doc_type]}</Field>
        <Field label="Size">
          <span className="tabular">{formatBytes(doc.size_bytes)}</span>
        </Field>
        <Field label="Date">
          {doc.document_date ? formatDate(doc.document_date) : '—'}
        </Field>
        <Field label="Expires">
          {doc.expires_at ? formatDate(doc.expires_at) : '—'}
        </Field>
        <Field label="File">
          <span className="break-all">{doc.filename}</span>
        </Field>
        <Field label="Visibility">{doc.is_shared ? 'Household' : 'Private'}</Field>
      </dl>

      {doc.retain_until && (
        <p className="rounded-xl bg-white/5 p-3 text-xs text-mist-400">
          Worth keeping until{' '}
          <span className="text-mist-300">{formatDate(doc.retain_until)}</span>.
          That is a suggestion based on the document type — nothing is ever
          deleted for you.
        </p>
      )}

      {doc.notes && (
        <div>
          <h3 className="label">Notes</h3>
          <p className="whitespace-pre-wrap text-sm text-mist-300">{doc.notes}</p>
        </div>
      )}

      {doc.links.length > 0 && (
        <div>
          <h3 className="label">Attached to</h3>
          <ul className="space-y-2">
            {doc.links.map((link) => (
              <li
                key={link.id}
                className="flex items-center justify-between gap-3 rounded-xl bg-white/5 px-3 py-2 text-sm"
              >
                <span className="min-w-0">
                  <span className="block truncate text-mist-200">
                    {link.label || link.target_kind}
                  </span>
                  {link.date && (
                    <span className="text-xs text-mist-500 tabular">
                      {formatDate(link.date)}
                      {link.amount && ` · ${formatMoney(link.amount)}`}
                    </span>
                  )}
                </span>
                <button
                  className="shrink-0 text-xs text-mist-400 transition hover:text-red-300"
                  onClick={() => unlink.mutate(link.id)}
                >
                  Detach
                </button>
              </li>
            ))}
          </ul>
        </div>
      )}

      {/* Receipts only. A tax return or a policy is never offered for
          extraction, whatever file format it happens to be in — see the
          allowlist in the documents package. The server enforces this too;
          hiding the button is the courtesy, not the control. */}
      {ocrEnabled && doc.doc_type === 'receipt' && doc.preview_type.startsWith('image/') && (
        <ReceiptExtractor document={doc} />
      )}

      <div className="flex flex-wrap justify-end gap-3">
        {confirmDelete ? (
          <>
            <span className="mr-auto self-center text-sm text-mist-300">
              Delete permanently?
            </span>
            <button className="btn-ghost" onClick={() => setConfirmDelete(false)}>
              Keep
            </button>
            <button
              className="btn-ghost border-red-400/40 text-red-300 hover:border-red-400/60"
              disabled={remove.isPending}
              onClick={() => remove.mutate()}
            >
              {remove.isPending ? 'Deleting…' : 'Delete'}
            </button>
          </>
        ) : (
          <>
            <button
              className="btn-ghost mr-auto text-sm"
              onClick={() => setConfirmDelete(true)}
            >
              Delete
            </button>
            <button className="btn-ghost" onClick={() => setEditing(true)}>
              Edit
            </button>
            <a className="btn-primary" href={`/api/documents/${doc.id}/download`}>
              Download
            </a>
          </>
        )}
      </div>
    </Modal>
  )
}

/**
 * Inline preview.
 *
 * The bytes are fetched and wrapped in a Blob whose type is `preview_type` —
 * a value the server produced from a short allowlist. Pointing an <img> or
 * <iframe> straight at the download URL would be simpler and wrong: object
 * URLs inherit this origin, so the type must come from us, never from whoever
 * uploaded the file.
 */
function Preview({ document: doc }: { document: VaultDocument }) {
  const [url, setUrl] = useState<string | null>(null)
  const [failed, setFailed] = useState(false)
  const [expanded, setExpanded] = useState(false)

  useEffect(() => {
    if (!doc.preview_type) return

    let objectUrl: string | null = null
    let cancelled = false

    api
      .documentPreviewURL(doc.id, doc.preview_type)
      .then((created) => {
        objectUrl = created
        if (cancelled) {
          URL.revokeObjectURL(created)
          return
        }
        setUrl(created)
      })
      .catch(() => !cancelled && setFailed(true))

    return () => {
      cancelled = true
      if (objectUrl) URL.revokeObjectURL(objectUrl)
    }
  }, [doc.id, doc.preview_type])

  if (!doc.preview_type) {
    return (
      <div className="rounded-xl bg-white/5 p-6 text-center text-sm text-mist-400">
        No preview for this file type. Download it to open it.
      </div>
    )
  }
  if (failed) {
    return (
      <div className="rounded-xl bg-white/5 p-6 text-center text-sm text-red-300">
        This document could not be opened. It may have been encrypted with a
        different key.
      </div>
    )
  }
  if (!url) return <SkeletonBlock className="h-72 w-full" />

  return (
    <div className="relative">
      {doc.preview_type === 'application/pdf' ? (
        <iframe
          src={url}
          title={doc.title}
          className="h-96 w-full rounded-xl border border-white/10 bg-white"
        />
      ) : (
        <img
          src={url}
          alt={doc.title}
          className="max-h-96 w-full cursor-zoom-in rounded-xl border border-white/10 object-contain"
          onClick={() => setExpanded(true)}
        />
      )}

      {/* Not hover-only: a PDF preview swallows pointer events into the
          iframe, and on a phone there is no hover at all. */}
      <button
        className="absolute right-2 top-2 rounded-lg bg-ink-950/70 p-1.5 text-mist-300 backdrop-blur-sm transition hover:text-mist-100"
        aria-label="Expand preview"
        title="Expand"
        onClick={() => setExpanded(true)}
      >
        <ExpandIcon />
      </button>

      {expanded && (
        <ExpandedPreview
          document={doc}
          url={url}
          onClose={() => setExpanded(false)}
        />
      )}
    </div>
  )
}

/**
 * The preview at full-viewport size, over the document modal.
 *
 * It reuses the object URL Preview already made rather than fetching again —
 * the bytes are decrypted and in memory, so expanding costs nothing and the
 * single revoke on unmount still owns them.
 *
 * Portalled to <body> out of necessity, not taste: `.glass` carries a
 * backdrop-filter, which makes the modal panel the containing block for any
 * fixed-position descendant. Left in place this would size itself to the
 * panel — the very box it is trying to escape.
 */
function ExpandedPreview({
  document: doc,
  url,
  onClose,
}: {
  document: VaultDocument
  url: string
  onClose: () => void
}) {
  // Escape has to close this and leave the modal underneath open. Both
  // handlers sit on window, so capture first and stop the event dead.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return
      e.stopImmediatePropagation()
      onClose()
    }
    window.addEventListener('keydown', onKey, true)
    return () => window.removeEventListener('keydown', onKey, true)
  }, [onClose])

  return createPortal(
    <div
      className="fixed inset-0 z-[60] flex flex-col bg-ink-950/95 p-3 backdrop-blur-sm sm:p-6"
      role="dialog"
      aria-modal="true"
      aria-label={`${doc.title} — expanded`}
      onClick={onClose}
    >
      <div className="mb-3 flex items-center justify-between gap-4">
        <h3 className="truncate text-sm text-mist-300">{doc.title}</h3>
        <button
          className="rounded-lg p-1 text-mist-400 transition hover:text-mist-100"
          aria-label="Close expanded preview"
          onClick={onClose}
        >
          <svg
            className="h-5 w-5"
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            strokeWidth={2}
            strokeLinecap="round"
            aria-hidden="true"
          >
            <path d="M6 6l12 12M18 6L6 18" />
          </svg>
        </button>
      </div>

      <div className="min-h-0 flex-1" onClick={(e) => e.stopPropagation()}>
        {doc.preview_type === 'application/pdf' ? (
          <iframe
            src={url}
            title={doc.title}
            className="h-full w-full rounded-xl border border-white/10 bg-white"
          />
        ) : (
          <img
            src={url}
            alt={doc.title}
            className="h-full w-full rounded-xl object-contain"
          />
        )}
      </div>
    </div>,
    document.body,
  )
}

function ExpandIcon({ className = 'h-4 w-4' }: { className?: string }) {
  return (
    <svg
      className={className}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={2}
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <path d="M15 3h6v6M21 3l-7 7M9 21H3v-6M3 21l7-7" />
    </svg>
  )
}

function EditModal({
  document: doc,
  onClose,
  onSaved,
}: {
  document: VaultDocument
  onClose: () => void
  onSaved: () => void
}) {
  const [title, setTitle] = useState(doc.title)
  const [docType, setDocType] = useState<DocumentType>(doc.doc_type)
  const [documentDate, setDocumentDate] = useState(
    doc.document_date?.slice(0, 10) ?? '',
  )
  const [expiresAt, setExpiresAt] = useState(doc.expires_at?.slice(0, 10) ?? '')
  const [notes, setNotes] = useState(doc.notes ?? '')
  const [isShared, setIsShared] = useState(doc.is_shared)

  const save = useMutation({
    mutationFn: () =>
      api.updateDocument(doc.id, {
        title: title.trim(),
        doc_type: docType,
        is_shared: isShared,
        document_date: documentDate || null,
        expires_at: expiresAt || null,
        notes: notes.trim() || null,
      }),
    onSuccess: onSaved,
  })

  return (
    <Modal title="Edit document" onClose={onClose}>
      <div>
        <label className="label" htmlFor="edit-title">
          Title
        </label>
        <input
          id="edit-title"
          className="field"
          value={title}
          onChange={(e) => setTitle(e.target.value)}
        />
      </div>

      <div className="grid gap-4 sm:grid-cols-2">
        <div>
          <label className="label" htmlFor="edit-type">
            Type
          </label>
          <select
            id="edit-type"
            className="field"
            value={docType}
            onChange={(e) => setDocType(e.target.value as DocumentType)}
          >
            {DOCUMENT_TYPES.map((t) => (
              <option key={t} value={t}>
                {TYPE_LABELS[t]}
              </option>
            ))}
          </select>
        </div>
        <div>
          <label className="label" htmlFor="edit-date">
            Date on the document
          </label>
          <input
            id="edit-date"
            type="date"
            className="field"
            value={documentDate}
            onChange={(e) => setDocumentDate(e.target.value)}
          />
        </div>
      </div>

      <div>
        <label className="label" htmlFor="edit-expires">
          Expires
        </label>
        <input
          id="edit-expires"
          type="date"
          className="field"
          value={expiresAt}
          onChange={(e) => setExpiresAt(e.target.value)}
        />
      </div>

      <div>
        <label className="label" htmlFor="edit-notes">
          Notes
        </label>
        <textarea
          id="edit-notes"
          className="field"
          rows={3}
          value={notes}
          onChange={(e) => setNotes(e.target.value)}
        />
      </div>

      <label className="flex cursor-pointer items-center gap-3 text-sm text-mist-300">
        <input
          type="checkbox"
          checked={!isShared}
          onChange={(e) => setIsShared(!e.target.checked)}
        />
        Keep this private
      </label>

      {save.isError && (
        <p className="text-sm text-red-300">{(save.error as Error).message}</p>
      )}

      <div className="flex justify-end gap-3">
        <button className="btn-ghost" onClick={onClose}>
          Cancel
        </button>
        <button
          className="btn-primary"
          disabled={!title.trim() || save.isPending}
          onClick={() => save.mutate()}
        >
          {save.isPending ? 'Saving…' : 'Save'}
        </button>
      </div>
    </Modal>
  )
}

// --- Receipt extraction ----------------------------------------------------

/**
 * Reads the fields off a receipt and offers the transactions it could belong to.
 *
 * The design turns on one timing problem. You scan a receipt at the register;
 * the card charge posts two or three days later. So the match that matters is
 * almost never the one available at the moment of scanning, and a feature that
 * only looked once would fail in exactly the normal case.
 *
 * The fix is that the reading is *cached on the document*. The image goes to
 * the AI provider once, ever; after that the fields are already here and
 * matching is a free deterministic query that re-runs every time this opens —
 * so the charge simply appears once it posts. "Read again" exists for a bad
 * first read and is the only thing that sends the image anywhere a second time.
 *
 * Nothing writes a transaction, a category or an amount from what the model
 * read. The one action is attaching this document to a charge the *user*
 * recognised.
 */
function ReceiptExtractor({ document: doc }: { document: VaultDocument }) {
  const qc = useQueryClient()
  const [fresh, setFresh] = useState<ReceiptExtraction | null>(null)

  // The cached reading is authoritative for display; a just-completed run
  // supersedes it until the document refetches.
  const reading = fresh ?? doc.extraction
  const hasBeenRead = reading !== null

  // Re-matched on every open, which is the whole point: no AI call, no
  // decryption, just the same amount-and-date comparison against whatever has
  // posted since. Skipped entirely until there is a stored total to match on.
  const matches = useQuery({
    queryKey: ['documents', 'matches', doc.id, reading?.total ?? ''],
    queryFn: () => api.documentMatches(doc.id),
    enabled: hasBeenRead && (reading?.total ?? '') !== '',
  })

  const extract = useMutation({
    mutationFn: () => api.extractReceipt(doc.id),
    onSuccess: (result) => {
      setFresh(result)
      // Refetch the document so the cached reading is on the row, not just in
      // this component's state.
      qc.invalidateQueries({ queryKey: ['documents'] })
    },
  })

  const attach = useMutation({
    mutationFn: (transactionId: string) =>
      api.linkDocument(doc.id, { kind: 'transaction', id: transactionId }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['documents'] })
      qc.invalidateQueries({ queryKey: ['document-counts'] })
    },
  })

  // Already-linked transactions are shown as attached rather than offered
  // again, so a second click cannot look like it failed.
  const linkedTransactions = new Set(
    doc.links.filter((l) => l.target_kind === 'transaction').map((l) => l.target_id),
  )

  const candidates = fresh?.matches ?? matches.data ?? []

  return (
    <div className="rounded-xl border border-white/10 p-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h3 className="text-sm font-medium text-mist-200">Read this receipt</h3>
        <button
          className="btn-ghost text-sm"
          disabled={extract.isPending}
          onClick={() => extract.mutate()}
        >
          {extract.isPending
            ? 'Reading…'
            : hasBeenRead
              ? 'Read again'
              : 'Extract fields'}
        </button>
      </div>

      <p className="mt-1 text-xs text-mist-500">
        {hasBeenRead
          ? 'Already read — the fields below are stored, so this stays on your server. "Read again" is the only thing that re-sends the image.'
          : 'Sends this image to your configured AI provider once. Nothing is written from what it reads.'}
      </p>

      {extract.isError && (
        <p className="mt-3 text-sm text-red-300">
          {(extract.error as Error).message}
        </p>
      )}

      {reading && (
        <div className="mt-4 space-y-3 text-sm">
          <dl className="grid grid-cols-1 gap-x-4 gap-y-2 sm:grid-cols-2">
            <Field label="Merchant">{reading.merchant || '—'}</Field>
            <Field label="Total">
              {reading.total ? formatMoney(reading.total) : '—'}
            </Field>
            <Field label="Date">
              {reading.date ? formatDate(reading.date) : '—'}
            </Field>
            <Field label="Confidence">
              {Math.round(reading.confidence * 100)}%
            </Field>
          </dl>

          {reading.notes && (
            <p className="text-xs text-mist-400">{reading.notes}</p>
          )}

          {candidates.length > 0 ? (
            <div>
              <h4 className="label">Might be this transaction</h4>
              <ul className="space-y-1.5">
                {candidates.map((m) => {
                  const linked = linkedTransactions.has(m.transaction_id)
                  return (
                    <li
                      key={m.transaction_id}
                      className="flex items-center justify-between gap-3 rounded-lg bg-white/5 px-3 py-2"
                    >
                      <span className="min-w-0 flex-1">
                        <span className="block truncate">{m.label}</span>
                        <span className="text-xs text-mist-400 tabular">
                          {formatDate(m.date)} · {formatMoney(m.amount)}
                          {/* Explains a charge whose posted date looks days off
                              from the receipt: it lined up on the swipe. */}
                          {m.authorized_date && m.authorized_date !== m.date && (
                            <> · card used {formatDate(m.authorized_date)}</>
                          )}
                        </span>
                      </span>
                      {linked ? (
                        <span className="shrink-0 text-xs text-mist-500">Attached</span>
                      ) : (
                        <button
                          className="btn-ghost shrink-0 px-2.5 py-1 text-xs"
                          disabled={attach.isPending}
                          onClick={() => attach.mutate(m.transaction_id)}
                        >
                          Attach
                        </button>
                      )}
                    </li>
                  )
                })}
              </ul>
              <p className="mt-2 text-xs text-mist-500">
                Attaching files this receipt against the charge. Nothing else is
                changed — the amounts above are not written anywhere.
              </p>
              {attach.isError && (
                <p className="mt-2 text-xs text-red-300">
                  {(attach.error as Error).message}
                </p>
              )}
            </div>
          ) : (
            reading.total !== '' &&
            !matches.isPending && (
              <p className="text-xs text-mist-500">
                Nothing has posted yet for {formatMoney(reading.total)}. Card
                charges usually take a few days — this receipt will be offered
                again in your insights when a matching charge arrives, and the
                match is rechecked whenever you open it.
              </p>
            )
          )}
        </div>
      )}
    </div>
  )
}

// --- Shared bits -----------------------------------------------------------

function Modal({
  title,
  onClose,
  children,
}: {
  title: string
  onClose: () => void
  children: ReactNode
}) {
  // Escape closes, matching every other dismissible surface in the app.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => e.key === 'Escape' && onClose()
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose])

  return (
    <div
      className="fixed inset-0 z-50 flex items-start justify-center overflow-y-auto bg-ink-950/80 p-4 backdrop-blur-sm sm:p-8"
      onClick={onClose}
    >
      <div
        role="dialog"
        aria-modal="true"
        aria-label={title}
        className="glass my-auto w-full max-w-xl space-y-5 p-6"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-start justify-between gap-4">
          <h2 className="text-lg font-medium">{title}</h2>
          <button
            className="rounded-lg p-1 text-mist-400 transition hover:text-mist-100"
            aria-label="Close"
            onClick={onClose}
          >
            <svg
              className="h-5 w-5"
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              strokeWidth={2}
              strokeLinecap="round"
              aria-hidden="true"
            >
              <path d="M6 6l12 12M18 6L6 18" />
            </svg>
          </button>
        </div>
        {children}
      </div>
    </div>
  )
}

function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div>
      <dt className="text-xs uppercase tracking-wide text-mist-500">{label}</dt>
      <dd className="mt-0.5 text-mist-200">{children}</dd>
    </div>
  )
}

function PaperclipIcon({ className = 'h-3.5 w-3.5' }: { className?: string }) {
  return (
    <svg
      className={className}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={2}
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <path d="M21.44 11.05l-9.19 9.19a6 6 0 0 1-8.49-8.49l9.19-9.19a4 4 0 0 1 5.66 5.66l-9.2 9.19a2 2 0 0 1-2.83-2.83l8.49-8.48" />
    </svg>
  )
}
