import { useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '../lib/api'
import type {
  BalanceProjection,
  Obligation,
  ObligationInput,
  ObligationUnit,
  UpcomingObligation,
} from '../lib/api'
import { formatDate, formatMoney } from '../lib/money'
import { MerchantLink } from '../components/MerchantLink'
import { Monogram } from '../components/Monogram'
import { ProjectionChart } from '../components/charts/ProjectionChart'
import { SkeletonRows, SkeletonChart, Reveal } from '../components/Skeleton'
import { EmptyState } from '../components/EmptyState'
import { STATUS } from '../components/charts/tokens'

const HORIZONS = [30, 60, 90] as const

export function Schedule() {
  const [days, setDays] = useState<number>(30)

  const upcoming = useQuery({
    queryKey: ['obligations-upcoming', days],
    queryFn: () => api.upcomingObligations(days),
  })
  const projection = useQuery({
    queryKey: ['obligations-projection', days],
    queryFn: () => api.obligationProjection(days),
  })
  const obligations = useQuery({
    queryKey: ['obligations'],
    queryFn: api.obligations,
  })

  const items = upcoming.data?.items ?? []

  return (
    <div className="space-y-8">
      <div className="flex flex-wrap items-end justify-between gap-4">
        <div>
          <h1 className="text-2xl font-semibold">Schedule</h1>
          <p className="mt-1 text-mist-300">
            What's due, when, and what it leaves in the bank.
          </p>
        </div>

        <div>
          <label className="label" htmlFor="horizon">
            Looking ahead
          </label>
          <select
            id="horizon"
            className="field"
            value={days}
            onChange={(e) => setDays(Number(e.target.value))}
          >
            {HORIZONS.map((d) => (
              <option key={d} value={d}>
                Next {d} days
              </option>
            ))}
          </select>
        </div>
      </div>

      <section className="glass p-6">
        <div className="flex flex-wrap items-baseline justify-between gap-3">
          <h2 className="text-lg font-medium">Due in the next {days} days</h2>
          <p className="tabular text-xl font-semibold">
            {formatMoney(upcoming.data?.total)}
          </p>
        </div>
        <p className="mt-1 mb-5 text-sm text-mist-300">
          Every bill the app knows about — detected recurring charges plus
          anything entered by hand.
        </p>

        {upcoming.isPending ? (
          <SkeletonRows count={4} />
        ) : items.length === 0 ? (
          <EmptyState title={`Nothing due in the next ${days} days`}>
            Recurring charges are picked up automatically. Anything the bank
            can't show can be added under “Your bills”.
          </EmptyState>
        ) : (
          <Reveal>
            <BillCalendar items={items} />
            <UpcomingList items={items} />
          </Reveal>
        )}
      </section>

      <section className="glass p-6">
        <h2 className="text-lg font-medium">Projected balance</h2>
        <p className="mt-1 mb-5 text-sm text-mist-300">
          Today's cash balance carried forward through the bills above.{' '}
          <span className="text-mist-400">
            Known obligations only — it does not try to predict day-to-day
            spending.
          </span>
        </p>
        {projection.isPending ? (
          <SkeletonChart />
        ) : (
          <ProjectionPanel projection={projection.data} />
        )}
      </section>

      <ObligationManager
        obligations={obligations.data ?? []}
        isPending={obligations.isPending}
      />
    </div>
  )
}

// --- Calendar ---------------------------------------------------------------

/**
 * A month grid per month the horizon touches, with each bill on its due day.
 * Dates are built from their calendar parts rather than through Date's ISO
 * parsing, which renders the previous day in any timezone west of UTC — that
 * would put a bill on the wrong square and, at a month boundary, the wrong grid.
 */
function BillCalendar({ items }: { items: UpcomingObligation[] }) {
  const months = useMemo(() => groupIntoMonths(items), [items])

  return (
    <div className="space-y-6">
      {months.map((m) => (
        <div key={m.key}>
          <h3 className="mb-2 text-sm font-medium text-mist-200">{m.label}</h3>
          <div className="grid grid-cols-7 gap-1 text-center text-[11px] text-mist-500">
            {['Su', 'Mo', 'Tu', 'We', 'Th', 'Fr', 'Sa'].map((d) => (
              <div key={d} className="pb-1">
                {d}
              </div>
            ))}
          </div>
          <div className="grid grid-cols-7 gap-1">
            {m.cells.map((cell, i) =>
              cell === null ? (
                <div key={`pad-${i}`} />
              ) : (
                <CalendarDay key={cell.iso} day={cell.day} bills={cell.bills} />
              ),
            )}
          </div>
        </div>
      ))}
    </div>
  )
}

function CalendarDay({
  day,
  bills,
}: {
  day: number
  bills: UpcomingObligation[]
}) {
  const total = bills.length

  return (
    <div
      className={`min-h-[68px] rounded-lg border p-1.5 text-left ${
        total > 0
          ? 'border-white/10 bg-white/5'
          : 'border-white/5 bg-transparent'
      }`}
    >
      <div className="text-[11px] text-mist-500">{day}</div>
      {bills.slice(0, 2).map((b) => (
        <div
          key={`${b.obligation_id}-${b.due_date}`}
          className="mt-1 truncate rounded px-1 py-0.5 text-[11px] text-mist-200"
          style={{ backgroundColor: 'rgba(144,133,233,0.18)' }}
          title={`${b.label} — ${formatMoney(b.amount)}`}
        >
          {b.label}
        </div>
      ))}
      {total > 2 && (
        <div className="mt-1 px-1 text-[11px] text-mist-500">
          +{total - 2} more
        </div>
      )}
    </div>
  )
}

type MonthGrid = {
  key: string
  label: string
  cells: ({ iso: string; day: number; bills: UpcomingObligation[] } | null)[]
}

function groupIntoMonths(items: UpcomingObligation[]): MonthGrid[] {
  const byDate = new Map<string, UpcomingObligation[]>()
  for (const item of items) {
    const key = item.due_date.slice(0, 10)
    byDate.set(key, [...(byDate.get(key) ?? []), item])
  }
  if (byDate.size === 0) return []

  const dates = [...byDate.keys()].sort()
  const first = parts(dates[0])
  const last = parts(dates[dates.length - 1])

  const grids: MonthGrid[] = []
  for (
    let cursor = new Date(first.year, first.month - 1, 1);
    cursor <= new Date(last.year, last.month - 1, 1);
    cursor = new Date(cursor.getFullYear(), cursor.getMonth() + 1, 1)
  ) {
    const year = cursor.getFullYear()
    const month = cursor.getMonth()
    const daysInMonth = new Date(year, month + 1, 0).getDate()
    const leading = new Date(year, month, 1).getDay()

    const cells: MonthGrid['cells'] = Array.from({ length: leading }, () => null)
    for (let day = 1; day <= daysInMonth; day++) {
      const iso = `${year}-${String(month + 1).padStart(2, '0')}-${String(day).padStart(2, '0')}`
      cells.push({ iso, day, bills: byDate.get(iso) ?? [] })
    }

    grids.push({
      key: `${year}-${month}`,
      label: cursor.toLocaleDateString('en-US', { month: 'long', year: 'numeric' }),
      cells,
    })
  }
  return grids
}

function parts(iso: string) {
  const [year, month, day] = iso.slice(0, 10).split('-').map(Number)
  return { year, month, day }
}

function UpcomingList({ items }: { items: UpcomingObligation[] }) {
  return (
    <div className="mt-6 overflow-x-auto">
      <table className="w-full text-sm">
        <thead className="text-left text-xs uppercase tracking-wide text-mist-500">
          <tr>
            <th className="pb-2">Bill</th>
            <th className="pb-2">Due</th>
            <th className="pb-2">Cadence</th>
            <th className="pb-2 text-right">Amount</th>
          </tr>
        </thead>
        <tbody>
          {items.map((b) => (
            <tr
              key={`${b.obligation_id}-${b.due_date}`}
              className="border-t border-white/5"
            >
              <td className="py-2 pr-3">
                {b.label}
                {b.source === 'detected' && (
                  <span className="ml-2 text-xs text-mist-500">detected</span>
                )}
              </td>
              <td className="py-2 pr-3 text-mist-300">
                {formatDate(b.due_date)}
                <span className="ml-2 text-xs text-mist-500">
                  {dueIn(b.days_until_due)}
                </span>
              </td>
              <td className="py-2 pr-3 text-mist-400">{b.cadence}</td>
              <td className="tabular py-2 text-right">{formatMoney(b.amount)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

function dueIn(days: number): string {
  if (days === 0) return 'today'
  if (days === 1) return 'tomorrow'
  return `in ${days} days`
}

// --- Projection -------------------------------------------------------------

function ProjectionPanel({
  projection,
}: {
  projection: BalanceProjection | undefined
}) {
  const [selected, setSelected] = useState<string>('combined')

  if (!projection)
    return <EmptyState title="Couldn't load a projection." />
  if (projection.accounts.length === 0) {
    return (
      <EmptyState
        title="No cash accounts to project"
        action={
          <Link to="/accounts" className="btn-ghost">
            Link an account
          </Link>
        }
      >
        Only checking and savings balances are projected — running this over a
        credit card would subtract the card's own bills from the balance they
        make up.
      </EmptyState>
    )
  }

  const series =
    selected === 'combined'
      ? projection.combined
      : (projection.accounts.find((a) => a.account_id === selected) ??
        projection.combined)

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-end justify-between gap-4">
        <div>
          <label className="label" htmlFor="projection-account">
            Account
          </label>
          <select
            id="projection-account"
            className="field"
            value={selected}
            onChange={(e) => setSelected(e.target.value)}
          >
            <option value="combined">All cash accounts</option>
            {projection.accounts.map((a) => (
              <option key={a.account_id} value={a.account_id ?? ''}>
                {a.name}
                {a.mask ? ` ••${a.mask}` : ''}
              </option>
            ))}
          </select>
        </div>

        <div className="text-right">
          <p className="text-xs uppercase tracking-wide text-mist-500">
            Lowest point
          </p>
          <p
            className="tabular text-xl font-semibold"
            style={{ color: series.goes_negative ? STATUS.critical : undefined }}
          >
            {formatMoney(series.lowest_balance)}
          </p>
          <p className="text-xs text-mist-500">on {formatDate(series.lowest_date)}</p>
        </div>
      </div>

      {series.goes_negative && (
        <p
          role="alert"
          className="rounded-xl px-4 py-3 text-sm"
          style={{
            color: STATUS.critical,
            backgroundColor: 'rgba(208,59,59,0.12)',
          }}
        >
          {series.name} is projected to run out on {formatDate(series.lowest_date)} —
          and that is before any day-to-day spending.
        </p>
      )}

      <ProjectionChart series={series} estimate={projection.estimate} />

      {projection.estimate.has_income_history ? (
        <p className="text-xs text-mist-500">
          The dashed line adds your typical income and spending, estimated from
          the trailing {projection.estimate.income_months} month
          {projection.estimate.income_months === 1 ? '' : 's'} — an estimate,
          not a guarantee. The solid line above stays known-bills-only.
        </p>
      ) : (
        <p className="text-xs text-mist-500">
          Not enough income history yet to add an estimated line — once a few
          months of income show up, this chart will add one.
        </p>
      )}

      {/* Per-account lines can only show bills someone assigned to an account.
          Saying so is better than quietly understating the drop. */}
      {selected !== 'combined' && Number(projection.unassigned_total) > 0 && (
        <p className="text-sm text-mist-400">
          {formatMoney(projection.unassigned_total)} of bills in this window name
          no account, so they are not in this line. They are included in “All cash
          accounts”.
        </p>
      )}
    </div>
  )
}

// --- Management -------------------------------------------------------------

function ObligationManager({
  obligations,
  isPending,
}: {
  obligations: Obligation[]
  isPending: boolean
}) {
  const [editing, setEditing] = useState<Obligation | null>(null)

  const active = obligations.filter((o) => o.is_active)
  const inactive = obligations.filter((o) => !o.is_active)

  return (
    <>
      <section className="glass p-6">
        <h2 className="mb-1 text-lg font-medium">Your bills</h2>
        <p className="mb-5 text-sm text-mist-300">
          Recurring charges are detected from your transactions, weekly through
          yearly. Anything the bank can't show — dues paid by cheque, a bill
          split with someone else — has to be added by hand.
        </p>

        {isPending ? (
          <SkeletonRows count={3} />
        ) : active.length === 0 ? (
          <EmptyState
            title="No bills tracked yet"
            icon={<BillGlyph />}
            action={
              <a href="#add-bill" className="btn-primary">
                Add a bill
              </a>
            }
          >
            Recurring charges are detected from your transactions; add anything
            else by hand here.
          </EmptyState>
        ) : (
          <Reveal>
            <div className="space-y-3">
              {active.map((o) => (
                <ObligationRow key={o.id} obligation={o} onEdit={setEditing} />
              ))}
            </div>
          </Reveal>
        )}

        {inactive.length > 0 && (
          <details className="mt-6">
            <summary className="cursor-pointer text-sm text-mist-400">
              {inactive.length} stopped {inactive.length === 1 ? 'bill' : 'bills'}
            </summary>
            <div className="mt-3 space-y-3">
              {inactive.map((o) => (
                <ObligationRow key={o.id} obligation={o} onEdit={setEditing} />
              ))}
            </div>
          </details>
        )}
      </section>

      <ObligationForm editing={editing} onDone={() => setEditing(null)} />
    </>
  )
}

function ObligationRow({
  obligation,
  onEdit,
}: {
  obligation: Obligation
  onEdit: (o: Obligation) => void
}) {
  const qc = useQueryClient()
  const remove = useMutation({
    mutationFn: () => api.deleteObligation(obligation.id),
    onSuccess: () => invalidateSchedule(qc),
  })

  return (
    <div className="rounded-xl border border-white/5 p-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex min-w-0 items-start gap-3">
          <Monogram name={obligation.label} className="mt-0.5" />
          <div>
            <p className="font-medium">
              {/* A detected bill IS a merchant, so its label opens that merchant.
                  The stored key is already the resolved one — the detector hands out
                  resolved keys, and a later merge re-promotes the row under the new
                  key while retiring the old. A hand-entered bill has no merchant and
                  stays plain text. */}
              <MerchantLink
                name={obligation.label}
                merchantKey={
                  obligation.source === 'detected' ? obligation.merchant_key : null
                }
              />
            {obligation.is_personal && (
              <span className="ml-2 text-xs font-normal text-mist-500">personal</span>
            )}
            {obligation.source === 'detected' && (
              <span className="ml-2 text-xs font-normal text-mist-500">detected</span>
            )}
          </p>
          <p className="mt-0.5 text-sm text-mist-400">
            {formatMoney(obligation.amount)} · {obligation.cadence}
            {obligation.next_due
              ? ` · next ${formatDate(obligation.next_due)}`
              : ' · no upcoming date'}
          </p>
          <p className="mt-0.5 text-xs text-mist-500">
            about {formatMoney(obligation.monthly_estimate)} a month
            {obligation.source === 'detected' && obligation.user_edited &&
              ' · your edits are kept when charges are re-detected'}
          </p>
          </div>
        </div>
        <div className="flex items-center gap-2">
          <button
            className="btn-ghost px-2.5 py-1 text-xs"
            onClick={() => onEdit(obligation)}
          >
            Edit
          </button>
          <button
            className="btn-ghost px-2.5 py-1 text-xs text-ember-400"
            disabled={remove.isPending}
            onClick={() => remove.mutate()}
          >
            {obligation.source === 'detected' ? 'Stop tracking' : 'Delete'}
          </button>
        </div>
      </div>
      {remove.isError && (
        <p role="alert" className="mt-2 text-sm text-ember-400">
          {remove.error.message}
        </p>
      )}
    </div>
  )
}

const UNITS: { value: ObligationUnit; label: string }[] = [
  { value: 'day', label: 'days' },
  { value: 'week', label: 'weeks' },
  { value: 'month', label: 'months' },
  { value: 'year', label: 'years' },
]

/**
 * Manual entry, and the edit form for a detected bill whose cadence came out
 * wrong. This is the only path for the bills Plaid cannot see, so it is a
 * first-class section rather than something buried behind a menu.
 */
function ObligationForm({
  editing,
  onDone,
}: {
  editing: Obligation | null
  onDone: () => void
}) {
  const qc = useQueryClient()
  const accounts = useQuery({ queryKey: ['accounts'], queryFn: api.accounts })
  const categories = useQuery({ queryKey: ['categories'], queryFn: api.categories })

  // Keyed on the row being edited so switching rows re-seeds every field.
  const [form, setForm] = useState(() => blankForm())
  const [formKey, setFormKey] = useState<string>('new')
  const targetKey = editing?.id ?? 'new'
  if (targetKey !== formKey) {
    setFormKey(targetKey)
    setForm(editing ? formFrom(editing) : blankForm())
  }

  const save = useMutation({
    mutationFn: (input: ObligationInput) =>
      editing ? api.updateObligation(editing.id, input) : api.createObligation(input),
    onSuccess: () => {
      invalidateSchedule(qc)
      setForm(blankForm())
      setFormKey('new')
      onDone()
    },
  })

  const set = <K extends keyof typeof form>(key: K, value: (typeof form)[K]) =>
    setForm((f) => ({ ...f, [key]: value }))

  const canSave =
    form.label.trim() !== '' &&
    form.amount !== '' &&
    Number(form.amount) > 0 &&
    form.anchorDate !== '' &&
    Number(form.intervalCount) > 0

  const submit = () => {
    if (!canSave) return
    save.mutate({
      label: form.label.trim(),
      amount: form.amount,
      category_id: form.categoryID || null,
      account_id: form.accountID || null,
      interval_count: Number(form.intervalCount),
      interval_unit: form.intervalUnit,
      anchor_date: form.anchorDate,
      end_date: form.endDate || undefined,
      personal: form.personal,
      is_active: form.isActive,
    })
  }

  return (
    <section id="add-bill" className="glass scroll-mt-24 p-6">
      <h2 className="mb-1 text-lg font-medium">
        {editing ? `Edit ${editing.label}` : 'Add a bill'}
      </h2>
      <p className="mb-5 text-sm text-mist-300">
        The first due date anchors the whole schedule — every later date is
        counted from it, so a monthly bill anchored on the 31st lands on the last
        day of shorter months.
      </p>

      <div className="grid gap-4 sm:grid-cols-2">
        <div>
          <label className="label" htmlFor="ob-label">
            What is it
          </label>
          <input
            id="ob-label"
            className="field w-full"
            placeholder="Car insurance"
            value={form.label}
            onChange={(e) => set('label', e.target.value)}
          />
        </div>

        <div>
          <label className="label" htmlFor="ob-amount">
            Amount
          </label>
          <input
            id="ob-amount"
            className="field w-full"
            type="number"
            min="0"
            step="0.01"
            placeholder="1200.00"
            value={form.amount}
            onChange={(e) => set('amount', e.target.value)}
          />
        </div>

        <div>
          <label className="label" htmlFor="ob-anchor">
            First (or most recent) due date
          </label>
          <input
            id="ob-anchor"
            className="field w-full"
            type="date"
            value={form.anchorDate}
            onChange={(e) => set('anchorDate', e.target.value)}
          />
        </div>

        <div>
          <label className="label" htmlFor="ob-count">
            Repeats every
          </label>
          <div className="flex gap-2">
            <input
              id="ob-count"
              className="field w-20"
              type="number"
              min="1"
              step="1"
              value={form.intervalCount}
              onChange={(e) => set('intervalCount', e.target.value)}
            />
            <select
              className="field flex-1"
              aria-label="Cadence unit"
              value={form.intervalUnit}
              onChange={(e) => set('intervalUnit', e.target.value as ObligationUnit)}
            >
              {UNITS.map((u) => (
                <option key={u.value} value={u.value}>
                  {u.label}
                </option>
              ))}
            </select>
          </div>
        </div>

        <div>
          <label className="label" htmlFor="ob-category">
            Category (optional)
          </label>
          <select
            id="ob-category"
            className="field w-full"
            value={form.categoryID}
            onChange={(e) => set('categoryID', e.target.value)}
          >
            <option value="">None</option>
            {(categories.data ?? [])
              .filter((c) => !c.is_income && !c.is_transfer)
              .map((c) => (
                <option key={c.id} value={c.id}>
                  {c.name}
                </option>
              ))}
          </select>
          <p className="mt-1 text-xs text-mist-500">
            Picking one keeps “safe to spend” from counting this bill twice.
          </p>
        </div>

        <div>
          <label className="label" htmlFor="ob-account">
            Paid from (optional)
          </label>
          <select
            id="ob-account"
            className="field w-full"
            value={form.accountID}
            onChange={(e) => set('accountID', e.target.value)}
          >
            <option value="">Not specified</option>
            {(accounts.data ?? []).map((a) => (
              <option key={a.id} value={a.id}>
                {a.name}
                {a.mask ? ` ••${a.mask}` : ''}
              </option>
            ))}
          </select>
        </div>

        <div>
          <label className="label" htmlFor="ob-end">
            Stops after (optional)
          </label>
          <input
            id="ob-end"
            className="field w-full"
            type="date"
            value={form.endDate}
            onChange={(e) => set('endDate', e.target.value)}
          />
        </div>

        <div className="flex flex-col justify-end gap-2 pb-2">
          <label className="flex items-center gap-2 text-sm text-mist-300">
            <input
              type="checkbox"
              className="h-4 w-4 accent-arcane-500"
              checked={form.personal}
              onChange={(e) => set('personal', e.target.checked)}
            />
            Just for me (keep it out of the household view)
          </label>
          {editing && (
            <label className="flex items-center gap-2 text-sm text-mist-300">
              <input
                type="checkbox"
                className="h-4 w-4 accent-arcane-500"
                checked={form.isActive}
                onChange={(e) => set('isActive', e.target.checked)}
              />
              Still active
            </label>
          )}
        </div>
      </div>

      <div className="mt-5 flex flex-wrap items-center gap-3">
        <button
          className="btn-primary px-4 py-2 text-sm"
          disabled={!canSave || save.isPending}
          onClick={submit}
        >
          {save.isPending ? 'Saving…' : editing ? 'Save changes' : 'Add bill'}
        </button>
        {editing && (
          <button
            className="btn-ghost px-3 py-2 text-sm text-mist-300"
            onClick={() => {
              setForm(blankForm())
              setFormKey('new')
              onDone()
            }}
          >
            Cancel
          </button>
        )}
        {save.isError && (
          <span role="alert" className="text-sm text-ember-400">
            {save.error.message}
          </span>
        )}
      </div>
    </section>
  )
}

function blankForm() {
  return {
    label: '',
    amount: '',
    categoryID: '',
    accountID: '',
    intervalCount: '1',
    intervalUnit: 'month' as ObligationUnit,
    anchorDate: '',
    endDate: '',
    personal: false,
    isActive: true,
  }
}

function formFrom(o: Obligation) {
  return {
    label: o.label,
    amount: o.amount,
    categoryID: o.category_id ?? '',
    accountID: o.account_id ?? '',
    intervalCount: String(o.interval_count),
    intervalUnit: o.interval_unit,
    anchorDate: o.anchor_date.slice(0, 10),
    endDate: o.end_date?.slice(0, 10) ?? '',
    personal: o.is_personal,
    isActive: o.is_active,
  }
}

// Every schedule view is derived from the same rows, so a write invalidates all
// of them — including safe-to-spend, whose fixed-cost split reads obligations.
function invalidateSchedule(qc: ReturnType<typeof useQueryClient>) {
  qc.invalidateQueries({ queryKey: ['obligations'] })
  qc.invalidateQueries({ queryKey: ['obligations-upcoming'] })
  qc.invalidateQueries({ queryKey: ['obligations-projection'] })
  qc.invalidateQueries({ queryKey: ['safe-to-spend'] })
}

/** Outline glyph for the schedule empty state. */
function BillGlyph() {
  return (
    <svg
      className="h-5 w-5"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={2}
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <path d="M5 4h14v16l-3-2-2 2-2-2-2 2-3-2-2 2zM9 9h6M9 13h6" />
    </svg>
  )
}
