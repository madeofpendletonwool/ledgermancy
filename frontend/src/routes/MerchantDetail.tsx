import type { ReactNode } from 'react'
import { Link, useNavigate, useSearchParams } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { api } from '../lib/api'
import { formatDate, formatMoney } from '../lib/money'
import { CategoryBars } from '../components/charts/CategoryBars'
import { MonthlyBars } from '../components/charts/MonthlyBars'

/** Default window: the trailing twelve months, ending today. */
function defaultRange() {
  const now = new Date()
  const to = new Date(now.getFullYear(), now.getMonth() + 1, 0)
  const from = new Date(now.getFullYear(), now.getMonth() - 11, 1)
  return { from: iso(from), to: iso(to) }
}

function iso(d: Date): string {
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(
    d.getDate(),
  ).padStart(2, '0')}`
}

/** Window options, longest last — a merchant's story is usually a year-shaped one. */
const WINDOWS = [
  { label: 'Last 3 months', months: 3 },
  { label: 'Last 6 months', months: 6 },
  { label: 'Last 12 months', months: 12 },
  { label: 'Last 24 months', months: 24 },
] as const

function windowRange(months: number) {
  const now = new Date()
  const to = new Date(now.getFullYear(), now.getMonth() + 1, 0)
  const from = new Date(now.getFullYear(), now.getMonth() - (months - 1), 1)
  return { from: iso(from), to: iso(to) }
}

/**
 * One merchant, in detail.
 *
 * Reached by drilling in from anywhere a merchant name appears. The URL is the
 * source of truth for both the merchant and the window, matching how the
 * Transactions page works — so a link to a merchant over a specific period is
 * shareable and the back button behaves.
 *
 * The merchant is addressed by its RESOLVED key rather than an entity id, which
 * is what lets this page work for merchants nobody has grouped. That is most of
 * them, and most of the spending.
 */
export function MerchantDetail() {
  const [params, setParams] = useSearchParams()
  const navigate = useNavigate()

  const key = params.get('key') ?? ''
  const fallback = defaultRange()
  const from = params.get('from') ?? fallback.from
  const to = params.get('to') ?? fallback.to

  const detail = useQuery({
    queryKey: ['merchant-detail', key, from, to],
    queryFn: () => api.merchantDetail(key, { from, to }),
    enabled: key !== '',
  })

  const setWindow = (months: number) => {
    const next = windowRange(months)
    setParams({ key, from: next.from, to: next.to })
  }

  if (key === '') {
    return (
      <Shell>
        <p className="text-sm text-mist-400">
          No merchant selected.{' '}
          <Link className="text-rune-300 underline" to="/merchants">
            Back to merchants
          </Link>
        </p>
      </Shell>
    )
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
          Couldn’t load that merchant.{' '}
          <Link className="text-rune-300 underline" to="/merchants">
            Back to merchants
          </Link>
        </p>
      </Shell>
    )
  }

  const d = detail.data
  const count = d.transaction_count
  // The window is what the user picked, so "per month" is over the window rather
  // than over the months that happen to have a charge — an occasional merchant
  // should read as occasional.
  const monthsInWindow = Math.max(d.monthly.length, 1)
  const perMonth = Number(d.total) / monthsInRange(from, to)

  const openTransactions = () =>
    navigate(`/transactions?from=${from}&to=${to}`)

  return (
    <Shell>
      <div className="flex flex-wrap items-end justify-between gap-4">
        <div className="min-w-0">
          <Link
            className="text-xs text-mist-400 underline underline-offset-2 hover:text-mist-100"
            to="/merchants"
          >
            ← Merchants
          </Link>
          <h1 className="mt-1 truncate text-2xl font-semibold">{d.merchant}</h1>
          <p className="mt-1 text-sm text-mist-300">
            {d.is_grouped
              ? `${d.descriptors.length} descriptor${
                  d.descriptors.length === 1 ? '' : 's'
                } grouped as one merchant`
              : 'A single descriptor, not grouped with anything'}
            {d.first_seen && d.last_seen && (
              <>
                {' · first seen '}
                {formatDate(d.first_seen)}
                {', last '}
                {formatDate(d.last_seen)}
              </>
            )}
          </p>
        </div>

        {/* Filters in one row above the charts. */}
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
            No charges at this merchant between {formatDate(from)} and{' '}
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

          <section className="glass p-6">
            <h2 className="mb-1 text-lg font-medium">Spend per month</h2>
            <p className="mb-5 text-sm text-mist-300">
              {monthsInWindow === 1
                ? 'One month with charges in this window.'
                : `${monthsInWindow} months with charges in this window.`}
            </p>
            <MonthlyBars months={d.monthly} from={from} to={to} />
          </section>

          {d.categories.length > 0 && (
            <section className="glass p-6">
              <h2 className="mb-1 text-lg font-medium">How it’s filed</h2>
              <p className="mb-5 text-sm text-mist-300">
                {d.categories.length === 1
                  ? 'Everything here lands in one category.'
                  : `Split across ${d.categories.length} categories.`}
              </p>
              <CategoryBars
                data={d.categories}
                onSelect={(categoryID) =>
                  navigate(
                    `/transactions?category=${categoryID}&from=${from}&to=${to}`,
                  )
                }
              />
            </section>
          )}

          <section className="glass p-6">
            <div className="mb-5 flex flex-wrap items-center justify-between gap-3">
              <div>
                <h2 className="text-lg font-medium">Charges</h2>
                <p className="mt-1 text-sm text-mist-300">
                  {d.is_grouped
                    ? 'The descriptor column shows which fragment each charge arrived under.'
                    : 'Every charge in this window.'}
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
                    <th className="py-2 pr-4 font-medium">Descriptor</th>
                    <th className="py-2 pr-4 font-medium">Account</th>
                    <th className="py-2 pr-4 font-medium">Category</th>
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
                        <span className="block truncate text-mist-100">
                          {t.descriptor}
                        </span>
                        {d.is_grouped && t.raw_merchant_key && (
                          <span className="block truncate font-mono text-xs text-mist-500">
                            {t.raw_merchant_key}
                          </span>
                        )}
                      </td>
                      <td className="py-2 pr-4 text-mist-400">
                        {t.account_name}
                      </td>
                      <td className="py-2 pr-4 text-mist-400">
                        {t.category_name ?? '—'}
                      </td>
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
                Showing the {d.transactions.length} most recent of {count}
                charges. The totals above cover all of them.
              </p>
            )}
          </section>

          {d.is_grouped && (
            <section className="glass p-6">
              <h2 className="mb-1 text-lg font-medium">Descriptors</h2>
              <p className="mb-4 text-sm text-mist-300">
                These read as one merchant everywhere in the app.{' '}
                <Link className="text-rune-300 underline" to="/merchants">
                  Manage the grouping
                </Link>
                .
              </p>
              <ul className="space-y-1">
                {d.descriptors.map((key) => (
                  <li
                    key={key}
                    className="truncate rounded-lg border border-white/10 px-3 py-2 font-mono text-xs text-mist-400"
                  >
                    {key}
                  </li>
                ))}
              </ul>
            </section>
          )}
        </>
      )}
    </Shell>
  )
}

/**
 * Months spanned by the range, floored at 1. Used for the per-month average, so
 * an occasional merchant is not overstated by dividing only by the months it
 * happened to appear in — the same reasoning as GetCategoryAverages.
 */
function monthsInRange(from: string, to: string): number {
  const [fy, fm] = from.split('-').map(Number)
  const [ty, tm] = to.split('-').map(Number)
  if (!fy || !fm || !ty || !tm) return 1
  return Math.max(1, (ty - fy) * 12 + (tm - fm) + 1)
}

/** Which preset window the current range corresponds to, for the select. */
function matchedWindow(from: string, to: string): number {
  const span = monthsInRange(from, to)
  const match = WINDOWS.find((w) => w.months === span)
  return match ? match.months : 12
}

function Shell({ children }: { children: ReactNode }) {
  return <div className="space-y-8">{children}</div>
}

function Tile({
  label,
  value,
  hint,
}: {
  label: string
  value: string
  hint?: string
}) {
  return (
    <div className="glass p-5">
      <p className="text-sm text-mist-300">{label}</p>
      <p
        className="tabular mt-2 text-3xl font-semibold"
        style={{ color: '#f2d492' }}
      >
        {value}
      </p>
      {hint && <p className="mt-1 text-xs text-mist-500">{hint}</p>}
    </div>
  )
}
