import { useState } from 'react'
import { motion, useReducedMotion } from 'motion/react'
import { formatMoney } from '../../lib/money'
import { barPath } from './motion'
import { axisTicks, compactMoney, labelStride } from './scale'
import { CHART, SINGLE_SERIES } from './tokens'
import { ChartBoundary } from './ChartBoundary'

const WIDTH = 760
const HEIGHT = 220
const PAD = { top: 16, right: 16, bottom: 26, left: 56 }

const PLOT_W = WIDTH - PAD.left - PAD.right
const PLOT_H = HEIGHT - PAD.top - PAD.bottom

/** A 2px surface gap between adjacent bars, so fills never touch. */
const BAR_GAP = 2

export type MonthlyPoint = {
  /** "YYYY-MM-DD", the first of the month. */
  month: string
  total: string
  transaction_count: number
}

/**
 * Spend per month at one merchant, as bars.
 *
 * Bars rather than a line because the months are discrete buckets and, at a
 * single merchant, an empty month is the norm rather than a gap in the data — a
 * line would draw a slope through a month with no visit and imply spending that
 * did not happen.
 *
 * ONE series, so every bar carries the same colour: hue is not encoding anything
 * here, and colouring bars individually would imply it was.
 *
 * `months` holds only the months that had spend; the axis is rebuilt across the
 * requested range so quiet months render as gaps rather than being dropped and
 * silently compressing the timeline.
 */
function MonthlyBarsUnguarded({
  months,
  from,
  to,
  onSelect,
  label = 'Spend per month at this merchant',
}: {
  months: MonthlyPoint[]
  /** Range start, "YYYY-MM-DD". */
  from: string
  /** Range end, "YYYY-MM-DD". */
  to: string
  /** Clicking a month calls this with its "YYYY-MM-DD" first-of-month. */
  onSelect?: (month: string) => void
  /**
   * The chart's accessible name. Defaulted to the merchant wording this chart was
   * built for, so the category view can say what it is actually showing instead of
   * telling a screen reader about a merchant that is not on the page.
   */
  label?: string
}) {
  const [active, setActive] = useState<number | null>(null)
  const reduce = useReducedMotion() ?? false

  const axis = monthsBetween(from, to)
  const byMonth = new Map(months.map((m) => [m.month.slice(0, 7), m]))

  const values = axis.map((key) => Number(byMonth.get(key)?.total ?? 0))
  const { ticks, niceMax } = axisTicks(Math.max(...values, 0))

  const band = PLOT_W / Math.max(axis.length, 1)
  const barW = Math.max(band - BAR_GAP, 1)
  const x = (i: number) => PAD.left + (i + 0.5) * band
  const y = (v: number) =>
    PAD.top + PLOT_H - (niceMax > 0 ? (v / niceMax) * PLOT_H : 0)

  const stride = labelStride(axis.length)

  if (axis.length === 0) {
    return <p className="text-sm text-mist-400">No months in this range.</p>
  }

  const activeMonth = active !== null ? axis[active] : null
  const activeRow = activeMonth ? byMonth.get(activeMonth) : undefined

  return (
    <div className="relative overflow-x-auto">
      <svg
        viewBox={`0 0 ${WIDTH} ${HEIGHT}`}
          className="w-full max-sm:min-w-0 sm:min-w-[560px]"
        role="img"
        aria-label={label}
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

        {/* Bars. The data-end is rounded and the baseline end is square, so every
            bar sits ON the axis — an rx on all four corners lifts the fill off the
            zero line and reads as a floating mark. */}
        {axis.map((key, i) => {
          const v = values[i]
          if (v <= 0) return null
          const top = y(v)
          const h = PAD.top + PLOT_H - top
          return (
            <motion.path
              key={key}
              d={barPathGeo(x(i) - barW / 2, top, barW, h)}
              fill={SINGLE_SERIES}
              opacity={active === null || active === i ? 0.9 : 0.45}
              style={{ transformBox: 'fill-box', originY: 1 }}
              {...barPath(barPathGeo(x(i) - barW / 2, top, barW, h), reduce)}
            />
          )
        })}

        {/* Invisible per-month hit targets, a full band wide so the target is
            always bigger than the bar. */}
        {axis.map((key, i) => (
          <rect
            key={key}
            x={x(i) - band / 2}
            y={PAD.top}
            width={band}
            height={PLOT_H}
            fill="transparent"
            style={{ cursor: onSelect ? 'pointer' : 'default' }}
            onMouseEnter={() => setActive(i)}
            onClick={onSelect ? () => onSelect(`${key}-01`) : undefined}
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
            {shortMonth(axis[active], true)}
          </p>
          <p className="tabular text-mist-300">
            {activeRow ? formatMoney(activeRow.total) : 'No spend'}
          </p>
          {activeRow && (
            <p className="text-mist-500">
              {activeRow.transaction_count} charge
              {activeRow.transaction_count === 1 ? '' : 's'}
            </p>
          )}
        </div>
      )}
    </div>
  )
}

/** Bar radius at the data-end. */
const BAR_RADIUS = 4

/**
 * A vertical bar rounded at the top only, square on the baseline.
 *
 * The radius shrinks for a bar shorter or narrower than it, so a small month
 * renders as a small bar rather than a lozenge.
 */
function barPathGeo(x: number, y: number, w: number, h: number): string {
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
 * Every "YYYY-MM" from `from` to `to` inclusive. Built from the string's calendar
 * parts rather than by parsing to a Date, so a UTC wire value cannot shift a month
 * backwards in a negative-offset timezone.
 */
function monthsBetween(from: string, to: string): string[] {
  const [fy, fm] = from.split('-').map(Number)
  const [ty, tm] = to.split('-').map(Number)
  if (!fy || !fm || !ty || !tm) return []

  const out: string[] = []
  let y = fy
  let m = fm
  // A hard ceiling: a corrupt range must not spin forever.
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

// The export is the guarded chart: a throw inside costs the reader the chart,
// not the page (MAD-61).
export function MonthlyBars(props: Parameters<typeof MonthlyBarsUnguarded>[0]) {
  return (
    <ChartBoundary label="monthly totals">
      <MonthlyBarsUnguarded {...props} />
    </ChartBoundary>
  )
}
