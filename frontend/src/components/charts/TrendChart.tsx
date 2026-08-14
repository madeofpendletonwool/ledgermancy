import { useState } from 'react'
import { motion, useReducedMotion } from 'motion/react'
import type { TrendPoint } from '../../lib/api'
import { formatMoney } from '../../lib/money'
import { areaFade, lineDraw } from './motion'
import { axisTicks, compactMoney, labelStride } from './scale'
import { CHART, SERIES, STATUS } from './tokens'
import { ChartBoundary } from './ChartBoundary'
import { PartialMonthTooltipNote } from './partialMonth'

const WIDTH = 760
const HEIGHT = 260
const PAD = { top: 16, right: 16, bottom: 28, left: 64 }

const PLOT_W = WIDTH - PAD.left - PAD.right
const PLOT_H = HEIGHT - PAD.top - PAD.bottom

/**
 * Income against spending, month by month.
 *
 * Both series are dollars, so they share ONE y-axis — a second scale would let
 * the two lines cross wherever the axes happened to be set and imply
 * relationships that are not in the data.
 *
 * `real` swaps every figure for its base-period equivalent. It is done by
 * PROJECTING the points once, up front, rather than by teaching each of the
 * dozen read sites below to pick a field: one place to get it wrong instead of
 * twelve, and a month with no published index drops out of the projection
 * entirely rather than appearing on a real axis with a nominal value.
 */
function TrendChartUnguarded({
  data: input,
  real = false,
  avgLeftover,
}: {
  data: TrendPoint[]
  real?: boolean
  /**
   * A reference line for the mean leftover across the window, as a decimal
   * string.
   *
   * SERVER-COMPUTED, ALWAYS. It is passed in rather than derived from `data`
   * here on purpose: a chart's average, budget or target line is a finished
   * figure like every other money figure in this app, and one the client
   * averaged would be a second, subtly different answer to a question the
   * server has already answered exactly. Omitted when the caller has no such
   * figure — the chart then draws exactly as it always has.
   */
  avgLeftover?: string
}) {
  const [active, setActive] = useState<number | null>(null)
  const reduce = useReducedMotion() ?? false

  const data = real ? input.flatMap(toRealPoint) : input
  const dropped = input.length - data.length

  if (data.length === 0) {
    return (
      <p className="py-12 text-center text-sm" style={{ color: CHART.textMuted }}>
        {real && input.length > 0
          ? 'None of these months have a published price index, so there is nothing to show in real terms.'
          : 'Not enough history yet to chart a trend.'}
      </p>
    )
  }

  const values = data.flatMap((d) => [Number(d.income), Number(d.spending)])
  // Always include zero so line positions are not exaggerated by a truncated
  // axis.
  const max = Math.max(...values, 0)
  const { ticks, niceMax } = axisTicks(max)

  const x = (i: number) =>
    data.length === 1
      ? PAD.left + PLOT_W / 2
      : PAD.left + (i / (data.length - 1)) * PLOT_W
  const y = (v: number) =>
    PAD.top + PLOT_H - (niceMax > 0 ? (v / niceMax) * PLOT_H : 0)

  const line = (key: 'income' | 'spending') =>
    data.map((d, i) => `${i === 0 ? 'M' : 'L'} ${x(i)} ${y(Number(d[key]))}`).join(' ')

  /**
   * The area between income and spending IS the savings (or the shortfall),
   * so a faint fill between the two lines makes "did I save this month"
   * readable at a glance instead of by subtraction.
   *
   * One polygon per segment, coloured by that segment's `leftover` (which the
   * server already computes as income − spending): green where income sat above
   * spending, the critical red where it did not. The fill is low-opacity and
   * sits behind the lines, so the two series still carry the detail — the tint
   * only carries the sign of the gap. Same axis, same two series, no new slot.
   */
  const gapFills = data.slice(0, Math.max(data.length - 1, 0)).map((_, i) => {
    const d0 = data[i]
    const d1 = data[i + 1]
    const saved = Number(d0.leftover) + Number(d1.leftover) >= 0
    const pts = [
      `${x(i)},${y(Number(d0.income))}`,
      `${x(i + 1)},${y(Number(d1.income))}`,
      `${x(i + 1)},${y(Number(d1.spending))}`,
      `${x(i)},${y(Number(d0.spending))}`,
    ].join(' ')
    return { pts, saved }
  })

  const point = active !== null ? data[active] : null

  // Only drawn when it is on the axis. A negative average leftover is a real
  // and important fact, but this axis starts at zero and always has, so the
  // line would sit off the plot and read as absent rather than as negative —
  // the caption below states it in words instead.
  const avg = avgLeftover === undefined ? null : Number(avgLeftover)
  const avgOnAxis = avg !== null && Number.isFinite(avg) && avg >= 0 && avg <= niceMax

  // At most one, and only when the window reaches the present.
  const runningIndex = data.findIndex((d) => d.in_progress)

  return (
    <div className="space-y-3">
      {/* Two series, so a legend is always present — identity is never
          carried by colour alone. */}
      <div className="flex items-center gap-5 text-xs">
        <LegendKey color={SERIES.income} label="Income" />
        <LegendKey color={SERIES.spending} label="Spending" />
        {avg !== null && Number.isFinite(avg) && (
          <span className="text-mist-500">
            Average leftover{' '}
            <span className="tabular text-mist-300">{formatMoney(avgLeftover!)}</span>
          </span>
        )}
      </div>

      <div className="chart-scroll relative overflow-x-auto">
        <span className="sr-only">This chart scrolls horizontally — swipe to see the rest.</span>
        <svg
          viewBox={`0 0 ${WIDTH} ${HEIGHT}`}
          className="w-full min-w-[560px]"
          role="img"
          aria-label="Monthly income and spending"
          onMouseLeave={() => setActive(null)}
        >
          {/* Recessive grid. */}
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

          {/* The average-leftover reference line. Dashed and recessive: it is a
              benchmark, not a third series, and must not read as one. */}
          {avgOnAxis && (
            <g>
              <line
                x1={PAD.left}
                x2={WIDTH - PAD.right}
                y1={y(avg!)}
                y2={y(avg!)}
                stroke={SERIES.leftover}
                strokeWidth={1.5}
                strokeDasharray="5 4"
                opacity={0.7}
              />
              <text
                x={WIDTH - PAD.right}
                y={y(avg!) - 6}
                textAnchor="end"
                fontSize="10"
                fill={SERIES.leftover}
              >
                avg leftover
              </text>
            </g>
          )}

          {/* The month in flight, shaded. Both lines really do dive at the right
              edge — a month a third over has banked about a third of its income
              — and without this band the only available reading is that the
              household's income collapsed (MAD-110). */}
          {runningIndex >= 0 && (
            <rect
              x={x(runningIndex) - PLOT_W / Math.max(data.length, 1) / 2}
              y={PAD.top}
              width={PLOT_W / Math.max(data.length, 1)}
              height={PLOT_H}
              fill={CHART.grid}
              opacity={0.5}
            />
          )}

          {/* Month labels, thinned so they never collide — except the month in
              flight, which is always named. */}
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

          {/* The shaded gap between the two lines: green where income cleared
              spending, red where it did not. Behind the lines, low-opacity. */}
          {gapFills.map((g, i) => (
            <motion.polygon
              key={i}
              points={g.pts}
              fill={g.saved ? SERIES.leftover : STATUS.critical}
              {...areaFade(0.12, reduce)}
            />
          ))}

          <motion.path
            fill="none"
            stroke={SERIES.income}
            strokeWidth={2}
            {...lineDraw(line('income'), reduce)}
          />
          <motion.path
            fill="none"
            stroke={SERIES.spending}
            strokeWidth={2}
            {...lineDraw(line('spending'), reduce)}
          />

          {/* Markers on the hovered month, ringed in the surface colour so they
              stay legible where the two lines overlap. */}
          {active !== null &&
            (['income', 'spending'] as const).map((key) => (
              <circle
                key={key}
                cx={x(active)}
                cy={y(Number(data[active][key]))}
                r={5}
                fill={SERIES[key]}
                stroke={CHART.surface}
                strokeWidth={2}
              />
            ))}

          {/* Invisible hit targets, far wider than the marks themselves. */}
          {data.map((d, i) => (
            <rect
              key={d.month}
              x={x(i) - PLOT_W / Math.max(data.length, 1) / 2}
              y={PAD.top}
              width={PLOT_W / Math.max(data.length, 1)}
              height={PLOT_H}
              fill="transparent"
              onMouseEnter={() => setActive(i)}
            />
          ))}
        </svg>

        {point && (
          <div
            className="pointer-events-none absolute top-2 rounded-xl border border-white/10 bg-ink-900/95 px-3 py-2 text-xs shadow-xl backdrop-blur"
            style={{
              left: `${(x(active!) / WIDTH) * 100}%`,
              transform: 'translateX(-50%)',
            }}
          >
            <p className="mb-1 font-medium text-mist-100">{monthLabel(point.month, true)}</p>
            <TooltipRow color={SERIES.income} label="Income" value={point.income} />
            <TooltipRow color={SERIES.spending} label="Spending" value={point.spending} />
            <p className="mt-1 border-t border-white/10 pt-1 text-mist-300">
              Leftover <span className="tabular">{formatMoney(point.leftover)}</span>
            </p>
            {point.in_progress && <PartialMonthTooltipNote asOf={point.as_of} />}
          </div>
        )}
      </div>

      {dropped > 0 && (
        <p className="text-xs" style={{ color: CHART.textMuted }}>
          {dropped} month{dropped === 1 ? '' : 's'} left out: no published price
          index, so there is no honest way to restate{' '}
          {dropped === 1 ? 'it' : 'them'} in today’s dollars.
        </p>
      )}
    </div>
  )
}

/**
 * One month restated in base-period dollars, or nothing at all.
 *
 * Returns an empty array — so `flatMap` drops the month — when the server could
 * not deflate it. That is the only correct answer: the alternative is putting a
 * nominal figure on a real axis, which is invisible to the reader and wrong by
 * however much prices moved.
 *
 * The fixed/discretionary split is deliberately left nominal and is not plotted
 * by this chart; nothing here reads those fields.
 */
function toRealPoint(d: TrendPoint): TrendPoint[] {
  if (
    d.real_income === undefined ||
    d.real_spending === undefined ||
    d.real_leftover === undefined
  ) {
    return []
  }
  return [
    {
      ...d,
      income: d.real_income,
      spending: d.real_spending,
      leftover: d.real_leftover,
    },
  ]
}

function LegendKey({ color, label }: { color: string; label: string }) {
  return (
    <span className="flex items-center gap-1.5" style={{ color: CHART.textSecondary }}>
      <span
        className="inline-block h-2.5 w-2.5 rounded-full"
        style={{ backgroundColor: color }}
      />
      {label}
    </span>
  )
}

function TooltipRow({
  color,
  label,
  value,
}: {
  color: string
  label: string
  value: string
}) {
  return (
    <p className="flex items-center gap-2">
      <span
        className="inline-block h-2 w-2 shrink-0 rounded-full"
        style={{ backgroundColor: color }}
      />
      <span style={{ color: CHART.textSecondary }}>{label}</span>
      <span className="tabular ml-auto text-mist-100">{formatMoney(value)}</span>
    </p>
  )
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
export function TrendChart(props: Parameters<typeof TrendChartUnguarded>[0]) {
  return (
    <ChartBoundary label="income and spending">
      <TrendChartUnguarded {...props} />
    </ChartBoundary>
  )
}
