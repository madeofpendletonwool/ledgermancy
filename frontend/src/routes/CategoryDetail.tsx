import type { ReactNode } from 'react'
import { Link, useNavigate, useParams, useSearchParams } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { api } from '../lib/api'
import type { CategoryMerchant } from '../lib/api'
import { formatDate, formatMoney } from '../lib/money'
import {
  WINDOWS,
  defaultRange,
  matchedWindow,
  monthsInRange,
  windowRange,
} from '../lib/period'
import { MonthlyBars } from '../components/charts/MonthlyBars'
import { SINGLE_SERIES, STATUS } from '../components/charts/tokens'
import { MerchantLink } from '../components/MerchantLink'
import { Tile } from '../components/Tile'

/**
 * One category, in detail.
 *
 * The counterpart of the merchant view, and it exists because every category click
 * in the app used to land in a filtered transaction list. That answers "which
 * charges" and nothing else — not how much, not how often, not trending which way,
 * and not who the money actually went to. Those are the questions people click a
 * category to ask.
 *
 * The URL is the source of truth for both the category and the window, so a link
 * to a category over a specific period is shareable and the back button behaves.
 * Addressed by id in the path, which a category can afford where a merchant cannot:
 * a category id is a UUID, whereas a raw merchant descriptor contains slashes.
 */
export function CategoryDetail() {
  const { categoryId = '' } = useParams()
  const [params, setParams] = useSearchParams()
  const navigate = useNavigate()

  const fallback = defaultRange()
  const from = params.get('from') ?? fallback.from
  const to = params.get('to') ?? fallback.to

  const detail = useQuery({
    queryKey: ['category-detail', categoryId, from, to],
    queryFn: () => api.categoryDetail(categoryId, { from, to }),
    enabled: categoryId !== '',
  })

  // Budget-vs-actual needs no dedicated endpoint: there is exactly one household
  // budget per category, and the budgets report already computes the envelope
  // maths (carryover, available, remaining) for all of them.
  const budgets = useQuery({
    queryKey: ['budgets', from, to],
    queryFn: () => api.budgets({ from, to }),
  })
  const budget = (budgets.data ?? []).find((b) => b.category_id === categoryId)

  const setWindow = (months: number) => {
    const next = windowRange(months)
    setParams({ from: next.from, to: next.to })
  }

  if (detail.isPending) {
    return (
      <Shell>
        <p className="text-sm text-mist-400">Loading…</p>
      </Shell>
    )
  }

  if (detail.isError || !detail.data) {
    return (
      <Shell>
        <p className="text-sm text-red-300">
          Couldn’t load that category.{' '}
          <Link className="text-rune-300 underline" to="/spending">
            Back to spending
          </Link>
        </p>
      </Shell>
    )
  }

  const d = detail.data
  const count = d.transaction_count
  const perMonth = Number(d.total) / monthsInRange(from, to)
  const monthsCharged = d.monthly.length

  const openTransactions = () =>
    navigate(`/transactions?category=${d.category_id}&from=${from}&to=${to}`)

  return (
    <Shell>
      <div className="flex flex-wrap items-end justify-between gap-4">
        <div className="min-w-0">
          <Link
            className="text-xs text-mist-400 underline underline-offset-2 hover:text-mist-100"
            to="/spending"
          >
            ← Spending
          </Link>
          <h1 className="mt-1 flex items-center gap-2.5 truncate text-2xl font-semibold">
            <span
              aria-hidden
              className="inline-block size-3 shrink-0 rounded-full"
              style={{ backgroundColor: d.color ?? 'rgba(255,255,255,0.25)' }}
            />
            {d.name}
          </h1>
          <p className="mt-1 text-sm text-mist-300">
            {d.is_fixed ? 'A fixed cost' : 'Discretionary spending'}
            {d.first_seen && d.last_seen && (
              <>
                {' · first charge '}
                {formatDate(d.first_seen)}
                {', last '}
                {formatDate(d.last_seen)}
              </>
            )}
          </p>
        </div>

        <select
          className="field w-44"
          aria-label="Time window"
          value={matchedWindow(from, to)}
          onChange={(e) => setWindow(Number(e.target.value))}
        >
          {WINDOWS.map((w) => (
            <option key={w.months} value={w.months}>
              {w.label}
            </option>
          ))}
        </select>
      </div>

      {count === 0 ? (
        <section className="glass p-6">
          <p className="text-sm text-mist-400">
            Nothing filed under {d.name} between {formatDate(from)} and{' '}
            {formatDate(to)}. Try a longer window.
          </p>
        </section>
      ) : (
        <>
          <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
            <Tile label="Total spent" value={formatMoney(d.total)} />
            <Tile
              label="Charges"
              value={String(count)}
              hint={`${formatMoney(d.average)} average`}
            />
            <Tile
              label="Per month"
              value={formatMoney(String(perMonth))}
              hint="Averaged over the window"
            />
            <Tile label="Largest charge" value={formatMoney(d.largest)} />
          </div>

          {budget && <BudgetProgressPanel budget={budget} />}

          <section className="glass p-6">
            <h2 className="mb-1 text-lg font-medium">Spend per month</h2>
            <p className="mb-5 text-sm text-mist-300">
              {monthsCharged === 1
                ? 'One month with charges in this window.'
                : `${monthsCharged} months with charges in this window.`}
            </p>
            <MonthlyBars
              months={d.monthly}
              from={from}
              to={to}
              label={`Spend per month on ${d.name}`}
            />
          </section>

          {d.merchants.length > 0 && (
            <section className="glass p-6">
              <h2 className="mb-1 text-lg font-medium">Where it goes</h2>
              <p className="mb-5 text-sm text-mist-300">
                {d.merchants.length === 1
                  ? 'One merchant accounts for all of it.'
                  : `The ${d.merchants.length} merchants this lands at, biggest first.`}
              </p>
              <MerchantBars merchants={d.merchants} range={{ from, to }} />
            </section>
          )}

          <section className="glass p-6">
            <div className="mb-5 flex flex-wrap items-center justify-between gap-3">
              <div>
                <h2 className="text-lg font-medium">Charges</h2>
                <p className="mt-1 text-sm text-mist-300">
                  Every charge filed here in this window.
                </p>
              </div>
              <button className="btn-ghost text-sm" onClick={openTransactions}>
                Open in Transactions
              </button>
            </div>

            <div className="overflow-x-auto">
              <table className="w-full text-sm">
                <thead>
                  <tr className="border-b border-white/10 text-left text-xs text-mist-400">
                    <th className="py-2 pr-4 font-medium">Date</th>
                    <th className="py-2 pr-4 font-medium">Merchant</th>
                    <th className="py-2 pr-4 font-medium">Account</th>
                    <th className="py-2 text-right font-medium">Amount</th>
                  </tr>
                </thead>
                <tbody>
                  {d.transactions.map((t) => (
                    <tr key={t.id} className="border-b border-white/5">
                      <td className="whitespace-nowrap py-2 pr-4 text-mist-300">
                        {formatDate(t.date)}
                      </td>
                      <td className="max-w-xs py-2 pr-4">
                        <MerchantLink
                          name={t.merchant}
                          merchantKey={t.merchant_key}
                          range={{ from, to }}
                          className="block max-w-full text-mist-100"
                        />
                        {t.descriptor !== t.merchant && (
                          <span className="block truncate text-xs text-mist-500">
                            {t.descriptor}
                          </span>
                        )}
                      </td>
                      <td className="py-2 pr-4 text-mist-400">{t.account_name}</td>
                      <td className="tabular py-2 text-right text-mist-100">
                        {formatMoney(t.amount)}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>

            {d.transactions.length < count && (
              <p className="mt-4 text-xs text-mist-500">
                Showing the {d.transactions.length} most recent of {count} charges.
                The totals above cover all of them.
              </p>
            )}
          </section>
        </>
      )}
    </Shell>
  )
}

/**
 * The merchants inside a category, as the same bar rows the explorer uses — so a
 * category page and the merchants page read as one system rather than two designs
 * for the same measure.
 */
function MerchantBars({
  merchants,
  range,
}: {
  merchants: CategoryMerchant[]
  range: { from: string; to: string }
}) {
  const max = Math.max(...merchants.map((m) => Number(m.total)), 0)

  return (
    <div className="space-y-2.5">
      {merchants.map((m) => {
        const pct = max > 0 ? (Number(m.total) / max) * 100 : 0
        return (
          <div
            key={m.merchant_key || m.merchant}
            className="grid grid-cols-[1fr_auto] items-center gap-x-3 gap-y-1 py-1 sm:grid-cols-[14rem_1fr_7rem]"
          >
            <MerchantLink
              name={m.merchant}
              merchantKey={m.merchant_key}
              range={range}
              className="min-w-0 text-sm text-mist-100"
            />
            {/* One measure across a categorical dimension is ONE series, so every
                bar carries the same colour — hue would encode nothing. */}
            <span className="order-last col-span-2 h-2.5 overflow-hidden rounded-full bg-white/5 sm:order-none sm:col-span-1">
              <span
                className="block h-full rounded-full"
                style={{
                  width: `${Math.max(pct, 1)}%`,
                  backgroundColor: SINGLE_SERIES,
                }}
              />
            </span>
            <div className="text-right">
              <span className="tabular block text-sm text-mist-200">
                {formatMoney(m.total)}
              </span>
              <span className="block text-[11px] text-mist-500">
                {m.transaction_count} charge{m.transaction_count === 1 ? '' : 's'}
              </span>
            </div>
          </div>
        )
      })}
    </div>
  )
}

/**
 * Budget versus actual, when this category has a budget.
 *
 * Reuses the figures the budgets report already computes rather than recomputing
 * an envelope here — carryover and rollover make "what is left" more than
 * budgeted minus spent, and there should be exactly one answer to that in the app.
 *
 * Note the window mismatch this deliberately surfaces: the budget covers its own
 * period (usually this month) while the page above may be showing a year, so the
 * panel states its period rather than implying it matches.
 */
function BudgetProgressPanel({
  budget,
}: {
  budget: {
    budgeted: string
    spent: string
    remaining: string
    available: string
    carryover: string
    period: string
    period_start: string
    period_end: string
  }
}) {
  const budgeted = Number(budget.available)
  const spent = Number(budget.spent)
  const pct = budgeted > 0 ? Math.min((spent / budgeted) * 100, 100) : 0
  const over = budgeted > 0 && spent > budgeted
  const carryover = Number(budget.carryover)

  return (
    <section className="glass p-6">
      <div className="mb-4 flex flex-wrap items-baseline justify-between gap-2">
        <h2 className="text-lg font-medium">Against your budget</h2>
        <p className="text-xs text-mist-500">
          {budget.period} budget · {formatDate(budget.period_start)} –{' '}
          {formatDate(budget.period_end)}
        </p>
      </div>

      <div className="h-3 overflow-hidden rounded-full bg-white/5">
        <div
          className="h-full rounded-full"
          style={{
            width: `${Math.max(pct, 1)}%`,
            backgroundColor: over ? STATUS.critical : SINGLE_SERIES,
          }}
        />
      </div>

      <p className="mt-3 text-sm text-mist-300">
        <span className="tabular text-mist-100">{formatMoney(budget.spent)}</span> of{' '}
        <span className="tabular text-mist-100">{formatMoney(budget.available)}</span>
        {over ? (
          <>
            {' — '}
            <span style={{ color: STATUS.critical }}>
              {formatMoney(String(spent - budgeted))} over
            </span>
          </>
        ) : (
          <>
            {' — '}
            <span className="tabular">{formatMoney(budget.remaining)}</span> left
          </>
        )}
        {carryover !== 0 && (
          <span className="text-mist-500">
            {' '}
            (includes {formatMoney(budget.carryover)} carried over)
          </span>
        )}
      </p>
    </section>
  )
}

function Shell({ children }: { children: ReactNode }) {
  return <div className="space-y-8">{children}</div>
}
