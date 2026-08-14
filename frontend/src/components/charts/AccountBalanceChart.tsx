import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { motion, useReducedMotion } from 'motion/react'
import { api } from '../../lib/api'
import { formatMoney } from '../../lib/money'
import { lineDraw } from './motion'
import { CHART, SINGLE_SERIES } from './tokens'
import { ChartBoundary } from './ChartBoundary'

/**
 * One account's balance over the past year.
 *
 * The per-account analog of the net-worth line: a single series, so one colour
 * and no legend. The domain is fit to the data with a margin rather than pinned
 * to zero — a line encodes value by position, not length, and a checking account
 * whose balance sits around $4,000 would flatten into the floor against a
 * zero-based axis the same way net worth did against a mortgage. Zero is still
 * pulled in whenever the data comes near it, so a card paying down into the
 * black does not hide the crossing.
 *
 * The honesty rule the rest of the app follows for dates applies here too:
 * `as_of` is a calendar DATE serialised as YYYY-MM-DD, so the axis labels read
 * the parts directly and never hand the string to `new Date()` (which would
 * render the previous day west of UTC and shift a balance into the wrong month).
 *
 * Balances have no history of their own — Plaid reports only the current
 * figure — so the line starts the day Ledgermancy began recording the account,
 * the same bound the net-worth chart carries. That is stated under the chart
 * rather than implied, because a flat opening month is the app's first month of
 * recording, not the account's real history.
 */
function AccountBalanceChartUnguarded({
  accountId,
  currency,
}: {
  accountId: string
  currency: string
}) {
  const reduce = useReducedMotion() ?? false

  // A one-year window. `to` is omitted so the endpoint bounds it to today; the
  // server also accepts an explicit `to`, matching /api/networth/history.
  const from = new Date()
  from.setFullYear(from.getFullYear() - 1)
  const fromStr = from.toISOString().slice(0, 10)

  const history = useQuery({
    queryKey: ['balance-history', accountId, fromStr],
    queryFn: () => api.accountBalanceHistory(accountId, { from: fromStr }),
  })

  const data = history.data ?? []

  if (data.length < 2) {
    return (
      <p className="py-6 text-center text-xs" style={{ color: CHART.textMuted }}>
        {data.length === 1
          ? 'One reading so far — the trend appears once there are at least two.'
          : 'No balance history yet. The trend begins once Ledgermancy has recorded this account for a day.'}
      </p>
    )
  }

  const W = 760
  const H = 200
  const PAD = { top: 12, right: 12, bottom: 24, left: 72 }
  const plotW = W - PAD.left - PAD.right
  const plotH = H - PAD.top - PAD.bottom

  const values = data.map((d) => Number(d.balance))
  const lo = Math.min(...values)
  const hi = Math.max(...values)

  // Fit the domain to the data with a margin, pulling zero in only when the
  // data comes near it. Same reasoning as NetWorthChart: pinning to zero
  // squashes a healthy checking account into the floor.
  const margin = (hi - lo || Math.abs(lo) || 1) * 0.15
  const nearZero = lo - margin <= 0 && hi + margin >= 0
  const min = nearZero ? Math.min(lo - margin, 0) : lo - margin
  const max = nearZero ? Math.max(hi + margin, 0) : hi + margin
  const span = max - min || 1

  const x = (i: number) => PAD.left + (i / (data.length - 1)) * plotW
  const y = (v: number) => PAD.top + plotH - ((v - min) / span) * plotH

  const path = data
    .map((d, i) => `${i === 0 ? 'M' : 'L'} ${x(i)} ${y(Number(d.balance))}`)
    .join(' ')

  return (
    <div>
      <div className="overflow-x-auto">
        <svg
          viewBox={`0 0 ${W} ${H}`}
          className="w-full max-sm:min-w-0 sm:min-w-[520px]"
          role="img"
          aria-label="Account balance over the past year"
        >
          {/* A zero line: a card or loan carries a negative balance by
              definition, and the sign is the most important thing on the chart.
              Drawn only when the domain actually crosses it. */}
          {min < 0 && max > 0 && (
            <line
              x1={PAD.left}
              x2={W - PAD.right}
              y1={y(0)}
              y2={y(0)}
              stroke={CHART.axis}
              strokeWidth={1}
              strokeDasharray="3 3"
            />
          )}
          <text
            x={PAD.left - 10}
            y={y(max) + 4}
            textAnchor="end"
            fontSize="11"
            fill={CHART.textMuted}
          >
            {formatMoney(String(max), currency)}
          </text>
          <text
            x={PAD.left - 10}
            y={y(min) + 4}
            textAnchor="end"
            fontSize="11"
            fill={CHART.textMuted}
          >
            {formatMoney(String(min), currency)}
          </text>
          <motion.path
            fill="none"
            stroke={SINGLE_SERIES}
            strokeWidth={2}
            {...lineDraw(path, reduce)}
          />
          {data.map((d, i) => (
            <circle
              key={d.as_of}
              cx={x(i)}
              cy={y(Number(d.balance))}
              r={3}
              fill={SINGLE_SERIES}
              stroke={CHART.surface}
              strokeWidth={1.5}
            >
              <title>{`${d.as_of}: ${formatMoney(d.balance, currency)}`}</title>
            </circle>
          ))}
          {/* Calendar dates read as YYYY-MM-DD; placed verbatim so no timezone
              conversion shifts them a day west of UTC. */}
          <text x={PAD.left} y={H - 6} fontSize="11" fill={CHART.textMuted}>
            {data[0].as_of}
          </text>
          <text
            x={W - PAD.right}
            y={H - 6}
            textAnchor="end"
            fontSize="11"
            fill={CHART.textMuted}
          >
            {data[data.length - 1].as_of}
          </text>
        </svg>
      </div>
      <p className="mt-1.5 text-xs" style={{ color: CHART.textMuted }}>
        Balances have no history of their own, so the line starts the day
        Ledgermancy began recording this account — not earlier.
      </p>
    </div>
  )
}

/**
 * The guarded export: a throw inside costs the reader the chart, not the page
 * (MAD-61). The list of accounts stays interactive even if one account's trend
 * fails to render.
 */
export function AccountBalanceChart(props: Parameters<typeof AccountBalanceChartUnguarded>[0]) {
  return (
    <ChartBoundary label="account balance trend">
      <AccountBalanceChartUnguarded {...props} />
    </ChartBoundary>
  )
}

/**
 * A collapsed "Balance trend" affordance that expands to the per-account line.
 *
 * Collapsed by default: this sits in a list of accounts, and unfolding a chart
 * under every row would turn the list into something else. Mirrors the
 * DebtTerms / DepositYield expanders on the same row, and carries the same
 * shape — an inline link when closed, the chart on a tinted panel when open —
 * so the affordance reads as one of the account row's expanders and not a new
 * kind of control. Shared by the linked-account rows and the manual-account
 * rows, since the read endpoint draws both trends the same way.
 */
export function BalanceTrend({
  accountId,
  currency,
}: {
  accountId: string
  currency: string
}) {
  const [open, setOpen] = useState(false)
  if (!open) {
    return (
      <div className="mt-1.5 text-xs text-mist-500">
        <button
          type="button"
          className="text-arcane-300 underline"
          onClick={() => setOpen(true)}
        >
          Balance trend
        </button>
      </div>
    )
  }
  return (
    <div className="mt-3 rounded-lg bg-white/[0.03] p-3">
      <div className="mb-2 flex items-center justify-between">
        <p className="text-xs font-medium text-mist-300">Balance over the past year</p>
        <button
          type="button"
          className="text-xs text-mist-400 underline"
          onClick={() => setOpen(false)}
        >
          Hide
        </button>
      </div>
      <AccountBalanceChart accountId={accountId} currency={currency} />
    </div>
  )
}
