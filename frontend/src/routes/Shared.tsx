import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, type Person, type SplitTransaction } from '../lib/api'
import { formatMoney } from '../lib/money'
import { SkeletonRows } from '../components/Skeleton'

/**
 * Shared expenses and the household ledger.
 *
 * THE RULE THIS PAGE MUST NOT BLUR: a split is an ATTRIBUTION OVERLAY. The
 * money left the household once. Nothing here is a spending total — it is a
 * record of who owes whom, and settling it moves no money.
 */
export function Shared() {
  const qc = useQueryClient()
  const ledger = useQuery({ queryKey: ['household-ledger'], queryFn: api.householdLedger })
  const split = useQuery({ queryKey: ['split-transactions'], queryFn: api.splitTransactions })
  const people = useQuery({ queryKey: ['people'], queryFn: api.people })

  const [editing, setEditing] = useState<SplitTransaction | null>(null)

  const refresh = () => {
    qc.invalidateQueries({ queryKey: ['household-ledger'] })
    qc.invalidateQueries({ queryKey: ['split-transactions'] })
  }

  return (
    <div className="space-y-8">
      <div>
        <h1 className="text-2xl font-semibold">Shared expenses</h1>
        <p className="mt-1 text-mist-300">
          Who paid for what, and who owes whom. Splitting a charge doesn't change
          what the household spent — it only records whose share it was.
        </p>
      </div>

      <section className="glass p-6">
        <h2 className="text-lg font-medium">Who owes whom</h2>

        {ledger.isPending && <SkeletonRows count={2} />}

        {ledger.data?.length === 0 && (
          <p className="mt-4 text-sm text-mist-500">
            Nothing outstanding. Split a transaction to start a balance.
          </p>
        )}

        <ul className="mt-4 divide-y divide-white/5">
          {ledger.data?.map((row) => (
            <li
              key={`${row.debtor_id}-${row.creditor_id}`}
              className="flex flex-wrap items-center gap-3 py-3"
            >
              <span className="font-medium">{row.debtor_name}</span>
              <span className="text-sm text-mist-500">owes</span>
              <span className="font-medium">{row.creditor_name}</span>
              <span className="ml-auto text-lg tabular">
                {formatMoney(row.amount)}
              </span>
            </li>
          ))}
        </ul>

        {!!ledger.data?.length && (
          <p className="mt-4 text-xs text-mist-500">
            Settling a share records that it was paid back. No money moves here.
          </p>
        )}
      </section>

      <section className="glass p-6">
        <h2 className="text-lg font-medium">Split transactions</h2>

        {split.data?.length === 0 && (
          <p className="mt-4 text-sm text-mist-500">
            No transactions are split yet.
          </p>
        )}

        <ul className="mt-4 divide-y divide-white/5">
          {split.data?.map((t) => (
            <li key={t.transaction_id} className="flex flex-wrap items-center gap-3 py-3">
              <div className="min-w-0 flex-1">
                <p className="truncate font-medium">{t.name}</p>
                <p className="truncate text-sm text-mist-500">
                  {t.date}
                  {t.payer_name && ` · paid by ${t.payer_name}`} · {t.split_count} ways
                </p>
              </div>
              <span className="shrink-0 tabular">{formatMoney(t.amount)}</span>
              <span
                className={`shrink-0 rounded-full px-2 py-0.5 text-xs ${
                  t.fully_settled
                    ? 'bg-rune-400/10 text-rune-300'
                    : 'bg-ember-400/10 text-ember-400'
                }`}
              >
                {t.fully_settled ? 'settled' : `${t.unsettled_count} open`}
              </span>
              <button
                className="shrink-0 text-xs text-mist-300 hover:underline"
                onClick={() => setEditing(t)}
              >
                Manage
              </button>
            </li>
          ))}
        </ul>
      </section>

      {editing && people.data && (
        <SplitDialog
          transaction={editing}
          people={people.data}
          onClose={() => setEditing(null)}
          onChanged={refresh}
        />
      )}
    </div>
  )
}

function SplitDialog({
  transaction,
  people,
  onClose,
  onChanged,
}: {
  transaction: SplitTransaction
  people: Person[]
  onClose: () => void
  onChanged: () => void
}) {
  const qc = useQueryClient()
  const splits = useQuery({
    queryKey: ['splits', transaction.transaction_id],
    queryFn: () => api.transactionSplits(transaction.transaction_id),
  })

  const [selected, setSelected] = useState<string[]>([])

  const refresh = () => {
    qc.invalidateQueries({ queryKey: ['splits', transaction.transaction_id] })
    onChanged()
  }

  // Equal split: the server resolves the remainder so the shares always sum to
  // the transaction exactly. The client never divides money.
  const splitEqually = useMutation({
    mutationFn: () =>
      api.setTransactionSplits(transaction.transaction_id, { equal: selected }),
    onSuccess: refresh,
  })

  const settle = useMutation({
    mutationFn: api.settleSplit,
    onSuccess: refresh,
  })

  const unsettle = useMutation({
    mutationFn: api.unsettleSplit,
    onSuccess: refresh,
  })

  const clear = useMutation({
    mutationFn: () => api.clearTransactionSplits(transaction.transaction_id),
    onSuccess: () => {
      refresh()
      onClose()
    },
  })

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-ink-950/70 p-4">
      <div className="glass max-h-[85vh] w-full max-w-md overflow-y-auto p-6">
        <h3 className="text-lg font-medium">{transaction.name}</h3>
        <p className="mt-1 text-sm text-mist-500">
          {transaction.date} · {formatMoney(transaction.amount)}
        </p>

        {!!splits.data?.shares.length && (
          <ul className="mt-4 divide-y divide-white/5 text-sm">
            {splits.data.shares.map((sh) => (
              <li key={sh.id} className="flex items-center gap-3 py-2.5">
                <span>{sh.person_name}</span>
                <span className="ml-auto tabular">{formatMoney(sh.amount)}</span>
                {sh.settled_at ? (
                  <button
                    className="shrink-0 text-xs text-mist-500 hover:underline"
                    onClick={() => unsettle.mutate(sh.id)}
                  >
                    settled
                  </button>
                ) : (
                  <button
                    className="shrink-0 text-xs text-rune-300 hover:underline"
                    onClick={() => settle.mutate(sh.id)}
                  >
                    mark settled
                  </button>
                )}
              </li>
            ))}
          </ul>
        )}

        <div className="mt-6">
          <h4 className="text-sm font-medium text-mist-300">Split evenly between</h4>
          <div className="mt-2 space-y-1.5">
            {people.map((p) => (
              <label key={p.id} className="flex items-center gap-2 text-sm">
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
                <span>{p.display_name}</span>
              </label>
            ))}
          </div>

          {splitEqually.isError && (
            <p role="alert" className="mt-3 text-sm text-ember-400">
              {splitEqually.error.message}
            </p>
          )}

          <button
            className="btn-primary mt-3 w-full"
            disabled={selected.length === 0 || splitEqually.isPending}
            onClick={() => splitEqually.mutate()}
          >
            {splitEqually.isPending ? 'Splitting…' : `Split ${selected.length} ways`}
          </button>
        </div>

        <div className="mt-6 flex justify-between gap-3">
          <button
            className="text-sm text-ember-400 hover:underline"
            onClick={() => clear.mutate()}
          >
            Remove split
          </button>
          <button className="btn-ghost" onClick={onClose}>
            Done
          </button>
        </div>
      </div>
    </div>
  )
}
