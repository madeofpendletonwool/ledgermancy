import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '../lib/api'
import { formatMoney } from '../lib/money'

/**
 * Split a transaction across household members, from the transaction row.
 *
 * A split is an ATTRIBUTION OVERLAY: the charge still happened once, and
 * household spending is unchanged by splitting it. The copy says so, because a
 * user who thinks splitting reduces their spend will misread every report.
 */
export function SplitTransaction({
  transactionId,
  amount,
}: {
  transactionId: string
  amount: string
}) {
  const [open, setOpen] = useState(false)
  const qc = useQueryClient()

  // Only fetched once the panel is opened: a transactions page renders
  // hundreds of rows and none of them need this until asked.
  const splits = useQuery({
    queryKey: ['splits', transactionId],
    queryFn: () => api.transactionSplits(transactionId),
    enabled: open,
  })
  const people = useQuery({
    queryKey: ['people'],
    queryFn: api.people,
    enabled: open,
  })

  const [selected, setSelected] = useState<string[]>([])

  const refresh = () => {
    qc.invalidateQueries({ queryKey: ['splits', transactionId] })
    qc.invalidateQueries({ queryKey: ['household-ledger'] })
    qc.invalidateQueries({ queryKey: ['split-transactions'] })
  }

  const save = useMutation({
    // Equal split: the server resolves the remainder so the shares sum to the
    // transaction exactly. The client never divides money.
    mutationFn: () => api.setTransactionSplits(transactionId, { equal: selected }),
    onSuccess: () => {
      refresh()
      setOpen(false)
    },
  })

  const clear = useMutation({
    mutationFn: () => api.clearTransactionSplits(transactionId),
    onSuccess: () => {
      refresh()
      setOpen(false)
    },
  })

  const existing = splits.data?.shares ?? []

  return (
    <div className="relative shrink-0">
      <button
        className="btn-ghost px-2 py-1 text-xs text-mist-300"
        title="Split with the household"
        onClick={() => setOpen((v) => !v)}
      >
        Split
      </button>

      {open && (
        <div className="absolute right-0 z-30 mt-1 w-64 rounded-xl border border-white/10 bg-ink-950/95 p-4 shadow-xl backdrop-blur-xl">
          {!!existing.length && (
            <div className="mb-3">
              <p className="text-xs font-medium text-mist-300">Split between</p>
              <ul className="mt-1.5 space-y-1 text-xs">
                {existing.map((sh) => (
                  <li key={sh.id} className="flex justify-between gap-2">
                    <span className="truncate">{sh.person_name}</span>
                    <span className="tabular-nums text-mist-300">
                      {formatMoney(sh.amount)}
                      {sh.settled_at && ' ✓'}
                    </span>
                  </li>
                ))}
              </ul>
            </div>
          )}

          <p className="text-xs font-medium text-mist-300">
            Split {formatMoney(amount)} evenly
          </p>
          <div className="mt-1.5 space-y-1">
            {people.data?.map((p) => (
              <label key={p.id} className="flex items-center gap-2 text-xs">
                <input
                  type="checkbox"
                  checked={selected.includes(p.id)}
                  onChange={(e) =>
                    setSelected((cur) =>
                      e.target.checked
                        ? [...cur, p.id]
                        : cur.filter((id) => id !== p.id),
                    )
                  }
                />
                <span className="truncate">{p.display_name}</span>
              </label>
            ))}
          </div>

          {save.isError && (
            <p role="alert" className="mt-2 text-xs text-ember-400">
              {save.error.message}
            </p>
          )}

          <div className="mt-3 flex gap-2">
            <button
              className="btn-primary flex-1 px-2 py-1 text-xs"
              disabled={selected.length === 0 || save.isPending}
              onClick={() => save.mutate()}
            >
              {save.isPending ? 'Saving…' : 'Split'}
            </button>
            {!!existing.length && (
              <button
                className="btn-ghost px-2 py-1 text-xs text-ember-400"
                onClick={() => clear.mutate()}
              >
                Clear
              </button>
            )}
          </div>

          {/* The misreading this prevents. */}
          <p className="mt-2 text-[0.65rem] leading-snug text-mist-500">
            Records whose share this was. Household spending doesn't change.
          </p>
        </div>
      )}
    </div>
  )
}
