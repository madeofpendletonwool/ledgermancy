import type { ReactNode } from 'react'
import { Link, useNavigate, useSearchParams } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { api } from '../lib/api'
import { formatDate, formatMoney } from '../lib/money'
import {
  WINDOWS,
  defaultRange,
  matchedWindow,
  monthsInRange,
  windowRange,
} from '../lib/period'
import { CategoryBars } from '../components/charts/CategoryBars'
import { MonthlyBars } from '../components/charts/MonthlyBars'
import { Tile } from '../components/Tile'
import { categoryDetailPath } from '../lib/categories'

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
  // should read as occasional. monthsCharged is a different number, and only
  // describes the chart below.
  const monthsCharged = d.monthly.length
  const perMonth = Number(d.total) / monthsInRange(from, to)

  // Carries the merchant through, so the list opens on this merchant's charges
  // rather than on every charge in the window.
  const openTransactions = () =>
    navigate(
      `/transactions?merchant=${encodeURIComponent(d.merchant_key)}&from=${from}&to=${to}`,
    )

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
              {monthsCharged === 1
                ? 'One month with charges in this window.'
                : `${monthsCharged} months with charges in this window.`}
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
              {/* Into the category's own breakdown rather than a filtered
                  transaction list: "how is this filed" leads naturally to "and
                  what else is filed there", which the list cannot answer. */}
              <CategoryBars
                data={d.categories}
                onSelect={(categoryID) =>
                  navigate(categoryDetailPath(categoryID, { from, to }))
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
                Showing the {d.transactions.length} most recent of {count} charges.
                The totals above cover all of them.
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

function Shell({ children }: { children: ReactNode }) {
  return <div className="space-y-8">{children}</div>
}
