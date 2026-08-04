import { useEffect, useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, DOCUMENT_TYPES } from '../lib/api'
import type { DocumentTarget, DocumentType, VaultDocument } from '../lib/api'
import { formatDate } from '../lib/money'
import { SkeletonRows } from '../components/Skeleton'

/**
 * The paperclip: attach a document to a transaction, asset, account or goal,
 * and see what is already attached.
 *
 * It is one component for all four targets because the only difference between
 * them is which id goes on the wire — the vault treats a link as a link, and
 * duplicating this per surface would mean four places to fix a bug in.
 *
 * Deliberately lazy. The popover's queries only run once it is open, so a page
 * of fifty transactions costs nothing until someone actually clicks a clip.
 */
export function AttachDocuments({
  target,
  count = 0,
  label = 'Attach a document',
}: {
  target: DocumentTarget
  /** Pre-fetched attachment count, so a closed clip can show a badge. */
  count?: number
  label?: string
}) {
  const [open, setOpen] = useState(false)
  const wrapRef = useRef<HTMLDivElement>(null)

  // Close on an outside click or Escape, matching DropdownMenu's behaviour.
  useEffect(() => {
    if (!open) return

    const onClick = (e: MouseEvent) => {
      if (!wrapRef.current?.contains(e.target as Node)) setOpen(false)
    }
    const onKey = (e: KeyboardEvent) => e.key === 'Escape' && setOpen(false)

    document.addEventListener('mousedown', onClick)
    window.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('mousedown', onClick)
      window.removeEventListener('keydown', onKey)
    }
  }, [open])

  return (
    <div className="relative shrink-0" ref={wrapRef}>
      <button
        type="button"
        aria-label={count > 0 ? `${count} attached` : label}
        title={count > 0 ? `${count} attached` : label}
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
        className={`inline-flex items-center gap-1 rounded-lg px-1.5 py-1 text-xs transition ${
          count > 0
            ? 'text-mist-200 hover:bg-white/5'
            : 'text-mist-500 hover:bg-white/5 hover:text-mist-300'
        }`}
      >
        <PaperclipIcon />
        {count > 0 && <span className="tabular">{count}</span>}
      </button>

      {open && <AttachPanel target={target} onClose={() => setOpen(false)} />}
    </div>
  )
}

/**
 * The popover on its own, without the paperclip. Exported so a surface that
 * already has a menu (the transactions row) can offer "Documents" as one item
 * among several rather than adding another button to the row. Callers own the
 * positioning context and dismissal.
 */
export function AttachPanel({
  target,
  onClose,
}: {
  target: DocumentTarget
  onClose: () => void
}) {
  const qc = useQueryClient()
  const [uploading, setUploading] = useState(false)

  const attached = useQuery({
    queryKey: ['documents', 'attached', target.kind, target.id],
    queryFn: () => api.attachedDocuments(target),
  })

  // The vault, minus what is already here — the "attach an existing document"
  // picker. Filtered client-side because the list is already loaded and the
  // exclusion is two ids, not a query.
  const all = useQuery({
    queryKey: ['documents', {}],
    queryFn: () => api.documents({}),
  })

  const invalidate = () => {
    qc.invalidateQueries({ queryKey: ['documents'] })
    qc.invalidateQueries({ queryKey: ['document-counts'] })
  }

  const link = useMutation({
    mutationFn: (documentId: string) => api.linkDocument(documentId, target),
    onSuccess: invalidate,
  })

  const unlink = useMutation({
    mutationFn: (doc: VaultDocument) => {
      const existing = doc.links.find(
        (l) => l.target_kind === target.kind && l.target_id === target.id,
      )
      if (!existing) return Promise.resolve()
      return api.unlinkDocument(doc.id, existing.id)
    },
    onSuccess: invalidate,
  })

  const attachedIds = new Set((attached.data ?? []).map((d) => d.id))
  const candidates = (all.data ?? []).filter((d) => !attachedIds.has(d.id))

  return (
    <div className="absolute right-0 top-full z-40 mt-2 w-80 rounded-xl border border-white/10 bg-ink-850 p-4 shadow-xl">
      {uploading ? (
        <QuickUpload
          target={target}
          onDone={() => {
            setUploading(false)
            invalidate()
          }}
          onCancel={() => setUploading(false)}
        />
      ) : (
        <>
          <div className="flex items-center justify-between gap-2">
            <h3 className="text-sm font-medium text-mist-200">Documents</h3>
            <button
              className="text-xs text-arcane-400 transition hover:text-arcane-300"
              onClick={() => setUploading(true)}
            >
              Upload new
            </button>
          </div>

          {attached.isPending ? (
            <SkeletonRows count={2} />
          ) : attached.data && attached.data.length > 0 ? (
            <ul className="mt-3 space-y-1.5">
              {attached.data.map((doc) => (
                <li
                  key={doc.id}
                  className="flex items-center justify-between gap-2 rounded-lg bg-white/5 px-2.5 py-2"
                >
                  <a
                    href={`/api/documents/${doc.id}/download`}
                    className="min-w-0 flex-1 truncate text-xs text-mist-200 hover:text-mist-100"
                  >
                    {doc.title}
                  </a>
                  <button
                    className="shrink-0 text-[11px] text-mist-500 transition hover:text-red-300"
                    onClick={() => unlink.mutate(doc)}
                  >
                    Detach
                  </button>
                </li>
              ))}
            </ul>
          ) : (
            <p className="mt-3 text-xs text-mist-500">Nothing attached yet.</p>
          )}

          {candidates.length > 0 && (
            <div className="mt-4 border-t border-white/10 pt-3">
              <label className="label text-xs" htmlFor="attach-existing">
                Attach an existing document
              </label>
              <select
                id="attach-existing"
                className="field py-2 text-xs"
                value=""
                disabled={link.isPending}
                onChange={(e) => e.target.value && link.mutate(e.target.value)}
              >
                <option value="">Choose…</option>
                {candidates.slice(0, 100).map((doc) => (
                  <option key={doc.id} value={doc.id}>
                    {doc.title}
                    {doc.document_date ? ` · ${formatDate(doc.document_date)}` : ''}
                  </option>
                ))}
              </select>
            </div>
          )}

          {link.isError && (
            <p className="mt-2 text-xs text-red-300">
              {(link.error as Error).message}
            </p>
          )}

          <button
            className="mt-4 w-full rounded-lg py-1.5 text-xs text-mist-500 transition hover:text-mist-300"
            onClick={onClose}
          >
            Close
          </button>
        </>
      )}
    </div>
  )
}

/**
 * Upload straight onto the record — the common case for a receipt, where the
 * document exists only because of the transaction it is being attached to.
 * The link rides along with the upload, so it is one request rather than two.
 */
function QuickUpload({
  target,
  onDone,
  onCancel,
}: {
  target: DocumentTarget
  onDone: () => void
  onCancel: () => void
}) {
  const [file, setFile] = useState<File | null>(null)
  const [docType, setDocType] = useState<DocumentType>('receipt')

  const upload = useMutation({
    mutationFn: () =>
      api.uploadDocument({
        file: file!,
        title: file!.name.replace(/\.[^.]+$/, ''),
        doc_type: docType,
        is_shared: true,
        document_date: null,
        expires_at: null,
        notes: null,
        link: target,
      }),
    onSuccess: onDone,
  })

  return (
    <div className="space-y-3">
      <h3 className="text-sm font-medium text-mist-200">Upload and attach</h3>

      <input
        type="file"
        className="w-full text-xs text-mist-300 file:mr-3 file:rounded-lg file:border-0 file:bg-white/10 file:px-3 file:py-1.5 file:text-xs file:text-mist-200"
        onChange={(e) => setFile(e.target.files?.[0] ?? null)}
      />

      <select
        className="field py-2 text-xs"
        value={docType}
        onChange={(e) => setDocType(e.target.value as DocumentType)}
      >
        {DOCUMENT_TYPES.map((t) => (
          <option key={t} value={t}>
            {t.charAt(0).toUpperCase() + t.slice(1)}
          </option>
        ))}
      </select>

      {upload.isError && (
        <p className="text-xs text-red-300">{(upload.error as Error).message}</p>
      )}

      <div className="flex justify-end gap-2">
        <button className="btn-ghost px-3 py-1.5 text-xs" onClick={onCancel}>
          Cancel
        </button>
        <button
          className="btn-primary px-3 py-1.5 text-xs"
          disabled={!file || upload.isPending}
          onClick={() => upload.mutate()}
        >
          {upload.isPending ? 'Uploading…' : 'Upload'}
        </button>
      </div>
    </div>
  )
}

export function PaperclipIcon() {
  return (
    <svg
      className="h-3.5 w-3.5"
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
