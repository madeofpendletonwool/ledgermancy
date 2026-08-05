import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  api,
  type InvestmentAccount,
  type InvestmentTransaction,
  type Security,
} from '../lib/api'
import { formatMoney } from '../lib/money'
import { OFFLINE_WRITE_HINT, useOnline } from '../lib/offline'

/**
 * Hand-entering positions and activity for an investment account Plaid cannot
 * reach.
 *
 * The point of this surface is parity: once a position and its contributions
 * are recorded, every engine on the Investments page — allocation, TWR/MWR,
 * dividends, snapshots — reads the same tables it always did and treats a Voya
 * plan exactly like a working brokerage link. Nothing downstream knows the
 * difference, which is why the entry forms here take the same vocabulary Plaid
 * uses rather than a friendlier one of their own: a subtype outside that
 * vocabulary is silently excluded from the return maths instead of rejected.
 *
 * What this deliberately does NOT do is fetch prices. Pulling quotes for
 * tickers the household holds is a different privacy contract from the
 * operator-configured benchmark set, and the README's claim about outbound
 * traffic depends on that line holding. Prices are typed in.
 */

const TX_TYPES: { value: string; label: string }[] = [
  { value: 'cash', label: 'Cash movement' },
  { value: 'buy', label: 'Buy' },
  { value: 'sell', label: 'Sell' },
  { value: 'fee', label: 'Fee' },
  { value: 'transfer', label: 'Transfer' },
]

// Only the subtypes that mean something to the reporting layer, grouped by the
// type they belong under so the form cannot offer a combination that is never
// counted.
const TX_SUBTYPES: Record<string, { value: string; label: string }[]> = {
  cash: [
    { value: 'contribution', label: 'Contribution' },
    { value: 'deposit', label: 'Deposit' },
    { value: 'withdrawal', label: 'Withdrawal' },
    { value: 'distribution', label: 'Distribution' },
    { value: 'dividend', label: 'Dividend' },
    { value: 'interest', label: 'Interest' },
  ],
  buy: [{ value: 'buy', label: 'Buy' }],
  sell: [{ value: 'sell', label: 'Sell' }],
  fee: [
    { value: 'fee', label: 'Fee' },
    { value: 'management fee', label: 'Management fee' },
  ],
  transfer: [
    { value: 'transfer', label: 'Transfer' },
    { value: 'send', label: 'Send' },
    { value: 'request', label: 'Request' },
  ],
}

/** The editor for one manual investment account. Renders nothing for a Plaid
 *  account: there is no honest edit to offer there. */
export function ManualInvestmentEditor({ account }: { account: InvestmentAccount }) {
  const [open, setOpen] = useState(false)

  if (account.source !== 'manual') return null

  return (
    <div className="mt-3">
      <button className="text-xs text-mist-400 underline" onClick={() => setOpen((v) => !v)}>
        {open ? 'Hide' : 'Record positions and activity'}
      </button>
      {open && (
        <div className="mt-3 space-y-4 rounded-lg border border-white/5 bg-black/20 p-4">
          <HoldingForm account={account} />
          <InvestmentTxForm account={account} />
          <RecordedActivity account={account} />
        </div>
      )}
    </div>
  )
}

/** Enters or corrects a position. Re-entering the same security replaces the
 *  quantity rather than adding to it, matching what a re-sync does for a linked
 *  holding — the form asks what you hold, not what you just bought. */
function HoldingForm({ account }: { account: InvestmentAccount }) {
  const qc = useQueryClient()
  const online = useOnline()
  const [securityId, setSecurityId] = useState('')
  const [quantity, setQuantity] = useState('')
  const [price, setPrice] = useState('')
  const [basis, setBasis] = useState('')
  const [error, setError] = useState<string | null>(null)

  const save = useMutation({
    mutationFn: () =>
      api.upsertManualHolding(account.id, {
        security_id: securityId,
        quantity: quantity.trim(),
        institution_price: price.trim() || null,
        cost_basis: basis.trim() || null,
      }),
    onSuccess: () => {
      setQuantity('')
      setPrice('')
      setBasis('')
      qc.invalidateQueries({ queryKey: ['investments'] })
      qc.invalidateQueries({ queryKey: ['holdings'] })
    },
    onError: (err: Error) => setError(err.message),
  })

  return (
    <div>
      <p className="text-xs font-medium text-mist-300">Position</p>
      <div className="mt-2 grid gap-2 sm:grid-cols-4">
        <SecurityPicker value={securityId} onChange={setSecurityId} />
        <label className="block text-xs">
          <span className="text-mist-400">Shares</span>
          <input
            className="input mt-1 w-full"
            value={quantity}
            onChange={(e) => setQuantity(e.target.value)}
            placeholder="12.3456"
            inputMode="decimal"
          />
        </label>
        <label className="block text-xs">
          <span className="text-mist-400">Price each</span>
          <input
            className="input mt-1 w-full"
            value={price}
            onChange={(e) => setPrice(e.target.value)}
            placeholder="optional"
            inputMode="decimal"
          />
        </label>
        <label className="block text-xs">
          <span className="text-mist-400">Cost basis</span>
          <input
            className="input mt-1 w-full"
            value={basis}
            onChange={(e) => setBasis(e.target.value)}
            placeholder="optional"
            inputMode="decimal"
          />
        </label>
      </div>
      <p className="mt-1 text-xs text-mist-500">
        Leave the cost basis blank if you do not know it — blank reads as
        "unknown" and is left out of the gain figures, where a zero would be
        counted as a real basis and overstate every gain.
      </p>
      {error && <p className="mt-1 text-xs text-ember-400">{error}</p>}
      <button
        className="btn-primary mt-2 px-3 py-1.5 text-xs"
        disabled={save.isPending || !securityId || !quantity.trim() || !online}
        title={online ? undefined : OFFLINE_WRITE_HINT}
        onClick={() => {
          setError(null)
          save.mutate()
        }}
      >
        {save.isPending ? 'Saving…' : 'Save position'}
      </button>
    </div>
  )
}

/** Search-and-create over the securities table. Creating is inline because a
 *  household holding something Plaid never reported will not find it in the
 *  list, and sending them elsewhere to add it would break the entry they came
 *  here to make. */
function SecurityPicker({
  value,
  onChange,
}: {
  value: string
  onChange: (id: string) => void
}) {
  const qc = useQueryClient()
  const [search, setSearch] = useState('')
  const [creating, setCreating] = useState(false)

  const securities = useQuery({
    queryKey: ['securities', search],
    queryFn: () => api.listSecurities(search || undefined),
  })

  const create = useMutation({
    mutationFn: (ticker: string) => api.createManualSecurity({ ticker }),
    onSuccess: (sec: Security) => {
      onChange(sec.id)
      setCreating(false)
      qc.invalidateQueries({ queryKey: ['securities'] })
    },
  })

  return (
    <label className="block text-xs">
      <span className="text-mist-400">Security</span>
      <input
        className="input mt-1 w-full"
        value={search}
        onChange={(e) => setSearch(e.target.value)}
        placeholder="Search ticker"
      />
      <select
        className="input mt-1 w-full"
        value={value}
        onChange={(e) => onChange(e.target.value)}
      >
        <option value="">Choose…</option>
        {(securities.data ?? []).map((s) => (
          <option key={s.id} value={s.id}>
            {s.ticker ?? '—'} {s.name ? `· ${s.name}` : ''}
          </option>
        ))}
      </select>
      {search.trim() && (securities.data ?? []).length === 0 && !creating && (
        <button
          className="mt-1 text-xs text-arcane-300 underline"
          onClick={() => {
            setCreating(true)
            create.mutate(search.trim())
          }}
        >
          Add "{search.trim().toUpperCase()}" as a new security
        </button>
      )}
      {create.isError && (
        <p className="mt-1 text-xs text-ember-400">{create.error.message}</p>
      )}
    </label>
  )
}

/**
 * Records one piece of activity: a contribution, a dividend, a buy.
 *
 * The direction picker exists so nobody has to remember the sign convention.
 * Internally, money moving INTO the portfolio is stored negative — that is what
 * Plaid does and what the return calculations expect — and getting it backwards
 * does not raise an error, it inverts every performance figure the account
 * appears in. So the form asks "in or out" and applies the sign itself.
 */
function InvestmentTxForm({ account }: { account: InvestmentAccount }) {
  const qc = useQueryClient()
  const online = useOnline()
  const [type, setType] = useState('cash')
  const [subtype, setSubtype] = useState('contribution')
  const [direction, setDirection] = useState<'in' | 'out'>('in')
  const [amount, setAmount] = useState('')
  const [date, setDate] = useState(new Date().toISOString().slice(0, 10))
  const [error, setError] = useState<string | null>(null)

  const save = useMutation({
    mutationFn: () => {
      const magnitude = amount.trim().replace(/^[+-]/, '')
      return api.createManualInvestmentTransaction({
        account_id: account.id,
        type,
        subtype,
        amount: direction === 'in' ? `-${magnitude}` : magnitude,
        date,
      })
    },
    onSuccess: () => {
      setAmount('')
      qc.invalidateQueries({ queryKey: ['investments'] })
      qc.invalidateQueries({ queryKey: ['account-investment-tx', account.id] })
    },
    onError: (err: Error) => setError(err.message),
  })

  const subtypes = TX_SUBTYPES[type] ?? []

  return (
    <div className="border-t border-white/5 pt-4">
      <p className="text-xs font-medium text-mist-300">Activity</p>
      <div className="mt-2 grid gap-2 sm:grid-cols-5">
        <label className="block text-xs">
          <span className="text-mist-400">Kind</span>
          <select
            className="input mt-1 w-full"
            value={type}
            onChange={(e) => {
              setType(e.target.value)
              setSubtype((TX_SUBTYPES[e.target.value] ?? [])[0]?.value ?? '')
            }}
          >
            {TX_TYPES.map((t) => (
              <option key={t.value} value={t.value}>
                {t.label}
              </option>
            ))}
          </select>
        </label>
        <label className="block text-xs">
          <span className="text-mist-400">Detail</span>
          <select
            className="input mt-1 w-full"
            value={subtype}
            onChange={(e) => setSubtype(e.target.value)}
          >
            {subtypes.map((s) => (
              <option key={s.value} value={s.value}>
                {s.label}
              </option>
            ))}
          </select>
        </label>
        <label className="block text-xs">
          <span className="text-mist-400">Direction</span>
          <select
            className="input mt-1 w-full"
            value={direction}
            onChange={(e) => setDirection(e.target.value as 'in' | 'out')}
          >
            <option value="in">Into the account</option>
            <option value="out">Out of the account</option>
          </select>
        </label>
        <label className="block text-xs">
          <span className="text-mist-400">Amount</span>
          <input
            className="input mt-1 w-full"
            value={amount}
            onChange={(e) => setAmount(e.target.value)}
            placeholder="500.00"
            inputMode="decimal"
          />
        </label>
        <label className="block text-xs">
          <span className="text-mist-400">Date</span>
          <input
            type="date"
            className="input mt-1 w-full"
            value={date}
            onChange={(e) => setDate(e.target.value)}
          />
        </label>
      </div>
      {error && <p className="mt-1 text-xs text-ember-400">{error}</p>}
      <button
        className="btn-primary mt-2 px-3 py-1.5 text-xs"
        disabled={save.isPending || !amount.trim() || !online}
        title={online ? undefined : OFFLINE_WRITE_HINT}
        onClick={() => {
          setError(null)
          save.mutate()
        }}
      >
        {save.isPending ? 'Saving…' : 'Record activity'}
      </button>
    </div>
  )
}

function RecordedActivity({ account }: { account: InvestmentAccount }) {
  const qc = useQueryClient()
  const rows = useQuery({
    queryKey: ['account-investment-tx', account.id],
    queryFn: () => api.accountInvestmentTransactions(account.id),
  })

  const remove = useMutation({
    mutationFn: (id: string) => api.deleteManualInvestmentTransaction(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['account-investment-tx', account.id] })
      qc.invalidateQueries({ queryKey: ['investments'] })
    },
  })

  const entries = rows.data ?? []
  if (entries.length === 0) return null

  return (
    <div className="border-t border-white/5 pt-4">
      <p className="text-xs font-medium text-mist-300">Recorded</p>
      <ul className="mt-2 space-y-1">
        {entries.map((t: InvestmentTransaction) => (
          <li key={t.id} className="flex items-baseline gap-2 text-xs text-mist-500">
            <span className="tabular">{t.date}</span>
            <span>{t.subtype ?? t.type}</span>
            {t.ticker && <span className="text-mist-400">{t.ticker}</span>}
            {/* Displayed with the sign flipped back to how it was entered:
                stored negative means money in, and showing a contribution as
                "-500" would read as a withdrawal to everyone but an accountant. */}
            <span className="tabular text-rune-300">
              {formatMoney(t.amount.startsWith('-') ? t.amount.slice(1) : `-${t.amount}`)}
            </span>
            {t.source === 'scheduled' && (
              <span className="text-mist-600">· auto-posted</span>
            )}
            <button
              className="ml-auto text-mist-600 transition hover:text-ember-400"
              disabled={remove.isPending}
              onClick={() => remove.mutate(t.id)}
            >
              Remove
            </button>
          </li>
        ))}
      </ul>
    </div>
  )
}
