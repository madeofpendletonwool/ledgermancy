import { useState } from 'react'
import type { TrendPoint } from '../../lib/api'
import { formatDate, formatMoney } from '../../lib/money'
import { labelStride } from './scale'
import { CHART, SERIES } from './tokens'
import { ChartBoundary } from './ChartBoundary'

const WIDTH = 760
const HEIGHT = 220
const PAD = { top: 16, right: 16, bottom: 28, left: 48 }

const PLOT_W = WIDTH - PAD.left - PAD.right
const PLOT_H = HEIGHT - PAD.top - PAD.bottom

/**
 * Savings rate (leftover ÷ income) over the trailing months.
 *
 * A single derived measure across a time dimension: ONE series, so one colour.
 * Savings rate is the leftover expressed as a fraction of income, so it wears
 * the leftover slot's green — the same series the page already uses, normalised.
 *
 * Months with no income have no ratio, so they are dropped from the line rather
 * than plotted as a misleading zero or a clipped 100%. Their neighbours are not
 * connected across the gap: drawing a slope through a month with no income
 * would imply a rate that was never measured.
 *
 * THE MONTH IN PROGRESS IS DROPPED FOR THE SAME REASON. A household paid twice a
 * month that took a trip on the 3rd has, on the 12th, a whole month of spending
 * over one paycheck — or over none. The division still returns a number; it just
 * is not this month's savings rate, because this month's savings rate is not a
 * measurable quantity yet. Plotted, it produced -53,702% and an axis fitted to it
 * flattened the other eleven months into a line on the floor (MAD-110).
 *
 * Dropping it is the honest move rather than the convenient one: the point is
 * not hidden, it is reported as a state — the "so far this month" line under the
 * chart says what HAS happened, in dollars, which is the part that is real.
 */
function SavingsRateChartUnguarded({ data }: { data: TrendPoint[] }) {
  const [active, setActive] = useState<number | null>(null)

  // Map each month to a rate, keeping the original index so the x positions
  // stay aligned with the months even when a gap is dropped.
  const points = data
    .map((d, i) => {
      const income = Number(d.income)
      if (income <= 0 || d.in_progress) return null
      return { i, month: d.month, income, spending: Number(d.spending), rate: Number(d.leftover) / income }
    })
    .filter((p): p is NonNullable<typeof p> => p !== null)

  // The month in flight, reported beside the line rather than on it.
  const running = data.find((d) => d.in_progress) ?? null

  if (points.length === 0) {
    return (
      <div className="py-12 text-center text-sm" style={{ color: CHART.textMuted }}>
        <p>No completed month with income yet — the savings rate appears once one closes.</p>
        {running && <RunningMonth point={running} />}
      </div>
    )
  }

  // Percentage ticks: snap the range to round steps so the axis reads in whole
  // percents rather than 12.5% / 37.5%.
  const rawMax = Math.max(...points.map((p) => p.rate), 0)
  const rawMin = Math.min(...points.map((p) => p.rate), 0)
  const { ticks, min, max } = pctRange(rawMin, rawMax)

  const x = (i: number) =>
    data.length === 1
      ? PAD.left + PLOT_W / 2
      : PAD.left + (i / (data.length - 1)) * PLOT_W
  const y = (rate: number) =>
    PAD.top + PLOT_H - (max > min ? ((rate - min) / (max - min)) * PLOT_H : PLOT_H / 2)

  // Segments only between consecutive plottable points, so a dropped month
  // leaves an honest gap rather than a bridge across it.
  const segments: { from: typeof points[number]; to: typeof points[number] }[] = []
  for (let i = 1; i < points.length; i++) {
    if (points[i].i === points[i - 1].i + 1) {
      segments.push({ from: points[i - 1], to: points[i] })
    }
  }

  const activePoint = active !== null ? points.find((p) => p.i === active) ?? null : null

  return (
    <div className="space-y-3">
      <div className="relative overflow-x-auto">
        <svg
          viewBox={`0 0 ${WIDTH} ${HEIGHT}`}
            className="w-full max-sm:min-w-0 sm:min-w-[560px]"
          role="img"
          aria-label="Savings rate by month"
          onMouseLeave={() => setActive(null)}
        >
          {/* Recessive grid + percentage labels. */}
          {ticks.map((t) => (
            <g key={t}>
              <line
                x1={PAD.left}
                x2={WIDTH - PAD.right}
                y1={y(t)}
                y2={y(t)}
                stroke={t === 0 ? CHART.axis : CHART.grid}
                strokeWidth={t === 0 ? 1.25 : 1}
              />
              <text
                x={PAD.left - 8}
                y={y(t) + 4}
                textAnchor="end"
                fontSize="11"
                fill={t === 0 ? CHART.textSecondary : CHART.textMuted}
              >
                {Math.round(t * 100)}%
              </text>
            </g>
          ))}

          {/* The month in flight, shaded out of the plot: the line genuinely has
              no value there, and an unexplained empty slot at the right edge reads
              as missing data rather than as a month that has not finished. */}
          {running && (
            <rect
              x={x(data.indexOf(running)) - PLOT_W / Math.max(data.length, 1) / 2}
              y={PAD.top}
              width={PLOT_W / Math.max(data.length, 1)}
              height={PLOT_H}
              fill={CHART.grid}
              opacity={0.5}
            />
          )}

          {/* Month labels, thinned so they never collide. The in-progress month is
              always labelled regardless of the stride — it is the one column the
              reader needs named to understand the gap beside it. */}
          {data.map((d, i) =>
            i % labelStride(data.length) === 0 || d.in_progress ? (
              <text
                key={d.month}
                x={x(i)}
                y={HEIGHT - 8}
                textAnchor="middle"
                fontSize="11"
                fill={CHART.textMuted}
                fontStyle={d.in_progress ? 'italic' : undefined}
              >
                {monthLabel(d.month)}
              </text>
            ) : null,
          )}

          {/* Crosshair for the hovered month. */}
          {active !== null && (
            <line
              x1={x(active)}
              x2={x(active)}
              y1={PAD.top}
              y2={PAD.top + PLOT_H}
              stroke={CHART.axis}
              strokeWidth={1}
            />
          )}

          {/* The line itself. */}
          <path
            d={segments
              .map((s) => `M ${x(s.from.i)} ${y(s.from.rate)} L ${x(s.to.i)} ${y(s.to.rate)}`)
              .join(' ')}
            fill="none"
            stroke={SERIES.leftover}
            strokeWidth={2}
          />

          {/* Markers on the hovered month. */}
          {activePoint && (
            <circle
              cx={x(activePoint.i)}
              cy={y(activePoint.rate)}
              r={5}
              fill={SERIES.leftover}
              stroke={CHART.surface}
              strokeWidth={2}
            />
          )}

          {/* Invisible hit targets, one per month with a rate, band-wide. */}
          {data.map((d, i) => {
            const has = points.some((p) => p.i === i)
            if (!has) return null
            return (
              <rect
                key={d.month}
                x={x(i) - PLOT_W / Math.max(data.length, 1) / 2}
                y={PAD.top}
                width={PLOT_W / Math.max(data.length, 1)}
                height={PLOT_H}
                fill="transparent"
                onMouseEnter={() => setActive(i)}
              />
            )
          })}
        </svg>

        {activePoint && (
          <div
            className="pointer-events-none absolute top-2 rounded-xl border border-white/10 bg-ink-900/95 px-3 py-2 text-xs shadow-xl backdrop-blur"
            style={{
              left: `${(x(activePoint.i) / WIDTH) * 100}%`,
              transform: 'translateX(-50%)',
            }}
          >
            <p className="mb-1 font-medium text-mist-100">{monthLabel(activePoint.month, true)}</p>
            <p className="tabular text-mist-300">
              Savings rate{' '}
              <span className="ml-1 text-mist-100">
                {(activePoint.rate * 100).toFixed(1)}%
              </span>
            </p>
            <p className="text-mist-500">
              {formatMoney(String(activePoint.income))} in ·{' '}
              {formatMoney(String(activePoint.spending))} out
            </p>
          </div>
        )}
      </div>
      {running && <RunningMonth point={running} />}
    </div>
  )
}

/**
 * What the in-progress month HAS done, in dollars, since its rate is not yet a
 * thing that exists. Dollars in and dollars out are both exact and both real —
 * only their ratio is premature — so the reader loses nothing but the number
 * that would have lied to them.
 *
 * Deliberately not a projection. Nothing here extrapolates the month's income
 * from the paychecks that have landed or its spending from the days elapsed:
 * this app's rule is that a projection is labelled arithmetic, never a figure
 * dressed as a measurement.
 */
function RunningMonth({ point }: { point: TrendPoint }) {
  const leftover = Number(point.leftover)
  return (
    <p className="text-xs" style={{ color: CHART.textMuted }}>
      <span style={{ color: CHART.textSecondary }}>
        {monthLabel(point.month, true)} is still running
      </span>
      {point.as_of ? ` (through ${formatDate(point.as_of)})` : ''} —{' '}
      {formatMoney(point.income)} in, {formatMoney(point.spending)} out,{' '}
      {formatMoney(String(Math.abs(leftover)))} {leftover < 0 ? 'short' : 'left'} so far. Its
      rate joins the line when the month closes.
    </p>
  )
}

/**
 * Percentage tick range that always contains zero and lands on round steps.
 * Returns fractions (0.1 = 10%) so the caller can render them as percent.
 */
function pctRange(rawMin: number, rawMax: number) {
  const step = nicePctStep(Math.max(Math.abs(rawMin), Math.abs(rawMax)) / 4)
  const min = Math.floor(rawMin / step) * step
  const max = Math.ceil(rawMax / step) * step

  const ticks: number[] = []
  for (let t = min; t <= max + step / 2; t += step) ticks.push(Math.round(t * 100) / 100)
  return { ticks, min, max }
}

/** Snaps a rough fractional step to a round percentage multiple. */
function nicePctStep(rough: number): number {
  const magnitude = 10 ** Math.floor(Math.log10(Math.max(rough, 0.001)))
  for (const m of [1, 2, 2.5, 5, 10]) {
    if (magnitude * m >= rough) return magnitude * m
  }
  return magnitude * 10
}

function monthLabel(month: string, long = false): string {
  const [year, m] = month.split('-').map(Number)
  return new Date(year, m - 1, 1).toLocaleDateString('en-US', {
    month: 'short',
    year: long ? 'numeric' : '2-digit',
  })
}

// The export is the guarded chart: a throw inside costs the reader the chart,
// not the page (MAD-61).
export function SavingsRateChart(props: Parameters<typeof SavingsRateChartUnguarded>[0]) {
  return (
    <ChartBoundary label="savings rate">
      <SavingsRateChartUnguarded {...props} />
    </ChartBoundary>
  )
}
