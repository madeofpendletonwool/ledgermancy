import { useState } from 'react'
import type { DaySpend } from '../../lib/api'
import { formatMoney } from '../../lib/money'
import { CHART, SERIES } from './tokens'

const WIDTH = 760
const HEIGHT = 220
const PAD = { top: 16, right: 16, bottom: 26, left: 56 }

const PLOT_W = WIDTH - PAD.left - PAD.right
const PLOT_H = HEIGHT - PAD.top - PAD.bottom

/**
 * Spend per day for one month, as bars, with a dashed reference line at last
 * month's average daily spend. Both are dollars-per-day, so they share one
 * axis — the "are we running hot?" read is bar height against that line. The
 * cumulative month-to-date-vs-last-month verdict lives in the card header,
 * where a running total belongs; putting a cumulative line on this daily axis
 * would mix two scales.
 *
 * `days` holds only the days that had spend; the full 1..N axis is rebuilt here
 * so empty days render as gaps rather than being dropped.
 */
export function DayBars({
  year,
  month,
  days,
  lastMonthAvgDaily,
}: {
  /** Full year, e.g. 2026. */
  year: number
  /** 1-based month, 1 = January. */
  month: number
  days: DaySpend[]
  /** Last month's mean spend per day, for the reference line. */
  lastMonthAvgDaily: number
}) {
  const [active, setActive] = useState<number | null>(null)

  const daysInMonth = new Date(year, month, 0).getDate()
  const today = new Date()
  const isCurrentMonth =
    today.getFullYear() === year && today.getMonth() + 1 === month
  const todayDom = isCurrentMonth ? today.getDate() : daysInMonth

  // Index daily spend by day-of-month (1-based). dayOfMonth reads the calendar
  // parts directly — the wire value is "YYYY-MM-DD", so no timezone shift.
  const daily = new Array<number>(daysInMonth + 1).fill(0)
  for (const d of days) {
    const dom = Number(d.day.slice(8, 10))
    if (dom >= 1 && dom <= daysInMonth) daily[dom] = Number(d.spending)
  }

  const max = Math.max(...daily, lastMonthAvgDaily, 0)
  const { ticks, niceMax } = axisTicks(max)

  const band = PLOT_W / daysInMonth
  const barW = band * 0.6
  const x = (dom: number) => PAD.left + (dom - 0.5) * band
  const y = (v: number) =>
    PAD.top + PLOT_H - (niceMax > 0 ? (v / niceMax) * PLOT_H : 0)

  const refY = y(lastMonthAvgDaily)
  const activeVal = active !== null ? daily[active] : 0

  return (
    <div className="space-y-3">
      <div className="flex items-center gap-5 text-xs">
        <span
          className="flex items-center gap-1.5"
          style={{ color: CHART.textSecondary }}
        >
          <span
            className="inline-block h-2.5 w-2.5 rounded-sm"
            style={{ backgroundColor: SERIES.spending }}
          />
          Spend / day
        </span>
        {lastMonthAvgDaily > 0 && (
          <span
            className="flex items-center gap-1.5"
            style={{ color: CHART.textSecondary }}
          >
            <span
              className="inline-block h-0 w-4 border-t border-dashed"
              style={{ borderColor: CHART.textMuted }}
            />
            Last month avg / day
          </span>
        )}
      </div>

      <div className="relative overflow-x-auto">
        <svg
          viewBox={`0 0 ${WIDTH} ${HEIGHT}`}
          className="w-full min-w-[560px]"
          role="img"
          aria-label="Spend by day this month"
          onMouseLeave={() => setActive(null)}
        >
          {/* Recessive grid + y labels. */}
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

          {/* Day-of-month labels, every fifth day plus the 1st. */}
          {Array.from({ length: daysInMonth }, (_, i) => i + 1).map((dom) =>
            dom === 1 || dom % 5 === 0 ? (
              <text
                key={dom}
                x={x(dom)}
                y={HEIGHT - 8}
                textAnchor="middle"
                fontSize="11"
                fill={CHART.textMuted}
              >
                {dom}
              </text>
            ) : null,
          )}

          {/* Bars. Days still to come (in the current month) are drawn empty. */}
          {Array.from({ length: daysInMonth }, (_, i) => i + 1).map((dom) => {
            const v = daily[dom]
            const future = dom > todayDom
            const h = v > 0 ? PAD.top + PLOT_H - y(v) : 0
            return (
              <rect
                key={dom}
                x={x(dom) - barW / 2}
                y={y(v)}
                width={barW}
                height={h}
                rx={1.5}
                fill={SERIES.spending}
                opacity={future ? 0 : active === null || active === dom ? 0.9 : 0.4}
              />
            )
          })}

          {/* Last month's average daily spend — the pace reference. */}
          {lastMonthAvgDaily > 0 && (
            <line
              x1={PAD.left}
              x2={WIDTH - PAD.right}
              y1={refY}
              y2={refY}
              stroke={CHART.textMuted}
              strokeWidth={1.5}
              strokeDasharray="5 4"
            />
          )}

          {/* Invisible per-day hit targets, full band wide. */}
          {Array.from({ length: daysInMonth }, (_, i) => i + 1).map((dom) => (
            <rect
              key={dom}
              x={x(dom) - band / 2}
              y={PAD.top}
              width={band}
              height={PLOT_H}
              fill="transparent"
              onMouseEnter={() => setActive(dom)}
            />
          ))}
        </svg>

        {active !== null && (
          <div
            className="pointer-events-none absolute top-2 rounded-xl border border-white/10 bg-ink-900/95 px-3 py-2 text-xs shadow-xl backdrop-blur"
            style={{
              left: `${(x(active) / WIDTH) * 100}%`,
              transform: 'translateX(-50%)',
            }}
          >
            <p className="mb-1 font-medium text-mist-100">
              {new Date(year, month - 1, active).toLocaleDateString('en-US', {
                month: 'short',
                day: 'numeric',
              })}
            </p>
            <p className="tabular text-mist-300">
              {activeVal > 0 ? formatMoney(String(activeVal)) : 'No spend'}
            </p>
          </div>
        )}
      </div>
    </div>
  )
}

/**
 * Ticks that land on round numbers, snapping the step to 1/2/2.5/5 × a power of
 * ten so labels read as an even sequence. Mirrors the approach in TrendChart.
 */
function axisTicks(max: number): { ticks: number[]; niceMax: number } {
  if (max <= 0) return { ticks: [0, 25, 50, 75, 100], niceMax: 100 }

  const rawStep = max / 4
  const magnitude = 10 ** Math.floor(Math.log10(rawStep))
  const normalized = rawStep / magnitude
  const niceStep =
    magnitude *
    (normalized <= 1 ? 1 : normalized <= 2 ? 2 : normalized <= 2.5 ? 2.5 : normalized <= 5 ? 5 : 10)

  const niceMax = Math.ceil(max / niceStep) * niceStep
  const ticks: number[] = []
  for (let v = 0; v <= niceMax + niceStep / 2; v += niceStep) ticks.push(v)
  return { ticks, niceMax }
}

function compactMoney(v: number): string {
  if (v === 0) return '$0'
  if (v >= 1000) {
    const k = v / 1000
    return `$${Number.isInteger(k) ? k : k.toFixed(1)}k`
  }
  return `$${Math.round(v)}`
}
