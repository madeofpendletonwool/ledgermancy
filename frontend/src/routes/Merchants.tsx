import { useMemo, useState } from 'react'
import type { ReactNode } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '../lib/api'
import type { Merchant, MerchantAlias, MerchantKeyStat } from '../lib/api'
import { formatDate, formatMoney } from '../lib/money'

/**
 * Merchants — canonical merchant management.
 *
 * Banks fragment one business across many descriptors, and every fragment is a
 * separate merchant to the app: subscriptions split across two descriptors are
 * never detected as recurring, and reports double-count a single business.
 *
 * The app proposes merges but never makes one. A proposal changes no number
 * anywhere until it is confirmed here, which is why the review queue shows the
 * evidence rather than just two strings — nobody can judge "same business?" from
 * descriptors alone, but the spend and the date ranges usually settle it.
 */
export function Merchants() {
  const merchants = useQuery({ queryKey: ['merchants'], queryFn: api.merchantGroups })

  const rows = merchants.data ?? []
  const pending = rows.filter((m) => m.suggested.length > 0)
  const confirmed = rows.filter((m) => m.members.length > 0)

  return (
    <div className="space-y-8">
      <div>
        <h1 className="text-2xl font-semibold">Merchants</h1>
        <p className="mt-1 text-mist-300">
          Your bank writes one business several ways — <code>AMAZON.COM</code>,{' '}
          <code>AMZN.COM/BILL</code>, <code>AMZ*ORDER</code>. Grouping them makes
          reports and subscription detection see one merchant instead of three.
        </p>
      </div>

      <ReviewQueue merchants={pending} isPending={merchants.isPending} />
      <ManualMerge />
      <ConfirmedMerchants merchants={confirmed} isPending={merchants.isPending} />
    </div>
  )
}

// --- Review queue ---------------------------------------------------------

function ReviewQueue({
  merchants,
  isPending,
}: {
  merchants: Merchant[]
  isPending: boolean
}) {
  const qc = useQueryClient()
  const [note, setNote] = useState<string | null>(null)

  const scan = useMutation({
    mutationFn: api.scanMerchants,
    onSuccess: () => {
      setNote(
        'Scanning. New suggestions appear here shortly — refresh in a moment.',
      )
    },
  })

  return (
    <section className="glass p-6">
      <div className="mb-1 flex flex-wrap items-center justify-between gap-3">
        <h2 className="text-lg font-medium">Suggested groupings</h2>
        <button
          className="btn-ghost text-sm"
          onClick={() => scan.mutate()}
          disabled={scan.isPending}
        >
          {scan.isPending ? 'Scanning…' : 'Scan for groupings'}
        </button>
      </div>
      <p className="mb-5 text-sm text-mist-300">
        Nothing here affects your reports yet. A suggestion only counts once you
        confirm it.
      </p>

      {note && <p className="mb-4 text-sm text-rune-300">{note}</p>}
      {scan.isError && (
        <p className="mb-4 text-sm text-red-300">
          Could not start a scan. Background jobs may not be running.
        </p>
      )}

      {isPending ? (
        <Loading />
      ) : merchants.length === 0 ? (
        <Empty>
          Nothing to review. Suggestions are refreshed daily, or scan now.
        </Empty>
      ) : (
        <div className="space-y-4">
          {merchants.map((m) => (
            <SuggestionCard
              key={m.id}
              merchant={m}
              onDone={() => {
                setNote(null)
                qc.invalidateQueries({ queryKey: ['merchants'] })
                qc.invalidateQueries({ queryKey: ['merchant-keys'] })
              }}
            />
          ))}
        </div>
      )}
    </section>
  )
}

function SuggestionCard({
  merchant,
  onDone,
}: {
  merchant: Merchant
  onDone: () => void
}) {
  const [name, setName] = useState(merchant.canonical_name)
  const [conflict, setConflict] = useState(false)

  const suggestedKeys = merchant.suggested.map((a) => a.merchant_key)

  const confirm = useMutation({
    mutationFn: async () => {
      // A brand-new proposal has no confirmed members yet, so the merge carries
      // the name; an existing merchant just gains the descriptors.
      const result = await api.mergeMerchants({
        merchant_keys: suggestedKeys,
        entity_id: merchant.id,
      })
      if (name.trim() && name.trim() !== merchant.canonical_name) {
        await api.renameMerchant(merchant.id, name.trim())
      }
      return result
    },
    onSuccess: (result) => {
      setConflict(result.category_conflict)
      if (!result.category_conflict) onDone()
    },
  })

  const reject = useMutation({
    mutationFn: () => api.rejectMerchantSuggestion(merchant.id, suggestedKeys),
    onSuccess: onDone,
  })

  const busy = confirm.isPending || reject.isPending

  return (
    <div className="rounded-xl border border-white/10 bg-ink-900/40 p-4">
      <div className="mb-3 flex flex-wrap items-end gap-3">
        <div>
          <label className="label" htmlFor={`name-${merchant.id}`}>
            Group these as
          </label>
          <input
            id={`name-${merchant.id}`}
            className="field w-56"
            maxLength={80}
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
        </div>
        <div className="ml-auto flex gap-2">
          <button
            className="btn-primary text-sm"
            onClick={() => confirm.mutate()}
            disabled={busy || name.trim() === ''}
          >
            {confirm.isPending ? 'Merging…' : 'Confirm'}
          </button>
          <button
            className="btn-ghost text-sm"
            onClick={() => reject.mutate()}
            disabled={busy}
          >
            Not the same
          </button>
        </div>
      </div>

      <div className="space-y-2">
        {merchant.members.map((a) => (
          <AliasRow key={a.merchant_key} alias={a} />
        ))}
        {merchant.suggested.map((a) => (
          <AliasRow key={a.merchant_key} alias={a} proposed />
        ))}
      </div>

      {conflict && (
        <p className="mt-3 text-sm text-amber-300">
          These descriptors were filed under two different categories that you
          set by hand. Nothing was changed — pick one on the Transactions page,
          then confirm again.
        </p>
      )}
      {(confirm.isError || reject.isError) && (
        <p className="mt-3 text-sm text-red-300">
          That didn’t go through. Try again.
        </p>
      )}
    </div>
  )
}

/**
 * One descriptor with the evidence behind it. `proposed` marks a row that is
 * only a suggestion — visually distinct, because the difference between "this
 * is counted" and "this might be counted" is the whole point of the page.
 */
function AliasRow({
  alias,
  proposed,
  onSplit,
}: {
  alias: MerchantAlias
  proposed?: boolean
  onSplit?: () => void
}) {
  const span =
    alias.first_seen && alias.last_seen
      ? `${formatDate(alias.first_seen)} – ${formatDate(alias.last_seen)}`
      : 'no charges'

  return (
    <div
      className={`flex flex-wrap items-center gap-x-4 gap-y-1 rounded-lg border px-3 py-2 text-sm ${
        proposed
          ? 'border-dashed border-arcane-500/40 bg-arcane-500/5'
          : 'border-white/10'
      }`}
    >
      <div className="min-w-0 flex-1">
        <div className="truncate font-medium text-mist-100">
          {alias.sample_name}
        </div>
        <div className="truncate font-mono text-xs text-mist-500">
          {alias.merchant_key}
        </div>
      </div>
      <div className="text-right text-mist-300">
        <div>{formatMoney(alias.total_amount)}</div>
        <div className="text-xs text-mist-500">
          {alias.transaction_count} charge
          {alias.transaction_count === 1 ? '' : 's'}
        </div>
      </div>
      <div className="w-44 text-right text-xs text-mist-500">{span}</div>
      {proposed && (
        <Pill className="border-arcane-500/40 text-arcane-300">
          {alias.confidence === null
            ? 'suggested'
            : `${Math.round(alias.confidence * 100)}% match`}
        </Pill>
      )}
      {onSplit && (
        <button
          className="text-xs text-mist-400 underline underline-offset-2 hover:text-mist-100"
          onClick={onSplit}
        >
          Separate
        </button>
      )}
    </div>
  )
}

// --- Manual merge ---------------------------------------------------------

function ManualMerge() {
  const qc = useQueryClient()
  const keys = useQuery({ queryKey: ['merchant-keys'], queryFn: api.merchantKeys })
  const [selected, setSelected] = useState<string[]>([])
  const [name, setName] = useState('')
  const [filter, setFilter] = useState('')

  // Only unmapped descriptors are offered. A descriptor already in a merchant is
  // moved by separating it first, which keeps "where did this go?" answerable.
  const available = useMemo(() => {
    const rows = (keys.data ?? []).filter((k) => k.entity_id === null)
    const needle = filter.trim().toLowerCase()
    if (!needle) return rows
    return rows.filter(
      (k) =>
        k.merchant_key.includes(needle) ||
        k.sample_name.toLowerCase().includes(needle),
    )
  }, [keys.data, filter])

  const merge = useMutation({
    mutationFn: () =>
      api.mergeMerchants({
        merchant_keys: selected,
        canonical_name: name.trim() || undefined,
      }),
    onSuccess: () => {
      setSelected([])
      setName('')
      setFilter('')
      qc.invalidateQueries({ queryKey: ['merchants'] })
      qc.invalidateQueries({ queryKey: ['merchant-keys'] })
    },
  })

  const toggle = (key: string) =>
    setSelected((prev) =>
      prev.includes(key) ? prev.filter((k) => k !== key) : [...prev, key],
    )

  return (
    <section className="glass p-6">
      <h2 className="mb-1 text-lg font-medium">Group merchants yourself</h2>
      <p className="mb-5 text-sm text-mist-300">
        Pick two or more descriptors the app hasn’t grouped. Only ungrouped ones
        are listed — to move one, separate it from its current merchant first.
      </p>

      <input
        className="field mb-4 w-full max-w-md"
        placeholder="Filter descriptors…"
        value={filter}
        onChange={(e) => setFilter(e.target.value)}
      />

      {keys.isPending ? (
        <Loading />
      ) : available.length === 0 ? (
        <Empty>
          {filter
            ? 'No descriptors match that filter.'
            : 'Every descriptor is already grouped.'}
        </Empty>
      ) : (
        <div className="max-h-80 space-y-1 overflow-y-auto pr-1">
          {available.map((k) => (
            <KeyOption
              key={k.merchant_key}
              stat={k}
              checked={selected.includes(k.merchant_key)}
              onToggle={() => toggle(k.merchant_key)}
            />
          ))}
        </div>
      )}

      <div className="mt-5 flex flex-wrap items-end gap-3">
        <div>
          <label className="label" htmlFor="merge-name">
            Name for the group
          </label>
          <input
            id="merge-name"
            className="field w-56"
            placeholder="Amazon"
            maxLength={80}
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
        </div>
        <button
          className="btn-primary"
          onClick={() => merge.mutate()}
          disabled={selected.length < 2 || merge.isPending}
        >
          {merge.isPending
            ? 'Grouping…'
            : `Group ${selected.length || ''} descriptor${
                selected.length === 1 ? '' : 's'
              }`.trim()}
        </button>
        {merge.isError && (
          <p className="text-sm text-red-300">That didn’t go through.</p>
        )}
        {merge.data?.category_conflict && (
          <p className="text-sm text-amber-300">
            Grouped, but the descriptors had two different categories you set by
            hand, so neither was changed.
          </p>
        )}
      </div>
    </section>
  )
}

function KeyOption({
  stat,
  checked,
  onToggle,
}: {
  stat: MerchantKeyStat
  checked: boolean
  onToggle: () => void
}) {
  return (
    <label className="flex cursor-pointer items-center gap-3 rounded-lg border border-white/5 px-3 py-2 text-sm hover:border-white/15">
      <input type="checkbox" checked={checked} onChange={onToggle} />
      <span className="min-w-0 flex-1">
        <span className="block truncate text-mist-100">{stat.sample_name}</span>
        <span className="block truncate font-mono text-xs text-mist-500">
          {stat.merchant_key}
        </span>
      </span>
      <span className="text-right text-mist-300">
        {formatMoney(stat.total_amount)}
        <span className="block text-xs text-mist-500">
          {stat.transaction_count} charge
          {stat.transaction_count === 1 ? '' : 's'}
        </span>
      </span>
    </label>
  )
}

// --- Confirmed merchants --------------------------------------------------

function ConfirmedMerchants({
  merchants,
  isPending,
}: {
  merchants: Merchant[]
  isPending: boolean
}) {
  return (
    <section className="glass p-6">
      <h2 className="mb-1 text-lg font-medium">Grouped merchants</h2>
      <p className="mb-5 text-sm text-mist-300">
        These read as one merchant everywhere in the app. Rename one, or separate
        a descriptor that doesn’t belong.
      </p>
      {isPending ? (
        <Loading />
      ) : merchants.length === 0 ? (
        <Empty>Nothing grouped yet.</Empty>
      ) : (
        <div className="space-y-4">
          {merchants.map((m) => (
            <MerchantCard key={m.id} merchant={m} />
          ))}
        </div>
      )}
    </section>
  )
}

function MerchantCard({ merchant }: { merchant: Merchant }) {
  const qc = useQueryClient()
  const [editing, setEditing] = useState(false)
  const [name, setName] = useState(merchant.canonical_name)

  const invalidate = () => {
    qc.invalidateQueries({ queryKey: ['merchants'] })
    qc.invalidateQueries({ queryKey: ['merchant-keys'] })
  }

  const rename = useMutation({
    mutationFn: () => api.renameMerchant(merchant.id, name.trim()),
    onSuccess: () => {
      setEditing(false)
      invalidate()
    },
  })

  const split = useMutation({
    mutationFn: (key: string) => api.splitMerchant(key),
    onSuccess: invalidate,
  })

  return (
    <div className="rounded-xl border border-white/10 bg-ink-900/40 p-4">
      <div className="mb-3 flex flex-wrap items-center gap-3">
        {editing ? (
          <>
            <input
              className="field w-56"
              maxLength={80}
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
            <button
              className="btn-primary text-sm"
              onClick={() => rename.mutate()}
              disabled={rename.isPending || name.trim() === ''}
            >
              Save
            </button>
            <button
              className="btn-ghost text-sm"
              onClick={() => {
                setName(merchant.canonical_name)
                setEditing(false)
              }}
            >
              Cancel
            </button>
          </>
        ) : (
          <>
            <h3 className="text-base font-medium text-mist-100">
              {merchant.canonical_name}
            </h3>
            <span className="text-sm text-mist-400">
              {formatMoney(merchant.total_amount)} over{' '}
              {merchant.transaction_count} charge
              {merchant.transaction_count === 1 ? '' : 's'}
            </span>
            <button
              className="ml-auto text-xs text-mist-400 underline underline-offset-2 hover:text-mist-100"
              onClick={() => setEditing(true)}
            >
              Rename
            </button>
          </>
        )}
      </div>

      <div className="space-y-2">
        {merchant.members.map((a) => (
          <AliasRow
            key={a.merchant_key}
            alias={a}
            onSplit={() => split.mutate(a.merchant_key)}
          />
        ))}
      </div>

      {merchant.suggested.length > 0 && (
        <p className="mt-3 text-xs text-arcane-300">
          {merchant.suggested.length} more descriptor
          {merchant.suggested.length === 1 ? '' : 's'} suggested for this
          merchant — see “Suggested groupings” above.
        </p>
      )}
      {split.isError && (
        <p className="mt-3 text-sm text-red-300">Could not separate that one.</p>
      )}
    </div>
  )
}

// --- Small shared bits ----------------------------------------------------

function Pill({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <span className={`rounded border px-1.5 py-0.5 text-[10px] ${className ?? ''}`}>
      {children}
    </span>
  )
}

function Loading() {
  return <p className="text-sm text-mist-400">Loading…</p>
}

function Empty({ children }: { children: ReactNode }) {
  return <p className="text-sm text-mist-400">{children}</p>
}
