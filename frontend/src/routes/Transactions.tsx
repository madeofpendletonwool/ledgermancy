import { useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { useMutation, useQuery, useQueryClient, keepPreviousData } from '@tanstack/react-query'
import {
  api,
  type Account,
  type Category,
  type ManualTransactionInput,
  type Transaction,
} from '../lib/api'
import { formatDate, formatTransactionAmount } from '../lib/money'
import { ImportTransactionsModal } from '../components/ImportTransactionsModal'

const PAGE_SIZE = 50

// modalState drives the add/edit dialog: null = closed, otherwise create or a
// specific manual row being edited.
type ModalState =
  | { mode: 'create' }
  | { mode: 'edit'; transaction: Transaction }
  | null

export function Transactions() {
  const today = new Date().toISOString().slice(0, 10)
  const yearAgo = new Date(Date.now() - 365 * 864e5).toISOString().slice(0, 10)

  // Filters live in the URL so the Dashboard/Spending charts can deep-link into
  // a filtered view (one day, one category) and the browser back button
  // restores it. searchParams is the single source of truth; there is no
  // duplicate local filter state to keep in sync.
  const [searchParams, setSearchParams] = useSearchParams()
  const from = searchParams.get('from') || yearAgo
  const to = searchParams.get('to') || today
  const accountIDs = (searchParams.get('accounts') || '').split(',').filter(Boolean)
  const categoryFilter = searchParams.get('category') || ''
  const onlyUncat = searchParams.get('uncat') === '1'
  const page = Math.max(0, Number(searchParams.get('page') || '0') || 0)

  const [modal, setModal] = useState<ModalState>(null)
  const [importing, setImporting] = useState(false)

  // patchParams writes filter changes back to the URL. Any change other than an
  // explicit page move resets to page 0, so a new filter never lands you past
  // the end of the (now shorter) result set.
  const patchParams = (patch: Record<string, string | null>, keepPage = false) => {
    setSearchParams(
      (prev) => {
        const next = new URLSearchParams(prev)
        for (const [k, v] of Object.entries(patch)) {
          if (v === null || v === '') next.delete(k)
          else next.set(k, v)
        }
        if (!keepPage) next.delete('page')
        return next
      },
      { replace: true },
    )
  }
  const setPage = (p: number) => patchParams({ page: p <= 0 ? null : String(p) }, true)
  const toggleAccount = (id: string) => {
    const set = new Set(accountIDs)
    set.has(id) ? set.delete(id) : set.add(id)
    patchParams({ accounts: [...set].join(',') || null })
  }

  const accounts = useQuery({ queryKey: ['accounts'], queryFn: api.accounts })
  const categories = useQuery({ queryKey: ['categories'], queryFn: api.categories })

  // id → category, for showing each row's resolved (app) category, and the list
  // a user can pick from when recategorising.
  const categoryById = new Map((categories.data ?? []).map((c) => [c.id, c]))
  const spendCats = spendCategories(categories.data ?? [])

  const transactions = useQuery({
    queryKey: ['transactions', from, to, accountIDs.join(','), categoryFilter, onlyUncat, page],
    queryFn: () =>
      api.transactions({
        from,
        to,
        accounts: accountIDs,
        category_id: categoryFilter || undefined,
        uncategorised: onlyUncat || undefined,
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
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <h1 className="text-2xl font-semibold">Transactions</h1>
          <p className="mt-1 text-mist-300">
            Everything Ledgermancy has pulled in, newest first.
          </p>
        </div>
        <div className="flex gap-2">
          <button
            className="btn-ghost px-4 py-2 text-sm"
            onClick={() => setImporting(true)}
          >
            Import CSV
          </button>
          <button
            className="btn-primary px-4 py-2 text-sm"
            onClick={() => setModal({ mode: 'create' })}
          >
            Add transaction
          </button>
        </div>
      </div>

      {/* relative z-20 lifts this whole bar (and the account dropdown that
          overflows it) above the transactions panel below, which is a sibling
          `glass` layer that would otherwise paint on top of the popover. */}
      <div className="glass relative z-20 flex flex-wrap items-end gap-4 p-4">
        <div>
          <label className="label" htmlFor="from">
            From
          </label>
          <input
            id="from"
            type="date"
            className="field"
            value={from}
            onChange={(e) => patchParams({ from: e.target.value })}
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
            onChange={(e) => patchParams({ to: e.target.value })}
          />
        </div>
        <div>
          <span className="label">Accounts</span>
          <AccountMultiSelect
            accounts={accounts.data ?? []}
            selected={accountIDs}
            onToggle={toggleAccount}
            onClear={() => patchParams({ accounts: null })}
          />
        </div>
        <div>
          <label className="label" htmlFor="category">
            Category
          </label>
          <select
            id="category"
            className="field"
            value={categoryFilter}
            onChange={(e) => patchParams({ category: e.target.value || null })}
          >
            <option value="">All categories</option>
            {(categories.data ?? []).map((c) => (
              <option key={c.id} value={c.id}>
                {c.name}
              </option>
            ))}
          </select>
        </div>

        <label className="flex items-center gap-2 pb-2 text-sm text-mist-300">
          <input
            type="checkbox"
            checked={onlyUncat}
            onChange={(e) => patchParams({ uncat: e.target.checked ? '1' : null })}
          />
          Needs a category
        </label>

        <p className="ml-auto text-sm text-mist-500">
          {transactions.isFetching ? 'Loading…' : `${rows.length} shown`}
        </p>
      </div>

      <section className="glass overflow-hidden">
        {rows.length === 0 && !transactions.isFetching ? (
          <p className="px-6 py-12 text-center text-sm text-mist-500">
            No transactions in this range. Connect an account or add one by hand.
          </p>
        ) : (
          <ul className="divide-y divide-white/5">
            {rows.map((t) => (
              <TransactionRow
                key={t.id}
                transaction={t}
                categoryById={categoryById}
                spendCats={spendCats}
                onEdit={() => setModal({ mode: 'edit', transaction: t })}
              />
            ))}
          </ul>
        )}
      </section>

      <div className="flex items-center justify-between">
        <button
          className="btn-ghost text-sm"
          disabled={page === 0}
          onClick={() => setPage(page - 1)}
        >
          ← Previous
        </button>
        <span className="text-sm text-mist-500">Page {page + 1}</span>
        <button
          className="btn-ghost text-sm"
          disabled={isLastPage}
          onClick={() => setPage(page + 1)}
        >
          Next →
        </button>
      </div>

      {modal && (
        <ManualTransactionModal
          state={modal}
          accounts={accounts.data ?? []}
          defaultAccountID={accountIDs[0] ?? ''}
          onClose={() => setModal(null)}
        />
      )}

      {importing && (
        <ImportTransactionsModal
          accounts={accounts.data ?? []}
          onClose={() => setImporting(false)}
        />
      )}
    </div>
  )
}

// AccountMultiSelect is a checkbox dropdown over the household's accounts. An
// empty selection means "all accounts", so the common case needs no clicks. It
// uses a native <details> for the popover — no outside-click plumbing, and it
// closes on its own when another one opens is not needed here.
function AccountMultiSelect({
  accounts,
  selected,
  onToggle,
  onClear,
}: {
  accounts: Account[]
  selected: string[]
  onToggle: (id: string) => void
  onClear: () => void
}) {
  const label =
    selected.length === 0
      ? 'All accounts'
      : selected.length === 1
        ? (accounts.find((a) => a.id === selected[0])?.name ?? '1 account')
        : `${selected.length} accounts`

  return (
    <details className="relative">
      <summary className="field flex w-56 cursor-pointer list-none items-center justify-between">
        <span className="truncate">{label}</span>
        <span className="ml-2 text-mist-500">▾</span>
      </summary>
      <div className="absolute z-30 mt-1 max-h-72 w-64 overflow-auto rounded-2xl border border-white/10 bg-ink-950/90 p-1.5 shadow-xl shadow-black/40 backdrop-blur-xl">
        <button
          className="w-full rounded-lg px-2 py-1.5 text-left text-sm text-mist-300 hover:bg-white/5"
          onClick={onClear}
        >
          All accounts
        </button>
        {groupByInstitution(accounts).map(([institution, accts]) => (
          <div key={institution} className="mt-1">
            <p className="px-2 pt-1 text-[11px] uppercase tracking-wide text-mist-500">
              {institution}
            </p>
            {accts.map((a) => (
              <label
                key={a.id}
                className="flex cursor-pointer items-center gap-2 rounded-lg px-2 py-1.5 text-sm hover:bg-white/5"
              >
                <input
                  type="checkbox"
                  checked={selected.includes(a.id)}
                  onChange={() => onToggle(a.id)}
                />
                <span className="truncate">
                  {a.name}
                  {a.mask ? ` ••${a.mask}` : ''}
                </span>
              </label>
            ))}
          </div>
        ))}
      </div>
    </details>
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

function TransactionRow({
  transaction: t,
  categoryById,
  spendCats,
  onEdit,
}: {
  transaction: Transaction
  categoryById: Map<string, Category>
  spendCats: Category[]
  onEdit: () => void
}) {
  const qc = useQueryClient()
  const amount = formatTransactionAmount(t.amount, t.currency)
  const isManual = t.source === 'manual'
  const [editingCat, setEditingCat] = useState(false)

  const remove = useMutation({
    mutationFn: () => api.deleteTransaction(t.id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['transactions'] }),
  })

  // The app's resolved category (falls back to "Uncategorised"), not Plaid's raw
  // guess. A category the list didn't include (e.g. an income/transfer) shows by
  // whatever name we have, or the fallback.
  const current = t.category_id ? categoryById.get(t.category_id) : undefined
  const currentLabel = current?.name ?? 'Uncategorised'

  return (
    <li className="flex items-center gap-4 px-6 py-3.5">
      <div className="w-24 shrink-0 text-sm text-mist-500">
        {formatDate(t.date)}
      </div>

      <div className="min-w-0 flex-1">
        <p className="flex items-center gap-2 truncate font-medium">
          <span className="truncate">{t.merchant_name ?? t.name}</span>
          {isManual && (
            <span className="shrink-0 rounded-full border border-arcane-500/30 bg-arcane-500/10 px-2 py-0.5 text-[10px] font-medium uppercase tracking-wide text-arcane-400">
              Manual
            </span>
          )}
        </p>
        <p className="truncate text-xs text-mist-500">
          {t.account_name}
          {t.institution_name && ` · ${t.institution_name}`}
        </p>
        {t.possible_duplicate && (
          <p className="mt-1 flex flex-wrap items-center gap-2 text-xs text-ember-400">
            <span>Possible duplicate — a matching synced charge arrived.</span>
            <button
              className="btn-ghost px-2 py-0.5 text-xs text-ember-400"
              disabled={remove.isPending}
              onClick={() => remove.mutate()}
            >
              Delete my entry
            </button>
          </p>
        )}
      </div>

      {editingCat ? (
        <CategoryEditor
          transaction={t}
          spendCats={spendCats}
          currentID={t.category_id}
          onDone={() => setEditingCat(false)}
        />
      ) : (
        <button
          className="hidden shrink-0 rounded-full border border-white/10 px-2.5 py-1 text-xs text-mist-300 transition hover:border-white/25 hover:text-mist-100 sm:inline"
          title="Change category"
          onClick={() => setEditingCat(true)}
        >
          {currentLabel}
        </button>
      )}

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

      {/* Edit/delete only on hand-entered rows. Plaid rows stay read-only
          except category, which has its own path. */}
      {isManual && (
        <div className="flex shrink-0 items-center gap-1">
          <button
            className="btn-ghost px-2 py-1 text-xs text-mist-300"
            onClick={onEdit}
          >
            Edit
          </button>
          <button
            className="btn-ghost px-2 py-1 text-xs text-ember-400"
            disabled={remove.isPending}
            onClick={() => remove.mutate()}
          >
            Delete
          </button>
        </div>
      )}
    </li>
  )
}

// CategoryEditor is the inline recategorise control. It writes through the
// existing recategorise endpoint; "apply to all from this merchant" both
// remembers the choice for future syncs and retroactively fixes every existing
// charge from that merchant (handled server-side).
function CategoryEditor({
  transaction: t,
  spendCats,
  currentID,
  onDone,
}: {
  transaction: Transaction
  spendCats: Category[]
  currentID: string | null
  onDone: () => void
}) {
  const qc = useQueryClient()
  const [categoryID, setCategoryID] = useState(currentID ?? '')
  const [applyToMerchant, setApplyToMerchant] = useState(false)
  // Gate on merchant_key (what the server caches by), not merchant_name — many
  // Plaid rows have a key derived from the name with no merchant_name set.
  const hasMerchant = Boolean(t.merchant_key)
  const merchantLabel = t.merchant_name ?? t.name

  const save = useMutation({
    mutationFn: () => api.recategorise(t.id, categoryID, applyToMerchant && hasMerchant),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['transactions'] })
      // Category totals feed the reports, so refresh those too.
      qc.invalidateQueries({ queryKey: ['by-category'] })
      qc.invalidateQueries({ queryKey: ['summary'] })
      qc.invalidateQueries({ queryKey: ['averages'] })
      onDone()
    },
  })

  return (
    <div className="flex shrink-0 flex-wrap items-center justify-end gap-2">
      <select
        className="field py-1 text-xs"
        value={categoryID}
        aria-label="Category"
        onChange={(e) => setCategoryID(e.target.value)}
      >
        <option value="" disabled>
          Choose…
        </option>
        {spendCats.map((c) => (
          <option key={c.id} value={c.id}>
            {c.name}
          </option>
        ))}
      </select>

      {hasMerchant && (
        <label className="flex items-center gap-1 text-xs text-mist-400">
          <input
            type="checkbox"
            checked={applyToMerchant}
            onChange={(e) => setApplyToMerchant(e.target.checked)}
          />
          All from {merchantLabel}
        </label>
      )}

      <button
        className="btn-ghost px-2 py-1 text-xs"
        disabled={save.isPending || categoryID === ''}
        onClick={() => save.mutate()}
      >
        Save
      </button>
      <button className="btn-ghost px-2 py-1 text-xs text-mist-300" onClick={onDone}>
        Cancel
      </button>
    </div>
  )
}

// splitAmount turns a signed decimal string into a magnitude + direction for
// the form, so an edited refund starts on the right toggle. abs() on a string
// is done by stripping a leading '-'.
function splitAmount(amount: string): { magnitude: string; income: boolean } {
  const income = amount.trim().startsWith('-')
  return { magnitude: amount.replace(/^-/, ''), income }
}

function ManualTransactionModal({
  state,
  accounts,
  defaultAccountID,
  onClose,
}: {
  state: Exclude<ModalState, null>
  accounts: Account[]
  defaultAccountID: string
  onClose: () => void
}) {
  const qc = useQueryClient()
  const categories = useQuery({ queryKey: ['categories'], queryFn: api.categories })

  const editing = state.mode === 'edit' ? state.transaction : null
  const initialAmount = editing ? splitAmount(editing.amount) : null

  const today = new Date().toISOString().slice(0, 10)
  const [accountID, setAccountID] = useState(
    editing?.account_id ?? defaultAccountID ?? accounts[0]?.id ?? '',
  )
  const [date, setDate] = useState(editing ? editing.date.slice(0, 10) : today)
  const [merchant, setMerchant] = useState(
    editing ? (editing.merchant_name ?? editing.name) : '',
  )
  const [magnitude, setMagnitude] = useState(initialAmount?.magnitude ?? '')
  const [income, setIncome] = useState(initialAmount?.income ?? false)
  const [categoryID, setCategoryID] = useState(editing?.category_id ?? '')
  const [notes, setNotes] = useState(editing?.notes ?? '')

  const save = useMutation({
    mutationFn: (input: ManualTransactionInput) =>
      editing ? api.updateTransaction(editing.id, input) : api.createTransaction(input),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['transactions'] })
      onClose()
    },
  })

  const canSave =
    accountID !== '' && merchant.trim() !== '' && magnitude !== '' && Number(magnitude) > 0

  const submit = () => {
    if (!canSave) return
    // The toggle sets the sign: expense = money out (positive, Plaid's
    // convention), income/refund = negative.
    const signed = income ? `-${magnitude}` : magnitude
    const name = merchant.trim()
    save.mutate({
      account_id: accountID,
      date,
      amount: signed,
      name,
      merchant_name: name,
      category_id: categoryID || null,
      notes: notes.trim() || null,
    })
  }

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4"
      role="dialog"
      aria-modal="true"
      onClick={onClose}
    >
      <div
        className="glass w-full max-w-lg space-y-4 p-6"
        onClick={(e) => e.stopPropagation()}
      >
        <div>
          <h2 className="text-lg font-medium">
            {editing ? 'Edit transaction' : 'Add transaction'}
          </h2>
          <p className="mt-1 text-sm text-mist-300">
            Reconcile a charge your bank feed missed. This corrects your spending
            totals only — it never changes an account balance.
          </p>
        </div>

        <div className="grid gap-4 sm:grid-cols-2">
          <div className="sm:col-span-2">
            <label className="label" htmlFor="mtx-account">
              Account
            </label>
            <select
              id="mtx-account"
              className="field w-full"
              value={accountID}
              onChange={(e) => setAccountID(e.target.value)}
            >
              <option value="" disabled>
                Select an account
              </option>
              {groupByInstitution(accounts).map(([institution, accts]) => (
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

          <div>
            <label className="label" htmlFor="mtx-date">
              Date
            </label>
            <input
              id="mtx-date"
              type="date"
              className="field w-full"
              value={date}
              onChange={(e) => setDate(e.target.value)}
            />
          </div>

          <div>
            <label className="label" htmlFor="mtx-amount">
              Amount
            </label>
            <input
              id="mtx-amount"
              type="number"
              min="0"
              step="0.01"
              className="field w-full"
              placeholder="11.86"
              value={magnitude}
              onChange={(e) => setMagnitude(e.target.value)}
            />
          </div>

          <div className="sm:col-span-2">
            <span className="label">Type</span>
            <div className="mt-1 flex overflow-hidden rounded-lg border border-white/10">
              <button
                type="button"
                className={`flex-1 px-3 py-2 text-sm ${
                  !income ? 'bg-arcane-500/20 text-arcane-400' : 'text-mist-300'
                }`}
                onClick={() => setIncome(false)}
              >
                Expense (money out)
              </button>
              <button
                type="button"
                className={`flex-1 px-3 py-2 text-sm ${
                  income ? 'bg-verdant-400/20 text-verdant-400' : 'text-mist-300'
                }`}
                onClick={() => setIncome(true)}
              >
                Income / refund
              </button>
            </div>
          </div>

          <div className="sm:col-span-2">
            <label className="label" htmlFor="mtx-merchant">
              Merchant / description
            </label>
            <input
              id="mtx-merchant"
              className="field w-full"
              placeholder="Capital One — Amazon charge"
              value={merchant}
              onChange={(e) => setMerchant(e.target.value)}
            />
          </div>

          <div className="sm:col-span-2">
            <label className="label" htmlFor="mtx-category">
              Category (optional)
            </label>
            <select
              id="mtx-category"
              className="field w-full"
              value={categoryID}
              onChange={(e) => setCategoryID(e.target.value)}
            >
              <option value="">Uncategorised</option>
              {spendCategories(categories.data ?? []).map((c) => (
                <option key={c.id} value={c.id}>
                  {c.name}
                </option>
              ))}
            </select>
          </div>

          <div className="sm:col-span-2">
            <label className="label" htmlFor="mtx-notes">
              Notes (optional)
            </label>
            <input
              id="mtx-notes"
              className="field w-full"
              placeholder="Reconciled from July statement"
              value={notes}
              onChange={(e) => setNotes(e.target.value)}
            />
          </div>
        </div>

        <div className="flex items-center gap-3">
          <button
            className="btn-primary px-4 py-2 text-sm"
            disabled={!canSave || save.isPending}
            onClick={submit}
          >
            {save.isPending ? 'Saving…' : editing ? 'Save changes' : 'Add transaction'}
          </button>
          <button
            className="btn-ghost px-3 py-2 text-sm text-mist-300"
            onClick={onClose}
          >
            Cancel
          </button>
          {save.isError && (
            <span role="alert" className="text-sm text-ember-400">
              {save.error.message}
            </span>
          )}
        </div>
      </div>
    </div>
  )
}

/** Categories a manual transaction can be filed under — transfers are noise. */
function spendCategories(categories: Category[]): Category[] {
  return categories.filter((c) => !c.is_transfer)
}
