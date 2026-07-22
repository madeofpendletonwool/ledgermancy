import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { api } from '../lib/api'
import { formatMoney } from '../lib/money'
import { CategoryBars } from '../components/charts/CategoryBars'
import { TrendChart } from '../components/charts/TrendChart'
import { CHART, STATUS } from '../components/charts/tokens'

/** Month options: the current month plus the previous eleven. */
function recentMonths(count = 12) {
  const now = new Date()
  return Array.from({ length: count }, (_, i) => {
    const d = new Date(now.getFullYear(), now.getMonth() - i, 1)
    const value = `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}`
    return {
      value,
      label: d.toLocaleDateString('en-US', { month: 'long', year: 'numeric' }),
      from: `${value}-01`,
      to: new Date(d.getFullYear(), d.getMonth() + 1, 0).toISOString().slice(0, 10),
    }
  })
}

export function Spending() {
  const months = recentMonths()
  const [monthValue, setMonthValue] = useState(months[0].value)
  const month = months.find((m) => m.value === monthValue) ?? months[0]
  const range = { from: month.from, to: month.to }

  const summary = useQuery({
    queryKey: ['summary', range.from, range.to],
    queryFn: () => api.summary(range),
  })
  const byCategory = useQuery({
    queryKey: ['by-category', range.from, range.to],
    queryFn: () => api.byCategory(range),
  })
  const trend = useQuery({ queryKey: ['trend'], queryFn: () => api.trend() })
  const averages = useQuery({ queryKey: ['averages'], queryFn: () => api.averages() })

  const s = summary.data

  return (
    <div className="space-y-8">
      <div className="flex flex-wrap items-end justify-between gap-4">
        <div>
          <h1 className="text-2xl font-semibold">Spending</h1>
          <p className="mt-1 text-mist-300">
            Where the money went, and what was left over.
          </p>
        </div>

        {/* Filters sit in one row above the charts. */}
        <div>
          <label className="label" htmlFor="month">
            Month
          </label>
          <select
            id="month"
            className="field"
            value={monthValue}
            onChange={(e) => setMonthValue(e.target.value)}
          >
            {months.map((m) => (
              <option key={m.value} value={m.value}>
                {m.label}
              </option>
            ))}
          </select>
        </div>
      </div>

      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <Tile label="Income" value={s ? formatMoney(s.income) : '—'} />
        <Tile label="Spending" value={s ? formatMoney(s.spending) : '—'} />
        <Tile
          label="Left to invest"
          value={s ? formatMoney(s.leftover) : '—'}
          tone={s && Number(s.leftover) < 0 ? 'critical' : 'good'}
        />
        <Tile
          label="Savings rate"
          value={
            s?.savings_rate != null
              ? `${(Number(s.savings_rate) * 100).toFixed(1)}%`
              : '—'
          }
          hint={
            s?.savings_rate == null ? 'No income recorded this period' : undefined
          }
        />
      </div>

      {s && Number(s.spending) > 0 && (
        <div className="grid gap-4 sm:grid-cols-2">
          <SplitTile
            label="Fixed"
            value={formatMoney(s.fixed_spending)}
            share={Number(s.fixed_spending) / Number(s.spending)}
            hint="Rent, utilities, loan payments"
          />
          <SplitTile
            label="Discretionary"
            value={formatMoney(s.discretionary_spending)}
            share={Number(s.discretionary_spending) / Number(s.spending)}
            hint="Everything you can flex"
          />
        </div>
      )}

      <section className="glass p-6">
        <h2 className="mb-1 text-lg font-medium">By category</h2>
        <p className="mb-5 text-sm text-mist-300">{month.label}</p>
        {byCategory.isPending ? (
          <Loading />
        ) : (
          <CategoryBars data={byCategory.data ?? []} />
        )}
      </section>

      <section className="glass p-6">
        <h2 className="mb-1 text-lg font-medium">Income vs spending</h2>
        <p className="mb-5 text-sm text-mist-300">Trailing twelve months</p>
        {trend.isPending ? <Loading /> : <TrendChart data={trend.data ?? []} />}
      </section>

      <section className="glass overflow-hidden">
        <div className="px-6 pt-6">
          <h2 className="text-lg font-medium">Typical month</h2>
          <p className="mt-1 mb-5 text-sm text-mist-300">
            Average monthly spend and annual total per category, over the last
            year — the figures that matter for planning.
          </p>
        </div>

        {/* The table view. Every chart above is also readable as numbers. */}
        <div className="overflow-x-auto">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-y border-white/5 text-left text-xs text-mist-500">
                <th className="px-6 py-2.5 font-medium">Category</th>
                <th className="px-6 py-2.5 text-right font-medium">Avg / month</th>
                <th className="px-6 py-2.5 text-right font-medium">Total / year</th>
                <th className="px-6 py-2.5 text-right font-medium">Txns</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-white/5">
              {(averages.data ?? []).map((c) => (
                <tr key={c.category_id}>
                  <td className="px-6 py-2.5">
                    <span className="flex items-center gap-2">
                      {/* A chip beside a label: colour is redundant here. */}
                      <span
                        className="inline-block h-2 w-2 shrink-0 rounded-full"
                        style={{ backgroundColor: c.color ?? CHART.textMuted }}
                      />
                      {c.name}
                      {c.is_fixed && (
                        <span className="rounded border border-white/10 px-1.5 py-0.5 text-[10px] text-mist-500">
                          fixed
                        </span>
                      )}
                    </span>
                  </td>
                  <td className="tabular px-6 py-2.5 text-right">
                    {formatMoney(c.monthly_average)}
                  </td>
                  <td className="tabular px-6 py-2.5 text-right text-mist-300">
                    {formatMoney(c.total)}
                  </td>
                  <td className="tabular px-6 py-2.5 text-right text-mist-500">
                    {c.transaction_count}
                  </td>
                </tr>
              ))}
              {averages.data?.length === 0 && (
                <tr>
                  <td colSpan={4} className="px-6 py-8 text-center text-mist-500">
                    No spending recorded in the last year yet.
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </div>
      </section>
    </div>
  )
}

function Tile({
  label,
  value,
  hint,
  tone,
}: {
  label: string
  value: string
  hint?: string
  tone?: 'good' | 'critical'
}) {
  const color =
    tone === 'critical' ? STATUS.critical : tone === 'good' ? STATUS.good : undefined

  return (
    <div className="glass p-5">
      <p className="text-sm text-mist-300">{label}</p>
      <p
        className="tabular mt-2 text-3xl font-semibold"
        style={{ color: color ?? '#f2d492' }}
      >
        {value}
      </p>
      {hint && <p className="mt-1 text-xs text-mist-500">{hint}</p>}
    </div>
  )
}

function SplitTile({
  label,
  value,
  share,
  hint,
}: {
  label: string
  value: string
  share: number
  hint: string
}) {
  const pct = Number.isFinite(share) ? Math.round(share * 100) : 0
  return (
    <div className="glass p-5">
      <div className="flex items-baseline justify-between">
        <p className="text-sm text-mist-300">{label}</p>
        <p className="tabular text-xs text-mist-500">{pct}% of spending</p>
      </div>
      <p className="tabular mt-2 text-2xl font-semibold text-rune-300">{value}</p>
      <div className="mt-3 h-1.5 overflow-hidden rounded-full bg-white/5">
        <div
          className="h-full rounded-full"
          style={{ width: `${pct}%`, backgroundColor: '#9085e9' }}
        />
      </div>
      <p className="mt-2 text-xs text-mist-500">{hint}</p>
    </div>
  )
}

function Loading() {
  return <p className="py-8 text-center text-sm text-mist-500">Loading…</p>
}
