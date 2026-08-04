import { useState } from 'react'
import type { MerchantExplorerRow } from '../../lib/api'
import { formatMoney } from '../../lib/money'
import { axisTicks, compactMoney } from './scale'
import { CHART, SINGLE_SERIES } from './tokens'

const WIDTH = 760
const HEIGHT = 240
const PAD = { top: 18, right: 16, bottom: 70, left: 56 }

const PLOT_W = WIDTH - PAD.left - PAD.right
const PLOT_H = HEIGHT - PAD.top - PAD.bottom

const MAX_BARS = 8
const BAR_GAP = 3

/** The cumulative-share line: the same dollars expressed as a running total,
 *  so it recedes rather than competing with the bars. */
const CUM_COLOR = CHART.textSecondary

/**
 * Merchant concentration as a Pareto chart.
 *
 * One measure (dollars) across a categorical dimension (merchants): ONE series,
 * so every bar carries the same colour — merchant identity comes from the x
 * label, not hue. The cumulative-share line answers "50% of spend lands at how
 * many merchants?": it is the same dollars expressed as a running total, so it
 * is plotted on the SAME dollar axis (never a second % axis — that would let
 * the two reads cross wherever the scales were set and imply a relationship
 * that is not in the data). Percentage labels on the line and a 50% reference
 * gridline carry the share read without a second scale.
 *
 * Anything past the top handful folds into "Other", so the chart never grows
 * past its readable cap and the cumulative still reaches 100%.
 */
export function MerchantPareto({
  rows,
  windowTotal,
}: {
  rows: MerchantExplorerRow[]
  windowTotal: number
}) {
  const [active, setActive] = useState<number | null>(null)

  if (rows.length === 0 || windowTotal <= 0) return null

  const sorted = [...rows].sort((a, b) => Number(b.total) - Number(a.total))
  const bars = foldToOther(sorted, MAX_BARS)

  // Cumulative dollars through each bar; the last always reaches window_total
  // because "Other" is everyone else summed.
  let running = 0
  const cumulative = bars.map((b) => {
    running += Number(b.total)
    return running
  })

  const maxBar = Math.max(...bars.map((b) => Number(b.total)), 0)
  const { ticks, niceMax } = axisTicks(Math.max(maxBar, windowTotal, 0))

  const band = PLOT_W / Math.max(bars.length, 1)
  const barW = Math.max(band - BAR_GAP, 1)
  const xMid = (i: number) => PAD.left + (i + 0.5) * band
  // Cumulative points sit at each bar's right edge, so the line climbs through
  // the bars and reaches the top-right at 100%.
  const xRight = (i: number) => PAD.left + (i + 1) * band
  const y = (v: number) =>
    PAD.top + PLOT_H - (niceMax > 0 ? (v / niceMax) * PLOT_H : 0)

  // The line starts at zero on the left edge so the climb is legible.
  const cumPath = [
    `M ${PAD.left} ${y(0)}`,
    ...cumulative.map((c, i) => `L ${xRight(i)} ${y(c)}`),
  ].join(' ')

  const halfTotalY = y(windowTotal / 2)

  return (
    <div className="mb-5">
      <div className="chart-scroll relative overflow-x-auto">
        <span className="sr-only">This chart scrolls horizontally — swipe to see the rest.</span>
        <svg
          viewBox={`0 0 ${WIDTH} ${HEIGHT}`}
          className="w-full min-w-[600px]"
          role="img"
          aria-label="Merchant spend concentration with cumulative share"
          onMouseLeave={() => setActive(null)}
        >
          {/* Recessive grid + dollar labels. */}
          {ticks.map((t) => (
            <g key={t}>
              <line
                x1={PAD.left}
                x2={WIDTH - PAD.right}
                y1={y(t)}
                y2={y(t)}
                stroke={CHART.grid}
                strokeWidth={1}
              />
              <text
                x={PAD.left - 10}
                y={y(t) + 4}
                textAnchor="end"
                fontSize="11"
                fill={CHART.textMuted}
              >
                {compactMoney(t)}
              </text>
            </g>
          ))}

          {/* 50% reference line: where the cumulative crosses half of all
              spend. The read "how many merchants to reach 50%" is the whole
              point of a Pareto, so the threshold is drawn explicitly. */}
          <line
            x1={PAD.left}
            x2={WIDTH - PAD.right}
            y1={halfTotalY}
            y2={halfTotalY}
            stroke={CHART.axis}
            strokeWidth={1}
            strokeDasharray="4 4"
          />
          <text
            x={WIDTH - PAD.right}
            y={halfTotalY - 4}
            textAnchor="end"
            fontSize="10"
            fill={CHART.textMuted}
          >
            50% of spend
          </text>

          {/* Bars. Single series, so one colour; the hovered bar is full
              strength and the rest dim. */}
          {bars.map((b, i) => {
            const v = Number(b.total)
            const top = y(v)
            const h = PAD.top + PLOT_H - top
            return (
              <rect
                key={b.merchant_key}
                x={xMid(i) - barW / 2}
                y={top}
                width={barW}
                height={Math.max(h, 0)}
                rx={2}
                fill={SINGLE_SERIES}
                opacity={active === null || active === i ? 0.9 : 0.45}
              />
            )
          })}

          {/* Cumulative-share line, on the same dollar axis. */}
          <path d={cumPath} fill="none" stroke={CUM_COLOR} strokeWidth={1.5} />
          {cumulative.map((c, i) => (
            <circle
              key={bars[i].merchant_key}
              cx={xRight(i)}
              cy={y(c)}
              r={3}
              fill={CUM_COLOR}
              stroke={CHART.surface}
              strokeWidth={1.5}
            />
          ))}

          {/* Percentage labels on the line — the share read without a second
              axis. Tucked just above each point. */}
          {cumulative.map((c, i) => {
            const pct = Math.round((c / windowTotal) * 100)
            return (
              <text
                key={`pct-${bars[i].merchant_key}`}
                x={xRight(i)}
                y={y(c) - 6}
                textAnchor="middle"
                fontSize="10"
                fill={CHART.textSecondary}
              >
                {pct}%
              </text>
            )
          })}

          {/* Merchant labels, rotated so even long names fit under a bar. */}
          {bars.map((b, i) => (
            <text
              key={`lbl-${b.merchant_key}`}
              x={xMid(i)}
              y={PAD.top + PLOT_H + 12}
              textAnchor="end"
              fontSize="11"
              fill={active === i ? CHART.textPrimary : CHART.textMuted}
              transform={`rotate(-35 ${xMid(i)} ${PAD.top + PLOT_H + 12})`}
            >
              {b.merchant}
            </text>
          ))}

          {/* Invisible per-bar hit targets. */}
          {bars.map((b, i) => (
            <rect
              key={`hit-${b.merchant_key}`}
              x={xMid(i) - band / 2}
              y={PAD.top}
              width={band}
              height={PLOT_H}
              fill="transparent"
              onMouseEnter={() => setActive(i)}
            />
          ))}
        </svg>

        {active !== null && (
          <div
            className="pointer-events-none absolute top-2 left-1/2 rounded-xl border border-white/10 bg-ink-900/95 px-3 py-2 text-xs shadow-xl backdrop-blur"
            style={{ transform: 'translateX(-50%)' }}
          >
            <p className="mb-1 max-w-[16rem] truncate font-medium text-mist-100">
              {bars[active].merchant}
            </p>
            <p className="tabular text-mist-300">
              {formatMoney(bars[active].total)} ·{' '}
              {Math.round((Number(bars[active].total) / windowTotal) * 100)}% of spend
            </p>
            <p className="tabular text-mist-500">
              {Math.round((cumulative[active] / windowTotal) * 100)}% cumulative
            </p>
          </div>
        )}
      </div>

      <div className="mt-2 flex flex-wrap gap-4 text-xs" style={{ color: CHART.textMuted }}>
        <span className="flex items-center gap-1.5">
          <span
            className="inline-block h-2.5 w-2.5 rounded-sm"
            style={{ backgroundColor: SINGLE_SERIES }}
          />
          Spend per merchant
        </span>
        <span className="flex items-center gap-1.5">
          <span
            className="inline-block h-0.5 w-4"
            style={{ backgroundColor: CUM_COLOR }}
          />
          Cumulative share
        </span>
      </div>
    </div>
  )
}

/**
 * Keeps the top N merchants and sums the remainder into a single "Other" row,
 * so the chart never grows past its readable cap and the cumulative still
 * reaches 100%. Mirrors CategoryBars' fold.
 */
function foldToOther(rows: MerchantExplorerRow[], max: number): MerchantExplorerRow[] {
  if (rows.length <= max) return rows

  const head = rows.slice(0, max - 1)
  const tail = rows.slice(max - 1)

  const otherTotal = tail.reduce((sum, r) => sum + Number(r.total), 0)
  const otherCount = tail.reduce((sum, r) => sum + r.transaction_count, 0)

  return [
    ...head,
    {
      merchant: `Other (${tail.length} merchants)`,
      merchant_key: '__other__',
      total: otherTotal.toFixed(2),
      transaction_count: otherCount,
      average: '0',
      first_seen: tail[0]?.first_seen ?? '',
      last_seen: tail[0]?.last_seen ?? '',
      prior_total: tail.reduce((s, r) => s + Number(r.prior_total), 0).toFixed(2),
      is_new: false,
      category_id: null,
      category_name: null,
      category_color: null,
    },
  ]
}
