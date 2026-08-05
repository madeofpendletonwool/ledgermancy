import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  api,
  type Account,
  type AccountBalanceEntry,
  type ManualAccountInput,
} from '../lib/api'
import { formatMoney, isLiability } from '../lib/money'
import { OFFLINE_WRITE_HINT, useOnline } from '../lib/offline'

/**
 * Accounts that exist without Plaid.
 *
 * Some institutions cannot be linked at all — TreasuryDirect, a private
 * holding — and some fail every link attempt and are not going to be fixed. A
 * Voya retirement plan is the live example. Before this, such an account simply
 * could not be recorded, so its balance was missing from net worth, its
 * contributions were missing from cash flow, and the household's actual
 * position was understated by however much sat in it.
 *
 * The trade the user is making here is explicit and worth being honest about in
 * the UI: nothing keeps these figures current except them. That is why the
 * balance editor records a reason and keeps a dated history rather than just
 * overwriting a number — the trail is what makes a stale figure visible.
 */

const ACCOUNT_TYPES: { value: string; label: string }[] = [
  { value: 'depository', label: 'Cash / bank' },
  { value: 'investment', label: 'Investment' },
  { value: 'brokerage', label: 'Brokerage' },
  { value: 'credit', label: 'Credit card' },
  { value: 'loan', label: 'Loan' },
  { value: 'other', label: 'Other' },
]

const BALANCE_REASONS: { value: string; label: string }[] = [
  { value: 'manual', label: 'Updated balance' },
  { value: 'holding_revalue', label: 'Repriced holdings' },
  { value: 'dividend', label: 'Dividend' },
  { value: 'fee', label: 'Fee' },
]

/** Opens the create form. Sits beside ConnectAccount, because "add an account"
 *  is one intent with two answers and splitting them across the page would make
 *  the manual path look like a lesser feature rather than a different one. */
export function AddManualAccount() {
  const [open, setOpen] = useState(false)
  const online = useOnline()

  return (
    <>
      <button
        className="btn-ghost px-3 py-1.5 text-sm"
        disabled={!online}
        title={online ? undefined : OFFLINE_WRITE_HINT}
        onClick={() => setOpen(true)}
      >
        + Add manually
      </button>
      {open && <ManualAccountModal onClose={() => setOpen(false)} />}
    </>
  )
}

function ManualAccountModal({
  account,
  onClose,
}: {
  account?: Account
  onClose: () => void
}) {
  const qc = useQueryClient()
  const editing = account !== undefined

  const [name, setName] = useState(account?.name ?? '')
  const [type, setType] = useState(account?.type ?? 'depository')
  const [subtype, setSubtype] = useState(account?.subtype ?? '')
  const [mask, setMask] = useState(account?.mask ?? '')
  const [balance, setBalance] = useState('')
  const [isShared, setIsShared] = useState(account?.is_shared ?? true)
  const [error, setError] = useState<string | null>(null)

  const save = useMutation({
    mutationFn: (input: ManualAccountInput) =>
      editing
        ? api.updateManualAccount(account.id, input)
        : api.createManualAccount(input),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['accounts'] })
      qc.invalidateQueries({ queryKey: ['networth'] })
      qc.invalidateQueries({ queryKey: ['investments'] })
      onClose()
    },
    onError: (err: Error) => setError(err.message),
  })

  const submit = () => {
    setError(null)
    if (!name.trim()) {
      setError('Give the account a name.')
      return
    }
    save.mutate({
      name: name.trim(),
      type,
      subtype: subtype.trim() || null,
      mask: mask.trim() || null,
      is_shared: isShared,
      // Balance is an OPENING balance and only meaningful on create. Editing an
      // account must not move its balance silently — that goes through the
      // balance editor, which records why.
      balance: editing ? undefined : balance.trim() || null,
    })
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4">
      <div className="glass w-full max-w-md space-y-4 p-6">
        <h2 className="text-lg font-medium">
          {editing ? 'Edit account' : 'Add an account manually'}
        </h2>
        {!editing && (
          <p className="text-xs text-mist-400">
            For accounts Ledgermancy cannot link. You keep the balance up to
            date; nothing syncs it for you.
          </p>
        )}

        <label className="block text-sm">
          <span className="text-mist-300">Name</span>
          <input
            className="input mt-1 w-full"
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="Voya 401(k)"
          />
        </label>

        <div className="grid grid-cols-2 gap-3">
          <label className="block text-sm">
            <span className="text-mist-300">Type</span>
            <select
              className="input mt-1 w-full"
              value={type}
              onChange={(e) => setType(e.target.value)}
            >
              {ACCOUNT_TYPES.map((t) => (
                <option key={t.value} value={t.value}>
                  {t.label}
                </option>
              ))}
            </select>
          </label>
          <label className="block text-sm">
            <span className="text-mist-300">Subtype</span>
            <input
              className="input mt-1 w-full"
              value={subtype}
              onChange={(e) => setSubtype(e.target.value)}
              placeholder="401k"
            />
          </label>
        </div>

        <div className="grid grid-cols-2 gap-3">
          <label className="block text-sm">
            <span className="text-mist-300">Last 4 digits</span>
            <input
              className="input mt-1 w-full"
              value={mask}
              onChange={(e) => setMask(e.target.value)}
              placeholder="optional"
            />
          </label>
          {!editing && (
            <label className="block text-sm">
              <span className="text-mist-300">Opening balance</span>
              <input
                className="input mt-1 w-full"
                value={balance}
                onChange={(e) => setBalance(e.target.value)}
                placeholder="0.00"
                inputMode="decimal"
              />
            </label>
          )}
        </div>

        <label className="flex cursor-pointer items-center gap-2 text-sm text-mist-300">
          <input
            type="checkbox"
            className="accent-arcane-500"
            checked={isShared}
            onChange={(e) => setIsShared(e.target.checked)}
          />
          Shared with household
        </label>

        {error && <p className="text-sm text-ember-400">{error}</p>}

        <div className="flex items-center gap-3">
          <button className="btn-primary px-4 py-2 text-sm" disabled={save.isPending} onClick={submit}>
            {save.isPending ? 'Saving…' : editing ? 'Save' : 'Add account'}
          </button>
          <button type="button" className="text-sm text-mist-400 underline" onClick={onClose}>
            Cancel
          </button>
        </div>
      </div>
    </div>
  )
}

/** The card holding every manual account, rendered alongside the institution
 *  cards. Kept separate rather than folded into one of them because these rows
 *  answer to nobody but the user — there is no sync, no reconnect and no
 *  institution to name. */
export function ManualAccountsCard({ accounts }: { accounts: Account[] }) {
  if (accounts.length === 0) return null

  return (
    <section className="glass overflow-hidden">
      <header className="flex flex-wrap items-center gap-4 border-b border-white/5 px-6 py-4">
        <div>
          <h2 className="font-medium">
            Manual accounts
            <span className="ml-2 rounded-full border border-arcane-500/30 bg-arcane-500/10 px-2 py-0.5 text-xs font-normal text-arcane-300">
              not linked
            </span>
          </h2>
          <p className="mt-0.5 text-xs text-mist-500">
            Balances here are whatever you last entered. Nothing updates them
            for you.
          </p>
        </div>
      </header>

      <ul className="divide-y divide-white/5">
        {accounts.map((a) => (
          <ManualAccountRow key={a.id} account={a} />
        ))}
      </ul>
    </section>
  )
}

function ManualAccountRow({ account }: { account: Account }) {
  const qc = useQueryClient()
  const online = useOnline()
  const [editing, setEditing] = useState(false)
  const [balanceOpen, setBalanceOpen] = useState(false)

  const remove = useMutation({
    mutationFn: () => api.deleteManualAccount(account.id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['accounts'] })
      qc.invalidateQueries({ queryKey: ['networth'] })
    },
  })

  return (
    <li className="px-6 py-3.5">
      <div className="flex items-center gap-4">
        <div className="min-w-0">
          <p className="truncate font-medium">
            {account.name}
            {account.mask && <span className="text-mist-500"> ••{account.mask}</span>}
          </p>
          <p className="text-xs text-mist-500">
            {account.subtype ?? account.type}
            {!account.is_own && ' · added by household member'}
          </p>
        </div>
        <div className="ml-auto text-right">
          <p
            className={`tabular font-medium ${
              isLiability(account.type) ? 'text-ember-400' : 'text-rune-300'
            }`}
          >
            {formatMoney(account.current_balance, account.currency)}
          </p>
          {isLiability(account.type) && <p className="text-xs text-mist-500">owed</p>}
        </div>
      </div>

      <div className="mt-2 flex flex-wrap items-center gap-3 text-xs">
        <button className="text-mist-400 underline" onClick={() => setBalanceOpen((v) => !v)}>
          {balanceOpen ? 'Hide balance history' : 'Update balance'}
        </button>
        <button className="text-mist-400 underline" onClick={() => setEditing(true)}>
          Edit
        </button>
        <button
          className="text-mist-500 transition hover:text-ember-400"
          disabled={remove.isPending || !online}
          title={online ? undefined : OFFLINE_WRITE_HINT}
          onClick={() => {
            if (
              confirm(
                `Delete ${account.name}? Its balance history and any transactions recorded against it go too.`,
              )
            ) {
              remove.mutate()
            }
          }}
        >
          Delete
        </button>
      </div>

      {balanceOpen && <BalanceEditor account={account} />}
      {editing && <ManualAccountModal account={account} onClose={() => setEditing(false)} />}
    </li>
  )
}

/**
 * Records a new balance and shows the trail behind the current one.
 *
 * The reason picker is not decoration. A manual balance is only as good as the
 * last time somebody looked, and a dated history with a stated reason is what
 * lets a figure be judged — "$48,200 as of March, repriced holdings" is a claim
 * you can assess, where a bare number is not.
 */
function BalanceEditor({ account }: { account: Account }) {
  const qc = useQueryClient()
  const online = useOnline()
  const [balance, setBalance] = useState('')
  const [asOf, setAsOf] = useState(new Date().toISOString().slice(0, 10))
  const [reason, setReason] = useState('manual')
  const [note, setNote] = useState('')
  const [error, setError] = useState<string | null>(null)

  const history = useQuery({
    queryKey: ['balance-history', account.id],
    queryFn: () => api.accountBalanceHistory(account.id),
  })

  const save = useMutation({
    mutationFn: () =>
      api.setManualAccountBalance(account.id, {
        balance: balance.trim(),
        as_of: asOf,
        reason: reason as 'manual' | 'holding_revalue' | 'fee' | 'dividend',
        note: note.trim() || null,
      }),
    onSuccess: () => {
      setBalance('')
      setNote('')
      qc.invalidateQueries({ queryKey: ['accounts'] })
      qc.invalidateQueries({ queryKey: ['balance-history', account.id] })
      qc.invalidateQueries({ queryKey: ['networth'] })
      qc.invalidateQueries({ queryKey: ['investments'] })
    },
    onError: (err: Error) => setError(err.message),
  })

  return (
    <div className="mt-3 rounded-lg border border-white/5 bg-black/20 p-4">
      <div className="grid gap-3 sm:grid-cols-4">
        <label className="block text-xs">
          <span className="text-mist-400">New balance</span>
          <input
            className="input mt-1 w-full"
            value={balance}
            onChange={(e) => setBalance(e.target.value)}
            placeholder="0.00"
            inputMode="decimal"
          />
        </label>
        <label className="block text-xs">
          <span className="text-mist-400">As of</span>
          <input
            type="date"
            className="input mt-1 w-full"
            value={asOf}
            onChange={(e) => setAsOf(e.target.value)}
          />
        </label>
        <label className="block text-xs">
          <span className="text-mist-400">Reason</span>
          <select
            className="input mt-1 w-full"
            value={reason}
            onChange={(e) => setReason(e.target.value)}
          >
            {BALANCE_REASONS.map((r) => (
              <option key={r.value} value={r.value}>
                {r.label}
              </option>
            ))}
          </select>
        </label>
        <label className="block text-xs">
          <span className="text-mist-400">Note</span>
          <input
            className="input mt-1 w-full"
            value={note}
            onChange={(e) => setNote(e.target.value)}
            placeholder="optional"
          />
        </label>
      </div>

      {error && <p className="mt-2 text-xs text-ember-400">{error}</p>}

      <button
        className="btn-primary mt-3 px-3 py-1.5 text-xs"
        disabled={save.isPending || !balance.trim() || !online}
        title={online ? undefined : OFFLINE_WRITE_HINT}
        onClick={() => {
          setError(null)
          save.mutate()
        }}
      >
        {save.isPending ? 'Saving…' : 'Record balance'}
      </button>

      <BalanceHistory entries={history.data ?? []} currency={account.currency} />
    </div>
  )
}

function BalanceHistory({
  entries,
  currency,
}: {
  entries: AccountBalanceEntry[]
  currency: string
}) {
  if (entries.length === 0) return null

  // Newest first: the question this answers is almost always "when did I last
  // check", not "what did it start at".
  const rows = [...entries].reverse()

  return (
    <div className="mt-4">
      <p className="text-xs font-medium text-mist-400">History</p>
      <ul className="mt-1 space-y-1">
        {rows.map((h) => (
          <li key={h.as_of} className="flex items-baseline gap-2 text-xs text-mist-500">
            <span className="tabular">{h.as_of}</span>
            <span className="tabular text-rune-300">{formatMoney(h.balance, currency)}</span>
            <span>
              {BALANCE_REASONS.find((r) => r.value === h.reason)?.label ??
                (h.reason === 'scheduled' ? 'Scheduled contribution' : h.reason)}
            </span>
            {h.note && <span className="truncate text-mist-600">— {h.note}</span>}
          </li>
        ))}
      </ul>
    </div>
  )
}
