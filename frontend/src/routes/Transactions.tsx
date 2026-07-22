import { useState } from 'react'
import { useQuery, keepPreviousData } from '@tanstack/react-query'
import { api, type Account, type Transaction } from '../lib/api'
import { formatDate, formatTransactionAmount } from '../lib/money'

const PAGE_SIZE = 50

/** Turns Plaid's SCREAMING_SNAKE categories into readable labels. */
function categoryLabel(primary: string | null): string {
  if (!primary) return 'Uncategorised'
  return primary
    .toLowerCase()
    .split('_')
    .map((w) => w.charAt(0).toUpperCase() + w.slice(1))
    .join(' ')
}

export function Transactions() {
  const today = new Date().toISOString().slice(0, 10)
  const yearAgo = new Date(Date.now() - 365 * 864e5).toISOString().slice(0, 10)

  const [from, setFrom] = useState(yearAgo)
  const [to, setTo] = useState(today)
  const [accountID, setAccountID] = useState('') // '' = all accounts
  const [page, setPage] = useState(0)

  const accounts = useQuery({ queryKey: ['accounts'], queryFn: api.accounts })

  const transactions = useQuery({
    queryKey: ['transactions', from, to, accountID, page],
    queryFn: () =>
      api.transactions({
        from,
        to,
        account_id: accountID,
        limit: PAGE_SIZE,
        offset: page * PAGE_SIZE,
      }),
    // Keeps the previous page on screen while the next loads, so paging does
    // not flash an empty table.
    placeholderData: keepPreviousData,
  })

  const rows = transactions.data ?? []
  const isLastPage = rows.length < PAGE_SIZE

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold">Transactions</h1>
        <p className="mt-1 text-mist-300">
          Everything Ledgermancy has pulled in, newest first.
        </p>
      </div>

      <div className="glass flex flex-wrap items-end gap-4 p-4">
        <div>
          <label className="label" htmlFor="from">
            From
          </label>
          <input
            id="from"
            type="date"
            className="field"
            value={from}
            onChange={(e) => {
              setFrom(e.target.value)
              setPage(0)
            }}
          />
        </div>
        <div>
          <label className="label" htmlFor="to">
            To
          </label>
          <input
            id="to"
            type="date"
            className="field"
            value={to}
            onChange={(e) => {
              setTo(e.target.value)
              setPage(0)
            }}
          />
        </div>
        <div>
          <label className="label" htmlFor="account">
            Account
          </label>
          <select
            id="account"
            className="field"
            value={accountID}
            onChange={(e) => {
              setAccountID(e.target.value)
              setPage(0)
            }}
          >
            <option value="">All accounts</option>
            {groupByInstitution(accounts.data ?? []).map(([institution, accts]) => (
              <optgroup key={institution} label={institution}>
                {accts.map((a) => (
                  <option key={a.id} value={a.id}>
                    {a.name}
                    {a.mask ? ` ••${a.mask}` : ''}
                  </option>
                ))}
              </optgroup>
            ))}
          </select>
        </div>

        <p className="ml-auto text-sm text-mist-500">
          {transactions.isFetching ? 'Loading…' : `${rows.length} shown`}
        </p>
      </div>

      <section className="glass overflow-hidden">
        {rows.length === 0 && !transactions.isFetching ? (
          <p className="px-6 py-12 text-center text-sm text-mist-500">
            No transactions in this range. Connect an account to get started.
          </p>
        ) : (
          <ul className="divide-y divide-white/5">
            {rows.map((t) => (
              <TransactionRow key={t.id} transaction={t} />
            ))}
          </ul>
        )}
      </section>

      <div className="flex items-center justify-between">
        <button
          className="btn-ghost text-sm"
          disabled={page === 0}
          onClick={() => setPage((p) => Math.max(0, p - 1))}
        >
          ← Previous
        </button>
        <span className="text-sm text-mist-500">Page {page + 1}</span>
        <button
          className="btn-ghost text-sm"
          disabled={isLastPage}
          onClick={() => setPage((p) => p + 1)}
        >
          Next →
        </button>
      </div>
    </div>
  )
}

/** Groups accounts by institution for the filter dropdown's optgroups. */
function groupByInstitution(accounts: Account[]): [string, Account[]][] {
  const groups = new Map<string, Account[]>()
  for (const a of accounts) {
    const key = a.institution_name ?? 'Other'
    const list = groups.get(key)
    if (list) list.push(a)
    else groups.set(key, [a])
  }
  return [...groups.entries()]
}

function TransactionRow({ transaction: t }: { transaction: Transaction }) {
  const amount = formatTransactionAmount(t.amount, t.currency)

  return (
    <li className="flex items-center gap-4 px-6 py-3.5">
      <div className="w-24 shrink-0 text-sm text-mist-500">
        {formatDate(t.date)}
      </div>

      <div className="min-w-0 flex-1">
        <p className="truncate font-medium">{t.merchant_name ?? t.name}</p>
        <p className="truncate text-xs text-mist-500">
          {t.account_name}
          {t.institution_name && ` · ${t.institution_name}`}
        </p>
      </div>

      <span className="hidden shrink-0 rounded-full border border-white/10 px-2.5 py-1 text-xs text-mist-300 sm:inline">
        {categoryLabel(t.plaid_category_primary)}
      </span>

      {t.pending && (
        <span className="shrink-0 rounded-full border border-rune-400/30 bg-rune-400/10 px-2.5 py-1 text-xs text-rune-300">
          Pending
        </span>
      )}

      <div
        className={`tabular w-28 shrink-0 text-right font-medium ${
          amount.isIncome ? 'text-verdant-400' : 'text-mist-100'
        }`}
      >
        {amount.text}
      </div>
    </li>
  )
}
