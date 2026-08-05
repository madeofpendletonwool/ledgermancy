import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { motion, useReducedMotion } from 'motion/react'
import {
  api,
  type AllocationSlice,
  type BenchmarkComparison,
  type DetailedHolding,
  type InvestmentAccount,
  type InvestmentPerformance,
  type InvestmentPeriod,
  type TaxTreatment,
} from '../lib/api'
import { formatMoney } from '../lib/money'
import { DividendBars } from '../components/charts/DividendBars'
import { lineDraw } from '../components/charts/motion'
import { AnimatedNumber } from '../components/motion'
import { SkeletonTiles } from '../components/Skeleton'
import { RealToggle } from '../components/RealToggle'
import { formatRate, realLabel, useInflation, useRealPreference } from '../lib/inflation'
import { CHART, SERIES, SINGLE_SERIES, STATUS } from '../components/charts/tokens'
import { ManualInvestmentEditor } from '../components/ManualInvestments'

const PERIODS: { value: InvestmentPeriod; label: string }[] = [
  { value: 'ytd', label: 'YTD' },
  { value: '1y', label: '1 year' },
  { value: '3y', label: '3 years' },
  { value: '5y', label: '5 years' },
  { value: 'inception', label: 'All' },
]

const TAX_TREATMENTS: { value: TaxTreatment; label: string }[] = [
  { value: 'taxable', label: 'Taxable brokerage' },
  { value: 'trad_401k', label: 'Traditional 401(k)' },
  { value: 'roth_401k', label: 'Roth 401(k)' },
  { value: 'trad_ira', label: 'Traditional IRA' },
  { value: 'roth_ira', label: 'Roth IRA' },
  { value: '529', label: '529' },
  { value: 'hsa', label: 'HSA' },
  { value: 'trust', label: 'Trust' },
  { value: 'utma_ugma', label: 'UTMA / UGMA' },
  { value: 'coverdell', label: 'Coverdell ESA' },
  { value: 'custodial_roth', label: 'Custodial Roth IRA' },
  { value: 'trump', label: 'Trump account' },
  { value: 'other', label: 'Other' },
]

/**
 * Treatments whose balance belongs to a dependent rather than the household.
 * Mirrors networth.IsCustodial on the server; kept in step deliberately, since
 * the UI labels these as excluded from retirement and must not disagree.
 */
const CUSTODIAL_TREATMENTS: TaxTreatment[] = [
  '529',
  'utma_ugma',
  'coverdell',
  'custodial_roth',
  'trump',
]

function treatmentLabel(value: TaxTreatment | null): string {
  return TAX_TREATMENTS.find((t) => t.value === value)?.label ?? 'Untagged'
}

/**
 * Formats a return fraction as a percentage. The backend sends fractions
 * ("0.0734"), so the ×100 happens exactly once, here.
 */
function formatPercent(value: string | null | undefined, digits = 2): string {
  if (value === null || value === undefined || value === '') return '—'
  const n = Number(value)
  if (!Number.isFinite(n)) return '—'
  return `${n >= 0 ? '+' : ''}${(n * 100).toFixed(digits)}%`
}

/** Formats an already-percentage figure (allocation shares, gain %). */
function formatShare(value: string, digits = 1): string {
  const n = Number(value)
  return Number.isFinite(n) ? `${n.toFixed(digits)}%` : '—'
}

export function Investments() {
  const [period, setPeriod] = useState<InvestmentPeriod>('1y')

  const overview = useQuery({ queryKey: ['investments'], queryFn: api.investments })
  // The real figures ride along in the same response as extra fields, so the
  // toggle changes which ones are DISPLAYED, not which numbers exist. The
  // nominal figures are always there and are never overwritten.
  const inflation = useInflation()
  const { enabled: real, setEnabled: setReal } = useRealPreference()
  const performance = useQuery({
    queryKey: ['investments', 'performance', period, real],
    queryFn: () => api.investmentPerformance(period, real),
  })
  const benchmarks = useQuery({
    queryKey: ['investments', 'benchmarks', period],
    queryFn: () => api.investmentBenchmarks(period),
  })
  const allocation = useQuery({
    queryKey: ['investments', 'allocation'],
    queryFn: api.investmentAllocation,
  })
  const holdings = useQuery({
    queryKey: ['investments', 'holdings'],
    queryFn: api.investmentHoldings,
  })
  const fees = useQuery({ queryKey: ['investments', 'fees'], queryFn: api.investmentFees })
  const dividends = useQuery({
    queryKey: ['investments', 'dividends'],
    queryFn: api.investmentDividends,
  })

  const data = overview.data
  const hasAccounts = (data?.accounts.length ?? 0) > 0

  return (
    <div className="space-y-8">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <h1 className="text-2xl font-semibold">Investments</h1>
          <p className="mt-1 text-mist-300">
            What you hold, what it has actually returned, and what it costs you to hold it.
          </p>
        </div>
        <a className="btn-ghost text-sm" href="/api/export/holdings.csv">
          Holdings CSV
        </a>
      </div>

      {overview.isSuccess && !hasAccounts && (
        <section className="glass p-6">
          <h2 className="text-lg font-medium">No investment accounts linked</h2>
          <p className="mt-2 text-sm text-mist-300">
            Link an institution with the Investments product enabled and your holdings will
            appear here. Ledgermancy records what they are worth once a day from that point
            on — your bank reports today's balance and keeps no history, so performance is
            measured from the day linking happens, not from when you opened the account.
          </p>
        </section>
      )}

      {hasAccounts && (
        <>
          <div className="grid gap-4 sm:grid-cols-3">
            <Tile label="Total value" value={data!.total_value} large />
            <Tile
              label="Unrealised gain"
              value={data!.unrealised_gain ?? null}
              sub={
                data!.unrealised_gain_pct
                  ? formatShare(data!.unrealised_gain_pct, 2)
                  : 'No cost basis reported'
              }
              tone={
                data!.unrealised_gain
                  ? Number(data!.unrealised_gain) >= 0
                    ? 'good'
                    : 'debt'
                  : undefined
              }
            />
            <Tile
              label="Recorded history"
              value={data!.history_days > 0 ? data!.history_days : null}
              format={(n) => `${Math.round(n)} days`}
              fallback="None yet"
              sub="Since Ledgermancy started watching"
            />
          </div>

          {data!.basis_excluded_holdings > 0 && (
            <Disclosure>
              The gain above covers {formatMoney(data!.basis_coverage_value)} of holdings.{' '}
              {data!.basis_excluded_holdings}{' '}
              {data!.basis_excluded_holdings === 1 ? 'holding reports' : 'holdings report'} no
              cost basis and {data!.basis_excluded_holdings === 1 ? 'is' : 'are'} left out
              rather than counted as free.
            </Disclosure>
          )}

          {data!.untagged_accounts > 0 && (
            <AccountTagging accounts={data!.accounts} />
          )}

          <section className="glass p-6">
            <div className="mb-5 flex flex-wrap items-start justify-between gap-4">
              <div>
                <h2 className="text-lg font-medium">Performance</h2>
                <p className="mt-1 text-sm text-mist-300">
                  Time-weighted measures the portfolio. Money-weighted measures you — it
                  accounts for when you paid in.
                </p>
              </div>
              <div className="space-y-2">
                <PeriodPicker value={period} onChange={setPeriod} />
                <RealToggle
                  enabled={real}
                  onChange={setReal}
                  inflation={inflation.data}
                  // A YTD window is not a year. Deflating a partial year by a
                  // partial year's price change says considerably less than it
                  // looks like it says; the other periods all clear the span.
                  shouldRender={period !== 'ytd'}
                />
              </div>
            </div>
            <Performance
              data={performance.data}
              loading={performance.isPending}
              real={real}
              baseLabel={realLabel(inflation.data)}
            />
          </section>

          <section className="glass p-6">
            <h2 className="text-lg font-medium">Against the market</h2>
            <p className="mt-1 mb-5 text-sm text-mist-300">
              {benchmarks.data?.basis ??
                'Growth of 100, with your own deposits and withdrawals removed.'}
            </p>
            <BenchmarkChart data={benchmarks.data} />
          </section>

          {allocation.data && (
            <section className="glass p-6">
              <h2 className="text-lg font-medium">Allocation</h2>
              <p className="mt-1 mb-5 text-sm text-mist-300">{allocation.data.note}</p>
              <div className="grid gap-8 sm:grid-cols-2">
                <AllocationList
                  title="By asset class"
                  slices={allocation.data.by_asset_class}
                />
                <AllocationList
                  title="By tax treatment"
                  slices={allocation.data.by_tax_treatment}
                />
              </div>
            </section>
          )}

          <HoldingsTable holdings={holdings.data ?? []} />

          <ManualInvestmentAccounts
            accounts={data!.accounts.filter((a) => a.source === 'manual')}
          />

          <div className="grid gap-4 sm:grid-cols-2">
            <section className="glass p-6">
              <h2 className="text-lg font-medium">Fund fees</h2>
              {fees.data && fees.data.covered_holdings > 0 ? (
                <>
                  <p className="tabular mt-3 text-3xl font-semibold" style={{ color: STATUS.serious }}>
                    <AnimatedNumber value={fees.data.annual_cost} />
                  </p>
                  <p className="mt-1 text-sm text-mist-300">per year in fund expenses</p>
                </>
              ) : (
                <p className="mt-3 text-3xl font-semibold text-mist-500">—</p>
              )}
              <p className="mt-3 text-xs text-mist-500">{fees.data?.note}</p>
            </section>

            <section className="glass p-6">
              <h2 className="text-lg font-medium">Dividends</h2>
              {dividends.data && dividends.data.months.length > 0 ? (
                <>
                  <p className="tabular mt-3 text-3xl font-semibold" style={{ color: STATUS.good }}>
                    <AnimatedNumber value={dividends.data.total} />
                  </p>
                  <p className="mt-1 text-sm text-mist-300">
                    received over the last two years
                  </p>
                  <div className="mt-5">
                    <DividendBars months={dividends.data.months} />
                  </div>
                </>
              ) : (
                <p className="mt-3 text-sm text-mist-300">
                  No dividend transactions reported yet.
                </p>
              )}
              <p className="mt-3 text-xs text-mist-500">{dividends.data?.basis}</p>
            </section>
          </div>
        </>
      )}
    </div>
  )
}

function PeriodPicker({
  value,
  onChange,
}: {
  value: InvestmentPeriod
  onChange: (p: InvestmentPeriod) => void
}) {
  return (
    <div className="flex flex-wrap gap-1" role="group" aria-label="Period">
      {PERIODS.map((p) => (
        <button
          key={p.value}
          type="button"
          aria-pressed={p.value === value}
          onClick={() => onChange(p.value)}
          className={`rounded-lg px-3 py-1.5 text-sm transition ${
            p.value === value
              ? 'bg-white/10 text-mist-100'
              : 'text-mist-300 hover:bg-white/5 hover:text-mist-100'
          }`}
        >
          {p.label}
        </button>
      ))}
    </div>
  )
}

/**
 * The performance block.
 *
 * The caveat is not decoration: Plaid serves no historical holdings, so a young
 * install genuinely cannot know what the portfolio returned last year. It is
 * rendered above the numbers, not below them, so it cannot be scrolled past.
 */
function Performance({
  data,
  loading,
  real = false,
  baseLabel,
}: {
  data: InvestmentPerformance | undefined
  loading: boolean
  real?: boolean
  baseLabel?: string | null
}) {
  if (loading)
    return (
      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <SkeletonTiles count={4} />
      </div>
    )
  if (!data) return null

  if (!data.computable) {
    return (
      <p className="rounded-xl border border-white/10 bg-white/5 px-4 py-3 text-sm text-mist-300">
        {data.caveat}
      </p>
    )
  }

  // Real is shown only where a real figure actually exists. Where it does not —
  // a span reaching outside the CPI series, or one too short to annualise — the
  // tile stays nominal and says so, rather than relabelling a nominal number.
  const showReal = real && data.real !== undefined
  const twr = showReal ? (data.real!.twr ?? data.twr) : data.twr
  const annualised = showReal ? (data.real!.annualised ?? null) : data.annualised
  const mwr = showReal ? (data.real!.mwr ?? null) : data.mwr

  return (
    <div className="space-y-5">
      {data.caveat && <Disclosure>{data.caveat}</Disclosure>}

      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <Stat
          label={showReal ? 'Time-weighted (real)' : 'Time-weighted'}
          value={twr}
          format={(n) => formatPercent(String(n))}
          sub={
            annualised
              ? `${formatPercent(annualised)} a year${showReal ? ', after inflation' : ''}`
              : 'Too short a span to annualise'
          }
          tone={twr ? (Number(twr) >= 0 ? 'good' : 'debt') : undefined}
        />
        <Stat
          label={showReal && mwr ? 'Money-weighted (real)' : 'Money-weighted'}
          value={mwr}
          format={(n) => formatPercent(String(n))}
          sub={
            mwr
              ? `Annualised (IRR)${showReal ? ', after inflation' : ''}`
              : showReal
                ? 'Too short a span to state after inflation'
                : data.mwr_note
          }
          tone={mwr ? (Number(mwr) >= 0 ? 'good' : 'debt') : undefined}
        />
        <Stat
          label="Market gain"
          value={data.gain}
          sub="Your deposits removed"
          tone={Number(data.gain) >= 0 ? 'good' : 'debt'}
        />
        <Stat
          label="Net paid in"
          value={data.net_flows}
          sub={`${data.start} → ${data.end}`}
        />
      </div>

      {showReal && (
        <p className="text-xs text-mist-500">
          Prices rose {formatRate(data.real!.inflation)} over this span
          {data.real!.annual_inflation
            ? ` (${formatRate(data.real!.annual_inflation)} a year)`
            : ''}
          . Nominally: {formatPercent(data.twr)} time-weighted
          {data.mwr ? `, ${formatPercent(data.mwr)} money-weighted` : ''}.{' '}
          {data.real!.note}
          {baseLabel ? ` Dollar figures above are not deflated; a real dollar total would be ${baseLabel}.` : ''}
        </p>
      )}

      {real && data.real === undefined && (
        <p className="text-xs text-mist-500">
          These figures are nominal: this span reaches outside the published
          price index, so there is no honest way to state the return after
          inflation.
        </p>
      )}
    </div>
  )
}

/** Rebased growth chart: the portfolio against any configured benchmarks. */
function BenchmarkChart({ data }: { data: BenchmarkComparison | undefined }) {
  const series = data?.series ?? []
  const withPoints = series.filter((s) => s.points.length >= 2)
  const reduce = useReducedMotion() ?? false

  if (withPoints.length === 0) {
    return (
      <p className="py-10 text-center text-sm" style={{ color: CHART.textMuted }}>
        Not enough recorded history to plot yet.
      </p>
    )
  }

  const W = 760
  const H = 240
  const PAD = { top: 12, right: 12, bottom: 28, left: 56 }
  const plotW = W - PAD.left - PAD.right
  const plotH = H - PAD.top - PAD.bottom

  const allValues = withPoints.flatMap((s) => s.points.map((p) => Number(p.value)))
  const lo = Math.min(...allValues, 100)
  const hi = Math.max(...allValues, 100)
  const margin = (hi - lo || 10) * 0.12
  const min = lo - margin
  const max = hi + margin
  const span = max - min || 1

  // Every series shares one date axis so the lines are comparable. The longest
  // series defines it; a shorter one simply starts further along.
  const dates = withPoints.reduce<string[]>(
    (longest, s) => (s.points.length > longest.length ? s.points.map((p) => p.date) : longest),
    [],
  )
  const xFor = (date: string) => {
    const i = dates.indexOf(date)
    const idx = i >= 0 ? i : dates.length - 1
    return PAD.left + (idx / Math.max(dates.length - 1, 1)) * plotW
  }
  const y = (v: number) => PAD.top + plotH - ((v - min) / span) * plotH

  const colours = [SERIES.leftover, SERIES.income, SERIES.spending, SINGLE_SERIES]

  return (
    <div className="space-y-3">
      <div className="overflow-x-auto">
        <svg
          viewBox={`0 0 ${W} ${H}`}
          className="w-full max-sm:min-w-0 sm:min-w-[520px]"
          role="img"
          aria-label="Portfolio growth against benchmarks, rebased to 100"
        >
          {/* The 100 line is the baseline everything is measured from, so it is
              drawn explicitly rather than left to be inferred. */}
          {min < 100 && max > 100 && (
            <line
              x1={PAD.left}
              x2={W - PAD.right}
              y1={y(100)}
              y2={y(100)}
              stroke={CHART.axis}
              strokeWidth={1}
              strokeDasharray="3 3"
            />
          )}
          <text x={PAD.left - 8} y={y(max) + 4} textAnchor="end" fontSize="11" fill={CHART.textMuted}>
            {max.toFixed(0)}
          </text>
          <text x={PAD.left - 8} y={y(min) + 4} textAnchor="end" fontSize="11" fill={CHART.textMuted}>
            {min.toFixed(0)}
          </text>

          {withPoints.map((s, i) => {
            const d = s.points
              .map(
                (p, j) =>
                  `${j === 0 ? 'M' : 'L'} ${xFor(p.date)} ${y(Number(p.value))}`,
              )
              .join(' ')
            return (
              <motion.path
                key={s.label}
                fill="none"
                stroke={colours[i % colours.length]}
                strokeWidth={i === 0 ? 2.5 : 1.5}
                {...lineDraw(d, reduce)}
              />
            )
          })}

          <text x={PAD.left} y={H - 8} fontSize="11" fill={CHART.textMuted}>
            {dates[0]}
          </text>
          <text
            x={W - PAD.right}
            y={H - 8}
            textAnchor="end"
            fontSize="11"
            fill={CHART.textMuted}
          >
            {dates[dates.length - 1]}
          </text>
        </svg>
      </div>

      <div className="flex flex-wrap gap-4">
        {withPoints.map((s, i) => (
          <span key={s.label} className="flex items-center gap-2 text-xs text-mist-300">
            <span
              className="inline-block h-2.5 w-2.5 rounded-full"
              style={{ background: colours[i % colours.length] }}
            />
            {s.label}
          </span>
        ))}
      </div>

      {data && !data.enabled && (
        <p className="text-xs text-mist-500">
          Benchmark comparison is off. Ledgermancy only calls Plaid and your AI provider
          unless you opt in — set <code>BENCHMARK_PRICES_ENABLED=true</code> to let it fetch
          daily index closes.
        </p>
      )}
    </div>
  )
}

function AllocationList({ title, slices }: { title: string; slices: AllocationSlice[] }) {
  if (slices.length === 0) {
    return (
      <div>
        <h3 className="mb-3 text-xs font-medium tracking-wide text-mist-500 uppercase">
          {title}
        </h3>
        <p className="text-sm text-mist-300">Nothing to show yet.</p>
      </div>
    )
  }

  return (
    <div>
      <h3 className="mb-3 text-xs font-medium tracking-wide text-mist-500 uppercase">
        {title}
      </h3>
      <ul className="space-y-2.5">
        {slices.map((s) => {
          // "Unknown" and "Untagged" are honest gaps, not categories. They are
          // shown muted so the eye does not read them as a real allocation.
          const isGap = s.label === 'Unknown' || s.label === 'Untagged'
          return (
            <li key={s.label} className="space-y-1">
              <div className="flex items-baseline justify-between text-sm">
                <span className={isGap ? 'text-mist-500 italic' : 'text-mist-100'}>
                  {s.label}
                </span>
                <span className="tabular text-mist-300">
                  {formatShare(s.percent)} · {formatMoney(s.value)}
                </span>
              </div>
              <div className="h-1.5 overflow-hidden rounded-full bg-white/5">
                <div
                  className="h-full rounded-full"
                  style={{
                    width: `${Math.min(Number(s.percent), 100)}%`,
                    background: isGap ? CHART.axis : SINGLE_SERIES,
                  }}
                />
              </div>
            </li>
          )
        })}
      </ul>
    </div>
  )
}

type SortKey = 'value' | 'gain' | 'name'

function HoldingsTable({ holdings }: { holdings: DetailedHolding[] }) {
  const [sort, setSort] = useState<SortKey>('value')

  if (holdings.length === 0) return null

  // Sorted in JS, which is safe here: these are comparisons for display order,
  // not arithmetic. Nothing on this page sums a value in the browser.
  const sorted = [...holdings].sort((a, b) => {
    switch (sort) {
      case 'gain':
        return Number(b.gain ?? '-Infinity') - Number(a.gain ?? '-Infinity')
      case 'name':
        return (a.security_name ?? '').localeCompare(b.security_name ?? '')
      default:
        return Number(b.value ?? 0) - Number(a.value ?? 0)
    }
  })

  return (
    <section className="glass overflow-hidden">
      <div className="flex flex-wrap items-start justify-between gap-4 px-6 pt-6 pb-4">
        <div>
          <h2 className="text-lg font-medium">Holdings</h2>
          <p className="mt-1 text-sm text-mist-300">
            {holdings.length} {holdings.length === 1 ? 'position' : 'positions'} across your
            investment accounts.
          </p>
        </div>
        <div className="flex gap-1" role="group" aria-label="Sort holdings">
          {(
            [
              ['value', 'Value'],
              ['gain', 'Gain'],
              ['name', 'Name'],
            ] as [SortKey, string][]
          ).map(([key, label]) => (
            <button
              key={key}
              type="button"
              aria-pressed={sort === key}
              onClick={() => setSort(key)}
              className={`rounded-lg px-3 py-1.5 text-sm transition ${
                sort === key
                  ? 'bg-white/10 text-mist-100'
                  : 'text-mist-300 hover:bg-white/5 hover:text-mist-100'
              }`}
            >
              {label}
            </button>
          ))}
        </div>
      </div>

      <div className="overflow-x-auto">
        <table className="w-full text-sm">
          <thead>
            <tr className="border-y border-white/5 text-left text-xs text-mist-500">
              <th className="px-6 py-2.5 font-medium">Security</th>
              <th className="px-6 py-2.5 text-right font-medium">Quantity</th>
              <th className="px-6 py-2.5 text-right font-medium">Price</th>
              <th className="px-6 py-2.5 text-right font-medium">Cost basis</th>
              <th className="px-6 py-2.5 text-right font-medium">Value</th>
              <th className="px-6 py-2.5 text-right font-medium">Gain</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-white/5">
            {sorted.map((h) => (
              <tr key={h.id}>
                <td className="px-6 py-2.5">
                  <span className="font-medium">{h.security_name ?? 'Unknown'}</span>
                  {h.ticker && <span className="ml-2 text-xs text-mist-500">{h.ticker}</span>}
                  <span className="block text-xs text-mist-500">
                    {h.account_name}
                    {h.tax_treatment && ` · ${treatmentLabel(h.tax_treatment)}`}
                  </span>
                </td>
                <td className="tabular px-6 py-2.5 text-right text-mist-300">
                  {trimQuantity(h.quantity)}
                </td>
                <td className="tabular px-6 py-2.5 text-right text-mist-300">
                  {formatMoney(h.last_price)}
                </td>
                <td className="tabular px-6 py-2.5 text-right text-mist-300">
                  {/* An em dash, never "$0.00": an unknown basis and a zero
                      basis are different claims. */}
                  {h.cost_basis ? formatMoney(h.cost_basis) : '—'}
                </td>
                <td className="tabular px-6 py-2.5 text-right">{formatMoney(h.value)}</td>
                <td
                  className="tabular px-6 py-2.5 text-right"
                  style={{
                    color: h.gain
                      ? Number(h.gain) >= 0
                        ? STATUS.good
                        : STATUS.critical
                      : CHART.textMuted,
                  }}
                >
                  {h.gain ? formatMoney(h.gain) : '—'}
                  {h.gain_pct && (
                    <span className="ml-2 text-xs opacity-70">
                      {formatShare(h.gain_pct, 1)}
                    </span>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </section>
  )
}

/**
 * The tagging prompt.
 *
 * Deliberately unmissable while accounts are untagged, and deliberately a
 * *suggestion*: the picker is pre-filled from the Plaid subtype only where that
 * subtype is unambiguous, and nothing is written until the user saves. Plaid
 * reports a Roth 401(k) and a traditional one identically, and a wrong tag here
 * silently changes every retirement figure built on it.
 */
function AccountTagging({ accounts }: { accounts: InvestmentAccount[] }) {
  const untagged = accounts.filter((a) => a.tax_treatment === null)
  if (untagged.length === 0) return null

  return (
    <section className="glass border border-arcane-500/30 p-6">
      <h2 className="text-lg font-medium">
        {untagged.length} {untagged.length === 1 ? 'account needs' : 'accounts need'}{' '}
        classifying
      </h2>
      <p className="mt-1 mb-5 text-sm text-mist-300">
        Your bank reports the plan type but not its tax treatment — a Roth 401(k) and a
        traditional one look identical to it. Ledgermancy will not guess, because the
        difference changes every retirement projection built on these accounts.
      </p>
      <ul className="space-y-3">
        {untagged.map((a) => (
          <AccountTagRow key={a.id} account={a} />
        ))}
      </ul>
    </section>
  )
}

function AccountTagRow({ account }: { account: InvestmentAccount }) {
  const qc = useQueryClient()
  const [choice, setChoice] = useState<TaxTreatment | ''>(
    account.suggested_tax_treatment,
  )
  const [managed, setManaged] = useState(false)
  const [beneficiary, setBeneficiary] = useState<string>('')

  // Only asked for a custodial treatment. A joint checking account has two
  // adult owners and this field cannot express that, so offering it everywhere
  // would invite the wrong answer.
  const isCustodial = choice !== '' && CUSTODIAL_TREATMENTS.includes(choice)
  const people = useQuery({
    queryKey: ['people'],
    queryFn: api.people,
    enabled: isCustodial,
  })

  const save = useMutation({
    mutationFn: async () => {
      await api.setAccountTaxTreatment(account.id, {
        tax_treatment: choice === '' ? null : choice,
        is_managed: managed,
      })
      // Written second and only when asked for, so an ordinary tagging never
      // touches the beneficiary column.
      if (isCustodial) {
        await api.setAccountBeneficiary(account.id, beneficiary || null)
      }
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['investments'] })
      qc.invalidateQueries({ queryKey: ['networth-by-person'] })
    },
  })

  return (
    <li className="flex flex-wrap items-end gap-3 rounded-xl bg-white/5 px-4 py-3">
      <div className="min-w-[10rem] flex-1">
        <p className="font-medium">
          {account.name}
          {account.mask && <span className="text-mist-500"> ••{account.mask}</span>}
        </p>
        {/* A manual account has no institution and no reported subtype — its
            subtype is whatever the user typed, so quoting it back as evidence
            would dress their own guess up as the bank's. */}
        <p className="text-xs text-mist-500">
          {account.source === 'manual' ? 'Added manually' : account.institution_name}
          {account.source === 'plaid' &&
            account.subtype &&
            ` · reported as "${account.subtype}"`}
        </p>
      </div>

      <div>
        <label className="label" htmlFor={`tax-${account.id}`}>
          Tax treatment
        </label>
        <select
          id={`tax-${account.id}`}
          className="field"
          value={choice}
          onChange={(e) => setChoice(e.target.value as TaxTreatment | '')}
        >
          <option value="">Choose…</option>
          {TAX_TREATMENTS.map((t) => (
            <option key={t.value} value={t.value}>
              {t.label}
              {t.value === account.suggested_tax_treatment ? ' (likely)' : ''}
            </option>
          ))}
        </select>
      </div>

      {isCustodial && (
        <div>
          <label className="label" htmlFor={`ben-${account.id}`}>
            Held for
          </label>
          <select
            id={`ben-${account.id}`}
            className="field"
            value={beneficiary}
            onChange={(e) => setBeneficiary(e.target.value)}
          >
            <option value="">Nobody in particular</option>
            {people.data?.map((p) => (
              <option key={p.id} value={p.id}>
                {p.display_name}
              </option>
            ))}
          </select>
        </div>
      )}

      <label className="flex items-center gap-2 pb-3 text-sm text-mist-300">
        <input
          type="checkbox"
          className="accent-arcane-500"
          checked={managed}
          onChange={(e) => setManaged(e.target.checked)}
        />
        Managed
      </label>

      <button
        type="button"
        className="btn-primary mb-0.5"
        disabled={choice === '' || save.isPending}
        onClick={() => save.mutate()}
      >
        {save.isPending ? 'Saving…' : 'Save'}
      </button>

      {isCustodial && (
        <p className="w-full text-xs text-mist-500">
          Held for a dependent, so this balance stays out of the household's
          retirement total. It is still counted in net worth.
        </p>
      )}

      {save.isError && (
        <p role="alert" className="w-full text-sm text-ember-400">
          {save.error.message}
        </p>
      )}
    </li>
  )
}

function Disclosure({ children }: { children: React.ReactNode }) {
  return (
    <p className="rounded-xl border border-white/10 bg-white/5 px-4 py-3 text-sm text-mist-300">
      {children}
    </p>
  )
}

function Tile({
  label,
  value,
  sub,
  tone,
  large,
  format,
  fallback = '—',
}: {
  label: string
  value: string | number | null | undefined
  sub?: string
  tone?: 'good' | 'debt'
  large?: boolean
  format?: (n: number) => string
  fallback?: string
}) {
  const color = tone === 'debt' ? STATUS.critical : tone === 'good' ? STATUS.good : '#f2d492'
  return (
    <div className="glass p-5">
      <p className="text-sm text-mist-300">{label}</p>
      <p
        className={`tabular mt-2 font-semibold ${large ? 'text-4xl' : 'text-3xl'}`}
        style={{ color }}
      >
        <AnimatedNumber value={value} format={format} fallback={fallback} />
      </p>
      {sub && <p className="mt-1 text-xs text-mist-500">{sub}</p>}
    </div>
  )
}

function Stat({
  label,
  value,
  sub,
  tone,
  format,
  fallback = '—',
}: {
  label: string
  value: string | number | null | undefined
  sub?: string
  tone?: 'good' | 'debt'
  format?: (n: number) => string
  fallback?: string
}) {
  const color = tone === 'debt' ? STATUS.critical : tone === 'good' ? STATUS.good : undefined
  return (
    <div className="rounded-xl bg-white/5 px-4 py-3.5">
      <p className="text-xs tracking-wide text-mist-500 uppercase">{label}</p>
      <p className="tabular mt-1.5 text-2xl font-semibold" style={{ color }}>
        <AnimatedNumber value={value} format={format} fallback={fallback} />
      </p>
      {sub && <p className="mt-1 text-xs text-mist-500">{sub}</p>}
    </div>
  )
}

/** Trims trailing zeros from a share quantity: "213.0000000000" -> "213". */
function trimQuantity(q: string): string {
  if (!q.includes('.')) return q
  return q.replace(/\.?0+$/, '')
}

/**
 * The accounts on this page that Plaid does not maintain.
 *
 * Everything above this reads the same tables regardless of where a position
 * came from — that is the whole design, and it is why a manual Voya plan shows
 * up in the allocation and return figures without a single engine knowing about
 * it. This section is the one place the difference surfaces, because it is the
 * one place it matters: nothing will update these numbers except the household.
 */
function ManualInvestmentAccounts({ accounts }: { accounts: InvestmentAccount[] }) {
  if (accounts.length === 0) return null

  return (
    <section className="glass p-6">
      <h2 className="text-lg font-medium">Accounts you maintain</h2>
      <p className="mt-1 text-sm text-mist-300">
        These are not linked to an institution, so their positions and activity
        are whatever you have entered. Everything above counts them the same as
        a synced account.
      </p>
      <ul className="mt-5 space-y-4">
        {accounts.map((a) => (
          <li key={a.id} className="rounded-xl bg-white/5 px-4 py-3">
            <div className="flex flex-wrap items-baseline gap-2">
              <p className="font-medium">
                {a.name}
                {a.mask && <span className="text-mist-500"> ••{a.mask}</span>}
              </p>
              <span className="text-xs text-mist-500">
                {treatmentLabel(a.tax_treatment)}
              </span>
              <p className="tabular ml-auto font-medium text-rune-300">
                {formatMoney(a.balance, a.currency)}
              </p>
            </div>
            <ManualInvestmentEditor account={a} />
          </li>
        ))}
      </ul>
    </section>
  )
}
