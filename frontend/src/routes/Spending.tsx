import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '../lib/api'
import { formatDate, formatMoney } from '../lib/money'
import { categoryDetailPath } from '../lib/categories'
import { CategoryBars } from '../components/charts/CategoryBars'
import { CategoryLink } from '../components/CategoryLink'
import { MerchantLink } from '../components/MerchantLink'
import { MerchantAvatar } from '../components/MerchantAvatar'
import { TrendChart } from '../components/charts/TrendChart'
import { RealBasis, RealToggle } from '../components/RealToggle'
import { useInflation, useRealPreference } from '../lib/inflation'
import { SavingsRateChart } from '../components/charts/SavingsRateChart'
import { FixedDiscretionaryChart } from '../components/charts/FixedDiscretionaryChart'
import { SpendingHeatmap } from '../components/charts/SpendingHeatmap'
import { CategoryMultiples } from '../components/charts/CategoryMultiples'
import { CashFlowSankey } from '../components/charts/CashFlowSankey'
import { AnimatedNumber } from '../components/motion'
import { SkeletonChart, SkeletonRows, SkeletonText, Reveal } from '../components/Skeleton'
import { enterProps } from '../lib/motion'
import { motion } from 'motion/react'
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
  const navigate = useNavigate()

  // Drill from a category bar into that category's own breakdown for the month
  // currently in view. This used to open a filtered transaction list, which is
  // where every category click in the app dead-ended; the breakdown offers that
  // list as one of its sections, alongside the questions the list cannot answer.
  const openCategory = (categoryID: string) =>
    navigate(categoryDetailPath(categoryID, range))

  const summary = useQuery({
    queryKey: ['summary', range.from, range.to],
    queryFn: () => api.summary(range),
  })
  const byCategory = useQuery({
    queryKey: ['by-category', range.from, range.to],
    queryFn: () => api.byCategory(range),
  })
  // The cash-flow Sankey carries its own income/spending breakdowns plus the
  // period totals in one round trip, so it does not lean on the two queries
  // above — but it uses the SAME server queries, so its bands reconcile with
  // these tiles to the cent.
  const cashFlow = useQuery({
    queryKey: ['cash-flow', range.from, range.to],
    queryFn: () => api.cashFlow(range),
  })
  // One trend query serves three charts here. Asking for `real` only ADDS
  // fields — the nominal ones are always present and unchanged — so the savings
  // rate and fixed/discretionary charts below are untouched by the toggle.
  const inflation = useInflation()
  const { enabled: real, setEnabled: setReal } = useRealPreference()
  const trend = useQuery({
    queryKey: ['trend', real],
    queryFn: () => api.trend({ real }),
  })
  // The category × month matrix is the trailing twelve months the trend chart
  // uses, fetched once and rendered two ways (heatmap + small multiples).
  const heatmap = useQuery({
    queryKey: ['heatmap'],
    queryFn: () => api.spendingHeatmap(),
  })
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
        <Tile label="Income" value={s ? s.income : null} />
        <Tile label="Spending" value={s ? s.spending : null} />
        <Tile
          label="Left to invest"
          value={s ? s.leftover : null}
          tone={s && Number(s.leftover) < 0 ? 'critical' : 'good'}
        />
        <Tile
          label="Savings rate"
          value={s?.savings_rate ?? null}
          format={(n) => `${(n * 100).toFixed(1)}%`}
          hint={
            s?.savings_rate == null ? 'No income recorded this period' : undefined
          }
        />
      </div>

      {s && Number(s.spending) > 0 && (
        <div className="grid gap-4 sm:grid-cols-2">
          <SplitTile
            label="Fixed"
            value={s.fixed_spending}
            share={Number(s.fixed_spending) / Number(s.spending)}
            hint="Rent, utilities, loan payments"
          />
          <SplitTile
            label="Discretionary"
            value={s.discretionary_spending}
            share={Number(s.discretionary_spending) / Number(s.spending)}
            hint="Everything you can flex"
          />
        </div>
      )}

      {/*
        Cash flow — the hero. Where the money came from, where it went, and what
        was left. Mounted here (top of Spending) as a DRAFT placement per MAD-33;
        the owner can move it to its own "Cash flow" surface. The bands carry the
        same totals the tiles above show: transfers and card payments are
        excluded, one-time charges included — the same money rules as everywhere.
      */}
      <section className="glass p-6">
        <h2 className="mb-1 text-lg font-medium">Cash flow</h2>
        <p className="mb-5 text-sm text-mist-300">
          Where {month.label}&rsquo;s money came from, where it went, and what was
          left. The bands add up to the same income, spending and leftover the
          tiles above show.
        </p>
        {cashFlow.isPending ? (
          <SkeletonChart />
        ) : cashFlow.data ? (
          <Reveal>
            <CashFlowSankey data={cashFlow.data} label={month.label} />
          </Reveal>
        ) : (
          <SkeletonChart />
        )}
      </section>

      {capabilities.data?.ai_enabled && (
        <MonthlySummaryCard month={month.value} label={month.label} />
      )}

      <RecurringSection />

      <section className="glass p-6">
        <h2 className="mb-1 text-lg font-medium">By category</h2>
        <p className="mb-5 text-sm text-mist-300">{month.label}</p>
        {byCategory.isPending ? (
          <SkeletonRows count={6} />
        ) : (
          <Reveal>
            <CategoryBars data={byCategory.data ?? []} onSelect={openCategory} />
          </Reveal>
        )}
      </section>

      <section className="glass p-6">
        <div className="mb-1 flex flex-wrap items-start justify-between gap-3">
          <h2 className="text-lg font-medium">Income vs spending</h2>
          <RealToggle
            enabled={real}
            onChange={setReal}
            inflation={inflation.data}
          />
        </div>
        <p className="mb-5 text-sm text-mist-300">Trailing twelve months</p>
        {trend.isPending ? <SkeletonChart /> : (
          <Reveal>
            <TrendChart data={trend.data ?? []} real={real} />
          </Reveal>
        )}
        <RealBasis enabled={real} inflation={inflation.data} />
      </section>

      <section className="glass p-6">
        <h2 className="mb-1 text-lg font-medium">Savings rate</h2>
        <p className="mb-5 text-sm text-mist-300">
          What share of income was left over each month — the arc, not just this
          month&rsquo;s number.
        </p>
        {trend.isPending ? <SkeletonChart /> : (
          <Reveal>
            <SavingsRateChart data={trend.data ?? []} />
          </Reveal>
        )}
      </section>

      <section className="glass p-6">
        <h2 className="mb-1 text-lg font-medium">Fixed vs discretionary</h2>
        <p className="mb-5 text-sm text-mist-300">
          Each month&rsquo;s spending split into the bills you can&rsquo;t flex
          and everything you can. The two segments sum to the month&rsquo;s
          total — the same split the period summary reports.
        </p>
        {trend.isPending ? <SkeletonChart /> : (
          <Reveal>
            <FixedDiscretionaryChart data={trend.data ?? []} />
          </Reveal>
        )}
      </section>

      <section className="glass p-6">
        <h2 className="mb-1 text-lg font-medium">Where it goes, when</h2>
        <p className="mb-5 text-sm text-mist-300">
          Spend by category across the last twelve months. Darker cells are
          bigger months; the ramp is one hue, so colour carries magnitude, not
          identity. Seasonality and creep show as a row darkening left to right.
        </p>
        {heatmap.isPending ? (
          <SkeletonChart />
        ) : (
          <Reveal>
            <SpendingHeatmap
              months={heatmap.data?.months ?? []}
              categories={heatmap.data?.categories ?? []}
            />
          </Reveal>
        )}
      </section>

      <section className="glass p-6">
        <h2 className="mb-1 text-lg font-medium">Category mix over time</h2>
        <p className="mb-5 text-sm text-mist-300">
          The top categories one panel each, each on its own scale — so a
          category&rsquo;s seasonal swing reads against its own range, not
          against a global max that flattens the rest.
        </p>
        {heatmap.isPending ? (
          <SkeletonChart />
        ) : (
          <Reveal>
            <CategoryMultiples
              months={heatmap.data?.months ?? []}
              categories={heatmap.data?.categories ?? []}
            />
          </Reveal>
        )}
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
                      {/* A chip beside a label: colour is redundant here.
                          These figures are the trailing year, so the breakdown
                          opens on its own default window rather than the month
                          selected above — the numbers would not match otherwise. */}
                      <CategoryLink
                        name={c.name}
                        categoryID={c.category_id}
                        color={c.color ?? CHART.textMuted}
                        showDot
                      />
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
  format,
  fallback = '—',
}: {
  label: string
  value: string | number | null | undefined
  hint?: string
  tone?: 'good' | 'critical'
  format?: (n: number) => string
  fallback?: string
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
        <AnimatedNumber value={value} format={format} fallback={fallback} />
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
  value: string | number | null | undefined
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
      <p className="tabular mt-2 text-2xl font-semibold text-rune-300">
        <AnimatedNumber value={value} />
      </p>
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
      <div className="flex flex-wrap items-center justify-between gap-4">
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
          <SkeletonText lines={3} />
        ) : text ? (
          <Reveal>
            <p className="leading-relaxed text-mist-100">{text}</p>
          </Reveal>
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
  const queryClient = useQueryClient()
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
  const suppressed = useQuery({
    queryKey: ['recurring', 'suppressed'],
    queryFn: api.suppressedRecurring,
  })
  const rows = recurring.data ?? []
  const showType = capabilities.data?.ai_enabled ?? false

  // A change to the override list ripples into the recurring table, the
  // suppressed list, and the insight feed (whose new_recurring/subscription rows
  // read the same detector), so refresh all three after a mutation.
  const refreshAfterOverride = () => {
    queryClient.invalidateQueries({ queryKey: ['recurring'] })
    queryClient.invalidateQueries({ queryKey: ['insights'] })
  }
  const suppress = useMutation({
    mutationFn: (m: { merchantKey: string; merchant: string }) =>
      api.suppressRecurring(m.merchantKey, m.merchant),
    onSuccess: refreshAfterOverride,
  })
  const restore = useMutation({
    mutationFn: (merchantKey: string) => api.unsuppressRecurring(merchantKey),
    onSuccess: refreshAfterOverride,
  })

  // Index subscription insights by merchant name for an O(1) per-row lookup.
  // Both the recurring report and the insight use COALESCE(merchant_name, name)
  // as the merchant, so the strings line up.
  const subByMerchant = new Map<string, Record<string, string | number>>()
  for (const i of insights.data ?? []) {
    if (i.kind === 'subscription') subByMerchant.set(String(i.data.merchant), i.data)
  }

  // Merchant, Type?, Cadence, Typical, ~/month, Last seen, actions.
  const cols = showType ? 7 : 6
  const suppressedRows = suppressed.data ?? []

  return (
    <section className="glass overflow-hidden">
      <div className="px-6 pt-6">
        <h2 className="text-lg font-medium">Recurring &amp; subscriptions</h2>
        <p className="mt-1 mb-5 text-sm text-mist-300">
          Merchants that charge you on a regular cadence — weekly through yearly
          — detected from the last three years of activity. If one is a
          coincidence, mark it{' '}
          <span className="text-mist-400">Not recurring</span> to hide it.
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
              <th className="px-6 py-2.5 font-medium">
                <span className="sr-only">Actions</span>
              </th>
            </tr>
          </thead>
          <tbody className="divide-y divide-white/5">
            {rows.map((m, i) => {
              const sub = subByMerchant.get(m.merchant)
              const creep = sub?.flavor === 'price_creep'
              const type = sub?.category ? String(sub.category) : null
              const suppressing =
                suppress.isPending && suppress.variables?.merchantKey === m.merchant_key
              return (
                <motion.tr key={m.merchant_key} {...enterProps(i)}>
                  <td className="px-6 py-2.5">
                    <span className="flex items-center gap-2">
                      {/* The detector already groups by resolved key, which is
                          exactly what addresses the merchant detail view. */}
                      <MerchantAvatar name={m.merchant} merchantKey={m.merchant_key} size="sm" />
                      <MerchantLink name={m.merchant} merchantKey={m.merchant_key} />
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
                    {formatMoney(m.typical_amount)}
                  </td>
                  <td className="tabular px-6 py-2.5 text-right text-mist-300">
                    {formatMoney(m.monthly_estimate)}
                  </td>
                  <td className="tabular px-6 py-2.5 text-right text-mist-500">
                    {formatDate(m.last_seen)}
                  </td>
                  <td className="px-6 py-2.5 text-right">
                    <button
                      type="button"
                      onClick={() =>
                        suppress.mutate({
                          merchantKey: m.merchant_key,
                          merchant: m.merchant,
                        })
                      }
                      disabled={suppressing}
                      className="text-xs text-mist-500 transition-colors hover:text-mist-200 disabled:opacity-50"
                    >
                      {suppressing ? 'Hiding…' : 'Not recurring'}
                    </button>
                  </td>
                </motion.tr>
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
                <td colSpan={cols} className="px-6 py-6">
                  <SkeletonRows count={3} />
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>

      {suppressedRows.length > 0 && (
        <div className="border-t border-white/5 px-6 py-4">
          <p className="text-xs text-mist-500">
            Marked not recurring — won&apos;t appear above or in insights:
          </p>
          <div className="mt-2 flex flex-wrap gap-2">
            {suppressedRows.map((s) => {
              const restoring =
                restore.isPending && restore.variables === s.merchant_key
              return (
                <span
                  key={s.merchant_key}
                  className="inline-flex items-center gap-1.5 rounded-full border border-white/10 bg-white/5 px-2.5 py-1 text-xs text-mist-300"
                >
                  {/* merchant_key_resolved, not merchant_key: the stored key is
                      what unsuppress acts on, but a suppression recorded before a
                      later merge would link nowhere. */}
                  <MerchantAvatar
                    name={s.merchant || s.merchant_key}
                    merchantKey={s.merchant_key_resolved}
                    size="xs"
                  />
                  <MerchantLink
                    name={s.merchant || s.merchant_key}
                    merchantKey={s.merchant_key_resolved}
                    variant="chip"
                  />
                  <button
                    type="button"
                    onClick={() => restore.mutate(s.merchant_key)}
                    disabled={restoring}
                    className="text-mist-500 transition-colors hover:text-mist-100 disabled:opacity-50"
                    title="Restore to the recurring detector"
                  >
                    {restoring ? '…' : '×'}
                  </button>
                </span>
              )
            })}
          </div>
        </div>
      )}
    </section>
  )
}
