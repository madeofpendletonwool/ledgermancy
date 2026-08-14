import { useRef, useState, type ReactNode } from 'react'
import { createPortal } from 'react-dom'
import { Link } from 'react-router-dom'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, type Category } from '../lib/api'
import { AnchoredPopover } from './AnchoredPopover'
import { OFFLINE_WRITE_HINT, useOnline } from '../lib/offline'

/**
 * The bar that appears once rows are ticked on the transactions list.
 *
 * Everything here is an action the row's own ⋯ menu already offers — the point
 * is only that saying it once beats saying it fifty times. Which is also why
 * there is no bulk delete: the actions offered are the ones that are reversible
 * or re-appliable, and the one that is not stays a deliberate, one-row-at-a-time
 * decision.
 *
 * Portalled and fixed for the reason AnchoredPopover exists: rendered in place it
 * would be sized to whichever `.glass` card contained it rather than the
 * viewport.
 */
export function BulkActionBar({
  selectedIDs,
  spendCats,
  onClear,
}: {
  selectedIDs: string[]
  /** The categories a charge can be filed under, as the list already computes. */
  spendCats: Category[]
  onClear: () => void
}) {
  const qc = useQueryClient()
  const online = useOnline()
  // Which action's popover is open, if any — the same one-at-a-time shape the
  // row menu uses.
  const [panel, setPanel] = useState<'tags' | 'category' | 'counts' | null>(null)
  // What the last action did, so the bar can answer "did that work?" without a
  // toast system. Cleared as soon as another action starts.
  const [result, setResult] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)

  const tagsRef = useRef<HTMLDivElement>(null)
  const categoryRef = useRef<HTMLDivElement>(null)
  const countsRef = useRef<HTMLDivElement>(null)

  const count = selectedIDs.length

  // Every bulk write moves the same figures its single-row twin does, so the
  // same surfaces have to refetch. Kept as one list rather than three, because
  // the cost of an extra invalidation is a refetch and the cost of a missing one
  // is a stale report.
  const invalidate = () => {
    for (const key of [
      'transactions',
      'tags',
      'by-tag',
      'tag-detail',
      'by-category',
      'summary',
      'averages',
      'safe-to-spend',
      'trend',
      'recurring',
    ]) {
      qc.invalidateQueries({ queryKey: [key] })
    }
  }

  const run = useMutation({
    mutationFn: (action: { label: string; call: () => Promise<{ changed: number }> }) =>
      action.call().then((r) => ({ ...r, label: action.label })),
    onMutate: () => {
      setResult(null)
      setError(null)
    },
    onSuccess: ({ changed, label }) => {
      invalidate()
      setPanel(null)
      setResult(`${label} ${changed} ${changed === 1 ? 'transaction' : 'transactions'}.`)
    },
    onError: (e: Error) => setError(e.message),
  })

  // The selection deliberately survives an action, so a run of "tag these, now
  // categorise the same ones" is two clicks rather than two selections.
  const busy = run.isPending

  return createPortal(
    <div className="pointer-events-none fixed inset-x-0 bottom-20 z-[45] flex justify-center px-4 lg:bottom-6">
      <div className="glass pointer-events-auto flex max-w-full flex-wrap items-center gap-2 px-3 py-2 text-xs">
        <span className="px-1 font-medium text-mist-100">
          {count} selected
        </span>

        <BulkAction
          anchorRef={tagsRef}
          label="Tags"
          disabled={!online || busy}
          open={panel === 'tags'}
          onToggle={() => setPanel((p) => (p === 'tags' ? null : 'tags'))}
          onClose={() => setPanel(null)}
        >
          <BulkTagPanel
            count={count}
            busy={busy}
            onApply={(tagIDs, action) =>
              run.mutate({
                label: action === 'add' ? 'Tagged' : 'Untagged',
                call: () => api.bulkTransactionTags(selectedIDs, tagIDs, action),
              })
            }
          />
        </BulkAction>

        <BulkAction
          anchorRef={categoryRef}
          label="Category"
          disabled={!online || busy}
          open={panel === 'category'}
          onToggle={() => setPanel((p) => (p === 'category' ? null : 'category'))}
          onClose={() => setPanel(null)}
        >
          <BulkCategoryPanel
            count={count}
            spendCats={spendCats}
            busy={busy}
            onApply={(categoryID) =>
              run.mutate({
                label: 'Recategorised',
                call: () => api.bulkTransactionCategory(selectedIDs, categoryID),
              })
            }
          />
        </BulkAction>

        <BulkAction
          anchorRef={countsRef}
          label="How it counts"
          disabled={!online || busy}
          open={panel === 'counts'}
          onToggle={() => setPanel((p) => (p === 'counts' ? null : 'counts'))}
          onClose={() => setPanel(null)}
        >
          {/* Both flags are offered in both directions rather than as toggles: a
              selection has no single current state to toggle away from. */}
          <BulkPanelShell title="How these count">
            <BulkChoice
              disabled={busy}
              title="Mark as one-time"
              hint="Keep them in their month, but out of future estimates."
              onClick={() =>
                run.mutate({
                  label: 'Marked one-time on',
                  call: () => api.bulkTransactionFlags(selectedIDs, { is_one_time: true }),
                })
              }
            />
            <BulkChoice
              disabled={busy}
              title="Treat as usual spending"
              hint="Let them count towards typical-month averages again."
              onClick={() =>
                run.mutate({
                  label: 'Cleared one-time on',
                  call: () => api.bulkTransactionFlags(selectedIDs, { is_one_time: false }),
                })
              }
            />
            <div className="my-1 border-t border-white/10" role="none" />
            <BulkChoice
              disabled={busy}
              title="Exclude from reports"
              hint="Hide them everywhere, as if they never happened."
              onClick={() =>
                run.mutate({
                  label: 'Excluded',
                  call: () =>
                    api.bulkTransactionFlags(selectedIDs, { excluded_from_reports: true }),
                })
              }
            />
            <BulkChoice
              disabled={busy}
              title="Include in reports"
              hint="Count them everywhere again."
              onClick={() =>
                run.mutate({
                  label: 'Included',
                  call: () =>
                    api.bulkTransactionFlags(selectedIDs, { excluded_from_reports: false }),
                })
              }
            />
          </BulkPanelShell>
        </BulkAction>

        <button
          type="button"
          className="btn-ghost px-2 py-1 text-xs text-mist-400"
          onClick={onClear}
        >
          Clear
        </button>

        {busy && <span className="px-1 text-mist-500">Working…</span>}
        {!busy && result && (
          <span className="px-1 text-verdant-400" role="status">
            {result}
          </span>
        )}
        {error && (
          <span className="px-1 text-ember-400" role="alert">
            {error}
          </span>
        )}
        {!online && (
          <span className="px-1 text-mist-500">{OFFLINE_WRITE_HINT}</span>
        )}
      </div>
    </div>,
    document.body,
  )
}

/** One action button in the bar plus the popover it opens. */
function BulkAction({
  anchorRef,
  label,
  disabled,
  open,
  onToggle,
  onClose,
  children,
}: {
  anchorRef: React.RefObject<HTMLDivElement | null>
  label: string
  disabled: boolean
  open: boolean
  onToggle: () => void
  onClose: () => void
  children: ReactNode
}) {
  return (
    <div ref={anchorRef} className="relative">
      <button
        type="button"
        className="btn-ghost px-2 py-1 text-xs"
        aria-haspopup="menu"
        aria-expanded={open}
        disabled={disabled}
        onClick={onToggle}
      >
        {label}
      </button>
      {open && (
        <AnchoredPopover anchorRef={anchorRef} onClose={onClose}>
          {children}
        </AnchoredPopover>
      )}
    </div>
  )
}

/**
 * The popover surface. `bottom-full` rather than `top-full`, because the bar it
 * hangs off sits at the bottom of the screen.
 */
function BulkPanelShell({
  title,
  children,
}: {
  title: string
  children: ReactNode
}) {
  return (
    <div className="absolute bottom-full left-0 mb-2 w-64 rounded-xl border border-white/10 bg-ink-950/95 p-3 text-left shadow-xl backdrop-blur-xl">
      <p className="mb-2 text-xs font-medium text-mist-100">{title}</p>
      {children}
    </div>
  )
}

function BulkChoice({
  title,
  hint,
  disabled,
  onClick,
}: {
  title: string
  hint: string
  disabled: boolean
  onClick: () => void
}) {
  return (
    <button
      type="button"
      role="menuitem"
      className="w-full rounded px-2 py-1.5 text-left text-xs transition hover:bg-white/5 disabled:opacity-60"
      disabled={disabled}
      onClick={onClick}
    >
      <span className="block font-medium text-mist-100">{title}</span>
      <span className="block text-mist-500">{hint}</span>
    </button>
  )
}

/**
 * Tick tags, then say whether they go on or come off.
 *
 * Two buttons rather than a checkbox list that is saved as a set, because a
 * selection has no shared "current" set to present — and add/remove is what the
 * endpoint offers for the same reason. Removing is the undo for a mis-aimed add.
 */
function BulkTagPanel({
  count,
  busy,
  onApply,
}: {
  count: number
  busy: boolean
  onApply: (tagIDs: string[], action: 'add' | 'remove') => void
}) {
  const tags = useQuery({ queryKey: ['tags'], queryFn: api.tags })
  const [selected, setSelected] = useState<string[]>([])

  const toggle = (id: string) =>
    setSelected((prev) =>
      prev.includes(id) ? prev.filter((x) => x !== id) : [...prev, id],
    )

  const rows = tags.data ?? []
  const nothingPicked = selected.length === 0 || busy

  return (
    <BulkPanelShell title={`Tag ${count} transactions`}>
      {tags.isPending ? (
        <p className="text-xs text-mist-500">Loading…</p>
      ) : rows.length === 0 ? (
        <p className="text-xs text-mist-500">
          No tags yet. Create one on the{' '}
          <Link to="/tags" className="text-rune-300 hover:text-rune-100">
            Tags page
          </Link>
          .
        </p>
      ) : (
        <div className="max-h-48 space-y-1 overflow-auto">
          {rows.map((tag) => (
            <label
              key={tag.id}
              className="flex cursor-pointer items-center gap-2 rounded px-1 py-1 text-xs hover:bg-white/5"
            >
              <input
                type="checkbox"
                checked={selected.includes(tag.id)}
                onChange={() => toggle(tag.id)}
              />
              <span className="truncate">{tag.name}</span>
            </label>
          ))}
        </div>
      )}

      {rows.length > 0 && (
        <>
          <p className="mt-2 text-[11px] text-mist-500">
            Adding never removes labels already on a row.
          </p>
          <div className="mt-2 flex items-center justify-end gap-2">
            <button
              className="btn-ghost px-2 py-1 text-xs text-mist-300"
              disabled={nothingPicked}
              onClick={() => onApply(selected, 'remove')}
            >
              Remove
            </button>
            <button
              className="btn-ghost px-2 py-1 text-xs"
              disabled={nothingPicked}
              onClick={() => onApply(selected, 'add')}
            >
              Add to {count}
            </button>
          </div>
        </>
      )}
    </BulkPanelShell>
  )
}

/**
 * One category for the whole selection.
 *
 * No "apply to all from this merchant" here, unlike the per-row editor: that
 * switch writes a durable rule about ONE merchant, and a selection spans many.
 */
function BulkCategoryPanel({
  count,
  spendCats,
  busy,
  onApply,
}: {
  count: number
  spendCats: Category[]
  busy: boolean
  onApply: (categoryID: string) => void
}) {
  const [categoryID, setCategoryID] = useState('')

  return (
    <BulkPanelShell title={`Categorise ${count} transactions`}>
      <select
        className="field w-full py-1 text-xs"
        value={categoryID}
        aria-label="Category"
        onChange={(e) => setCategoryID(e.target.value)}
      >
        <option value="">Choose a category…</option>
        {spendCats.map((c) => (
          <option key={c.id} value={c.id}>
            {c.name}
          </option>
        ))}
      </select>

      <div className="mt-2 flex items-center justify-end">
        <button
          className="btn-ghost px-2 py-1 text-xs"
          disabled={!categoryID || busy}
          onClick={() => onApply(categoryID)}
        >
          Apply to {count}
        </button>
      </div>
    </BulkPanelShell>
  )
}
