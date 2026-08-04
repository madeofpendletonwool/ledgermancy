import { useState } from 'react'
import type { DividendMonth } from '../../lib/api'
import { formatMoney } from '../../lib/money'
import { axisTicks, compactMoney, labelStride } from './scale'
import { CHART, SERIES } from './tokens'

const WIDTH = 760
const HEIGHT = 220
const PAD = { top: 16, right: 16, bottom: 26, left: 56 }

const PLOT_W = WIDTH - PAD.left - PAD.right
const PLOT_H = HEIGHT - PAD.top - PAD.bottom

const BAR_GAP = 2
const BAR_RADIUS = 4

/**
 * Dividend income by month, as bars.
 *
 * Dividends are income, so the bars wear the income slot's blue. ONE series
 * across a categorical (month) dimension, so every bar carries the same colour.
 *
 * Months are discrete buckets and dividends are often quarterly, so empty
 * months are the norm rather than gaps in the data: the axis is rebuilt across
 * the full first..last span so a quiet month renders as an honest gap rather
 * than being dropped and silently compressing two payouts into adjacent bars.
 */
export function DividendBars({ months }: { months: DividendMonth[] }) {
  const [active, setActive] = useState<number | null>(null)

  if (months.length === 0) {
    return (
      <p className="py-8 text-center text-sm" style={{ color: CHART.textMuted }}>
        No dividend transactions reported yet.
      </p>
    )
  }

  // The full month axis across the data's span, so empty months show as gaps.
  const axis = monthsBetween(months[0].month, months[months.length - 1].month)
  const byMonth = new Map(months.map((m) => [m.month, m]))

  const values = axis.map((key) => Number(byMonth.get(key)?.total ?? 0))
  const { ticks, niceMax } = axisTicks(Math.max(...values, 0))

  const band = PLOT_W / Math.max(axis.length, 1)
  const barW = Math.max(band - BAR_GAP, 1)
  const x = (i: number) => PAD.left + (i + 0.5) * band
  const y = (v: number) =>
    PAD.top + PLOT_H - (niceMax > 0 ? (v / niceMax) * PLOT_H : 0)

  const stride = labelStride(axis.length)
  const activeKey = active !== null ? axis[active] : null
  const activeRow = activeKey ? byMonth.get(activeKey) : undefined

  return (
    <div className="relative overflow-x-auto">
      <svg
        viewBox={`0 0 ${WIDTH} ${HEIGHT}`}
          className="w-full max-sm:min-w-0 sm:min-w-[560px]"
        role="img"
        aria-label="Dividends received per month"
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

        {/* Month labels, thinned so they never collide. */}
        {axis.map((key, i) =>
          i % stride === 0 ? (
            <text
              key={key}
              x={x(i)}
              y={HEIGHT - 8}
              textAnchor="middle"
              fontSize="11"
              fill={CHART.textMuted}
            >
              {shortMonth(key)}
            </text>
          ) : null,
        )}

        {/* Bars. Data-end rounded, baseline square, so each sits ON the axis. */}
        {axis.map((key, i) => {
          const v = values[i]
          if (v <= 0) return null
          const top = y(v)
          const h = PAD.top + PLOT_H - top
          return (
            <path
              key={key}
              d={barPath(x(i) - barW / 2, top, barW, h)}
              fill={SERIES.income}
              opacity={active === null || active === i ? 0.9 : 0.45}
            />
          )
        })}

        {/* Invisible per-month hit targets, full band wide. */}
        {axis.map((key, i) => (
          <rect
            key={key}
            x={x(i) - band / 2}
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
          className="pointer-events-none absolute top-2 rounded-xl border border-white/10 bg-ink-900/95 px-3 py-2 text-xs shadow-xl backdrop-blur"
          style={{
            left: `${(x(active) / WIDTH) * 100}%`,
            transform: 'translateX(-50%)',
          }}
        >
          <p className="mb-1 font-medium text-mist-100">{shortMonth(axis[active], true)}</p>
          <p className="tabular text-mist-300">
            {activeRow ? formatMoney(activeRow.total) : 'No dividends'}
          </p>
        </div>
      )}
    </div>
  )
}

/**
 * A vertical bar rounded at the top only, square on the baseline. The radius
 * shrinks for a short or narrow bar so a small month renders as a small bar
 * rather than a lozenge.
 */
function barPath(x: number, y: number, w: number, h: number): string {
  const r = Math.max(0, Math.min(BAR_RADIUS, w / 2, h))
  const bottom = y + h
  if (r === 0) return `M${x} ${y}H${x + w}V${bottom}H${x}Z`
  return [
    `M${x} ${bottom}`,
    `V${y + r}`,
    `Q${x} ${y} ${x + r} ${y}`,
    `H${x + w - r}`,
    `Q${x + w} ${y} ${x + w} ${y + r}`,
    `V${bottom}`,
    'Z',
  ].join(' ')
}

/**
 * Every "YYYY-MM" from `from` to `to` inclusive. Built from the string's
 * calendar parts rather than by parsing to a Date, so a UTC wire value cannot
 * shift a month backwards in a negative-offset timezone.
 */
function monthsBetween(from: string, to: string): string[] {
  const [fy, fm] = from.split('-').map(Number)
  const [ty, tm] = to.split('-').map(Number)
  if (!fy || !fm || !ty || !tm) return []

  const out: string[] = []
  let y = fy
  let m = fm
  while ((y < ty || (y === ty && m <= tm)) && out.length < 600) {
    out.push(`${y}-${String(m).padStart(2, '0')}`)
    m += 1
    if (m > 12) {
      m = 1
      y += 1
    }
  }
  return out
}

function shortMonth(key: string, long = false): string {
  const [y, m] = key.split('-').map(Number)
  return new Date(y, m - 1, 1).toLocaleDateString('en-US', {
    month: 'short',
    year: long ? 'numeric' : '2-digit',
  })
}
