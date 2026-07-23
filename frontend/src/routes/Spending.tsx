import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '../lib/api'
import { formatDate, formatMoney } from '../lib/money'
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
  const capabilities = useQuery({
    queryKey: ['capabilities'],
    queryFn: api.capabilities,
    staleTime: Infinity,
  })

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

      {capabilities.data?.ai_enabled && (
        <MonthlySummaryCard month={month.value} label={month.label} />
      )}

      <RecurringSection />

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

// MonthlySummaryCard shows the AI-written recap for the selected month, cached
// server-side. It only mounts when AI is enabled (the parent gates on it).
function MonthlySummaryCard({ month, label }: { month: string; label: string }) {
  const qc = useQueryClient()
  const summary = useQuery({
    queryKey: ['monthly-summary', month],
    queryFn: () => api.monthlySummary(month),
  })

  const generate = useMutation({
    mutationFn: () => api.generateMonthlySummary(month),
    onSuccess: (data) => qc.setQueryData(['monthly-summary', month], data),
  })

  const text = summary.data?.summary
  const busy = generate.isPending

  return (
    <section className="glass p-6">
      <div className="flex items-center justify-between gap-4">
        <div>
          <h2 className="text-lg font-medium">Monthly recap</h2>
          <p className="text-sm text-mist-300">{label}, in plain English</p>
        </div>
        <button
          className="btn-ghost shrink-0 px-3 py-1.5 text-sm"
          disabled={busy}
          onClick={() => generate.mutate()}
        >
          {busy ? 'Writing…' : text ? 'Regenerate' : 'Generate'}
        </button>
      </div>

      {generate.isError && (
        <p role="alert" className="mt-4 text-sm text-ember-400">
          {generate.error.message}
        </p>
      )}

      <div className="mt-4">
        {summary.isPending ? (
          <Loading />
        ) : text ? (
          <p className="leading-relaxed text-mist-100">{text}</p>
        ) : (
          <p className="text-sm text-mist-500">
            No recap yet. Generate one to see the month at a glance.
          </p>
        )}
      </div>
    </section>
  )
}

// RecurringSection lists detected subscriptions and regular bills, with each
// charge normalised to a monthly figure (computed server-side — never summed
// here). It also joins each row against the `subscription` insights (doc 05):
// a "price up" badge when that merchant's charge has crept up, and, when AI is
// enabled, the classified type. The join is by merchant name against the feed
// the app already fetches — no bespoke endpoint. Everything degrades cleanly:
// the table is fully deterministic, the badge and type are additive.
function RecurringSection() {
  const recurring = useQuery({ queryKey: ['recurring'], queryFn: api.recurring })
  const insights = useQuery({
    queryKey: ['insights', 'all'],
    queryFn: () => api.insights({ state: 'all' }),
  })
  const capabilities = useQuery({
    queryKey: ['capabilities'],
    queryFn: api.capabilities,
    staleTime: Infinity,
  })
  const rows = recurring.data ?? []
  const showType = capabilities.data?.ai_enabled ?? false

  // Index subscription insights by merchant name for an O(1) per-row lookup.
  // Both the recurring report and the insight use COALESCE(merchant_name, name)
  // as the merchant, so the strings line up.
  const subByMerchant = new Map<string, Record<string, string | number>>()
  for (const i of insights.data ?? []) {
    if (i.kind === 'subscription') subByMerchant.set(String(i.data.merchant), i.data)
  }

  const cols = showType ? 6 : 5

  return (
    <section className="glass overflow-hidden">
      <div className="px-6 pt-6">
        <h2 className="text-lg font-medium">Recurring &amp; subscriptions</h2>
        <p className="mt-1 mb-5 text-sm text-mist-300">
          Merchants that charge you on a regular cadence, detected from the last
          year of activity.
        </p>
      </div>

      <div className="overflow-x-auto">
        <table className="w-full text-sm">
          <thead>
            <tr className="border-y border-white/5 text-left text-xs text-mist-500">
              <th className="px-6 py-2.5 font-medium">Merchant</th>
              {showType && <th className="px-6 py-2.5 font-medium">Type</th>}
              <th className="px-6 py-2.5 font-medium">Cadence</th>
              <th className="px-6 py-2.5 text-right font-medium">Typical</th>
              <th className="px-6 py-2.5 text-right font-medium">~ / month</th>
              <th className="px-6 py-2.5 text-right font-medium">Last seen</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-white/5">
            {rows.map((m) => {
              const sub = subByMerchant.get(m.merchant)
              const creep = sub?.flavor === 'price_creep'
              const type = sub?.category ? String(sub.category) : null
              return (
                <tr key={m.merchant}>
                  <td className="px-6 py-2.5">
                    <span className="flex items-center gap-2">
                      {m.merchant}
                      {creep && (
                        <span className="rounded border border-fern-400/30 bg-fern-400/10 px-1.5 py-0.5 text-[10px] text-fern-300">
                          price up
                        </span>
                      )}
                    </span>
                  </td>
                  {showType && (
                    <td className="px-6 py-2.5 text-mist-300 capitalize">
                      {type ?? '—'}
                    </td>
                  )}
                  <td className="px-6 py-2.5 text-mist-300">{m.cadence}</td>
                  <td className="tabular px-6 py-2.5 text-right">
                    {formatMoney(m.average_amount)}
                  </td>
                  <td className="tabular px-6 py-2.5 text-right text-mist-300">
                    {formatMoney(m.monthly_estimate)}
                  </td>
                  <td className="tabular px-6 py-2.5 text-right text-mist-500">
                    {formatDate(m.last_seen)}
                  </td>
                </tr>
              )
            })}
            {!recurring.isPending && rows.length === 0 && (
              <tr>
                <td colSpan={cols} className="px-6 py-8 text-center text-mist-500">
                  No recurring charges detected yet.
                </td>
              </tr>
            )}
            {recurring.isPending && (
              <tr>
                <td colSpan={cols} className="px-6 py-8 text-center text-mist-500">
                  Loading…
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>
    </section>
  )
}
