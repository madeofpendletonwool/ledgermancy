import { useState, type FormEvent } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '../lib/api'
import { formatMoney } from '../lib/money'

/**
 * The child view.
 *
 * Deliberately its own route tree rather than the adult UI with sections
 * hidden — hidden sections leak through URLs, and every endpoint behind this
 * page is separately guarded server-side anyway.
 *
 * Everything here is scoped to the signed-in person: their balance, their
 * goals, the accounts held for them. There is no household figure anywhere on
 * this page, by design.
 */
export function MyMoney() {
  const person = useQuery({ queryKey: ['my-person'], queryFn: api.myPerson })
  const allowance = useQuery({ queryKey: ['my-allowance'], queryFn: api.myAllowance })
  const entries = useQuery({
    queryKey: ['my-allowance-entries'],
    queryFn: api.myAllowanceEntries,
  })
  const goals = useQuery({ queryKey: ['my-goals'], queryFn: api.myGoals })
  const accounts = useQuery({ queryKey: ['my-accounts'], queryFn: api.myAccounts })

  return (
    <div className="space-y-8">
      <div>
        <h1 className="text-2xl font-semibold">
          {person.data ? `Hi, ${person.data.display_name}` : 'My money'}
        </h1>
        <p className="mt-1 text-mist-300">Everything that's yours, in one place.</p>
      </div>

      {allowance.data && (
        <section className="glass p-6">
          <p className="text-xs uppercase tracking-wide text-mist-500">You have</p>
          <p className="mt-1 text-4xl font-semibold tabular-nums">
            {formatMoney(allowance.data.balance)}
          </p>

          {allowance.data.amount && allowance.data.cadence && (
            <p className="mt-2 text-sm text-mist-300">
              {formatMoney(allowance.data.amount)} {allowance.data.cadence}
            </p>
          )}

          {allowance.data.limit_remaining !== null && (
            <p className="mt-1 text-sm text-mist-500">
              {formatMoney(allowance.data.limit_remaining)} left to spend this month
            </p>
          )}

          {/* Never let this read as a bank balance. It is a record a parent and
              a child keep together. */}
          <p className="mt-3 text-xs text-mist-500">
            This is a record you and your family keep here — it isn't a bank
            account.
          </p>

          <RecordSpendForm />
        </section>
      )}

      {!!goals.data?.length && (
        <section className="glass p-6">
          <h2 className="text-lg font-medium">What you're saving for</h2>
          <ul className="mt-4 space-y-4">
            {goals.data.map((g) => {
              // Progress comes from the server. Percentages are display-only —
              // never sum or divide money in JavaScript.
              const pct = Math.min(
                100,
                Math.max(
                  0,
                  (Number(g.current_amount) / Number(g.target_amount)) * 100 || 0,
                ),
              )
              return (
                <li key={g.id}>
                  <div className="flex items-baseline justify-between gap-3">
                    <span className="font-medium">{g.name}</span>
                    <span className="text-sm tabular-nums text-mist-300">
                      {formatMoney(g.current_amount)} of {formatMoney(g.target_amount)}
                    </span>
                  </div>
                  <div className="mt-2 h-2.5 overflow-hidden rounded-full bg-white/5">
                    <div
                      className="h-full rounded-full bg-rune-400 transition-[width]"
                      style={{ width: `${pct}%` }}
                    />
                  </div>
                  {g.achieved && (
                    <p className="mt-1 text-xs text-rune-300">You did it! 🎉</p>
                  )}
                </li>
              )
            })}
          </ul>
        </section>
      )}

      {!!accounts.data?.length && (
        <section className="glass p-6">
          <h2 className="text-lg font-medium">Accounts in your name</h2>
          <p className="mt-1 text-sm text-mist-300">
            Money that's being saved for you. You can watch it grow, and a
            grown-up looks after it.
          </p>
          <ul className="mt-4 divide-y divide-white/5">
            {accounts.data.map((a) => (
              <li key={a.id} className="flex items-center gap-3 py-3">
                <div className="min-w-0">
                  <p className="truncate font-medium">{a.name}</p>
                  <p className="truncate text-sm text-mist-500">
                    {a.institution_name ?? 'Account'}
                    {a.is_custodial && ' · held for you'}
                  </p>
                </div>
                <span className="ml-auto shrink-0 tabular-nums">
                  {formatMoney(a.balance)}
                </span>
              </li>
            ))}
          </ul>
        </section>
      )}

      {!!entries.data?.length && (
        <section className="glass p-6">
          <h2 className="text-lg font-medium">Recent</h2>
          <ul className="mt-4 divide-y divide-white/5 text-sm">
            {entries.data.slice(0, 20).map((e) => (
              <li key={e.id} className="flex items-center gap-3 py-2.5">
                <span className="capitalize">{e.note || e.kind}</span>
                <span className="text-xs text-mist-500">{e.occurred_on}</span>
                <span
                  className={`ml-auto tabular-nums ${
                    e.amount.startsWith('-') ? 'text-ember-400' : 'text-rune-300'
                  }`}
                >
                  {formatMoney(e.amount)}
                </span>
              </li>
            ))}
          </ul>
        </section>
      )}
    </div>
  )
}

/**
 * A child records their own spending. This is the only write a child login has,
 * and it is deliberate: a ledger a kid cannot write to teaches nothing. Credits
 * stay a parent's action, enforced server-side.
 */
function RecordSpendForm() {
  const qc = useQueryClient()
  const [amount, setAmount] = useState('')
  const [note, setNote] = useState('')

  const record = useMutation({
    mutationFn: () => api.recordMySpend(amount, note || undefined),
    onSuccess: () => {
      setAmount('')
      setNote('')
      qc.invalidateQueries({ queryKey: ['my-allowance'] })
      qc.invalidateQueries({ queryKey: ['my-allowance-entries'] })
    },
  })

  function onSubmit(e: FormEvent) {
    e.preventDefault()
    record.mutate()
  }

  return (
    <form onSubmit={onSubmit} className="mt-5 space-y-3">
      <h3 className="text-sm font-medium text-mist-300">I spent some</h3>
      <div className="flex flex-col gap-3 sm:flex-row">
        <input
          required
          className="field sm:w-32"
          placeholder="Amount"
          inputMode="decimal"
          value={amount}
          onChange={(e) => setAmount(e.target.value)}
        />
        <input
          className="field sm:flex-1"
          placeholder="On what?"
          value={note}
          onChange={(e) => setNote(e.target.value)}
        />
        <button className="btn-primary shrink-0" disabled={record.isPending}>
          {record.isPending ? 'Saving…' : 'Add'}
        </button>
      </div>
      {record.isError && (
        <p role="alert" className="text-sm text-ember-400">
          {record.error.message}
        </p>
      )}
    </form>
  )
}
