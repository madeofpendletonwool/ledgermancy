import { useState } from 'react'
import type { DaySpend } from '../../lib/api'
import { formatMoney } from '../../lib/money'
import { axisTicks, compactMoney } from './scale'
import { CHART, SERIES } from './tokens'

const WIDTH = 760
const HEIGHT = 240
const PAD = { top: 16, right: 16, bottom: 26, left: 56 }

const PLOT_W = WIDTH - PAD.left - PAD.right
const PLOT_H = HEIGHT - PAD.top - PAD.bottom

/** The even-pace reference: a de-emphasis dashed line, like DayBars' last-month
 *  average — it is the same dollars expressed as a target, not a second series. */
const PACE_COLOR = CHART.textMuted

/**
 * Aggregate budget burn-down for one month.
 *
 * Two lines, both dollars, so they share ONE axis: cumulative actual spend
 * against a straight even-pace reference (budget ÷ days × day-of-month). The
 * static progress bars on the page can't show mid-month pace — whether you are
 * ahead of or behind an even burn — because they don't account for how far
 * through the month you are. This does.
 *
 * The actual line only runs through the days that have happened: for the
 * current month it stops at today, so the gap between it and the pace line at
 * the right edge is the verdict (under the line = ahead, over it = behind).
 */
export function BurnDown({
  year,
  month,
  days,
  budgeted,
}: {
  /** Full year, e.g. 2026. */
  year: number
  /** 1-based month, 1 = January. */
  month: number
  days: DaySpend[]
  /** Total budgeted across categories for this month, display-only sum. */
  budgeted: number
}) {
  const [active, setActive] = useState<number | null>(null)

  const daysInMonth = new Date(year, month, 0).getDate()
  const today = new Date()
  const isCurrentMonth =
    today.getFullYear() === year && today.getMonth() + 1 === month
  const endpoint = isCurrentMonth ? today.getDate() : daysInMonth

  // Index daily spend by day-of-month (1-based), reading calendar parts so a
  // UTC wire value cannot shift a day backwards.
  const daily = new Array<number>(daysInMonth + 1).fill(0)
  for (const d of days) {
    const dom = Number(d.day.slice(8, 10))
    if (dom >= 1 && dom <= daysInMonth) daily[dom] = Number(d.spending)
  }

  // Cumulative actual spend through each day. Display-only arithmetic over
  // server-exact per-day totals.
  const cum = new Array<number>(daysInMonth + 1).fill(0)
  for (let d = 1; d <= daysInMonth; d++) cum[d] = cum[d - 1] + daily[d]

  if (budgeted <= 0) {
    return (
      <p className="py-8 text-center text-sm" style={{ color: CHART.textMuted }}>
        Set a budget to see the month&rsquo;s burn-down against an even pace.
      </p>
    )
  }

  const maxVal = Math.max(cum[endpoint], budgeted, 0)
  const { ticks, niceMax } = axisTicks(maxVal)

  const x = (d: number) => PAD.left + (d / daysInMonth) * PLOT_W
  const y = (v: number) =>
    PAD.top + PLOT_H - (niceMax > 0 ? (v / niceMax) * PLOT_H : 0)

  // Actual line only through the endpoint; future days haven't happened.
  const actualPts: string[] = []
  for (let d = 1; d <= endpoint; d++) {
    actualPts.push(`${d === 1 ? 'M' : 'L'} ${x(d)} ${y(cum[d])}`)
  }
  const actualPath = actualPts.join(' ')

  // Even pace: budgeted spread evenly across the month, day 0 → day N.
  const pacePath = `M ${x(0)} ${y(0)} L ${x(daysInMonth)} ${y(budgeted)}`

  const activeCum = active !== null ? cum[active] : 0
  const activePace = active !== null ? (budgeted * active) / daysInMonth : 0

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-baseline justify-between gap-3">
        <div className="flex items-center gap-5 text-xs">
          <span
            className="flex items-center gap-1.5"
            style={{ color: CHART.textSecondary }}
          >
            <span
              className="inline-block h-2.5 w-2.5 rounded-full"
              style={{ backgroundColor: SERIES.spending }}
            />
            Cumulative spend
          </span>
          <span
            className="flex items-center gap-1.5"
            style={{ color: CHART.textSecondary }}
          >
            <span
              className="inline-block h-0 w-4 border-t border-dashed"
              style={{ borderColor: PACE_COLOR }}
            />
            Even pace
          </span>
        </div>
        <PaceVerdict
          actual={cum[endpoint]}
          pace={(budgeted * endpoint) / daysInMonth}
        />
      </div>

      <div className="relative overflow-x-auto">
        <svg
          viewBox={`0 0 ${WIDTH} ${HEIGHT}`}
          className="w-full max-sm:min-w-0 sm:min-w-[560px]"
          role="img"
          aria-label="Cumulative spend against even pace this month"
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
          {Array.from({ length: daysInMonth }, (_, i) => i + 1).map((d) =>
            d === 1 || d % 5 === 0 ? (
              <text
                key={d}
                x={x(d)}
                y={HEIGHT - 8}
                textAnchor="middle"
                fontSize="11"
                fill={CHART.textMuted}
              >
                {d}
              </text>
            ) : null,
          )}

          {/* Crosshair for the hovered day. */}
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

          {/* Even-pace reference, drawn first so the actual line sits on top. */}
          <path
            d={pacePath}
            fill="none"
            stroke={PACE_COLOR}
            strokeWidth={1.5}
            strokeDasharray="5 4"
          />

          {/* Cumulative actual. */}
          <path
            d={actualPath}
            fill="none"
            stroke={SERIES.spending}
            strokeWidth={2}
          />

          {/* Endpoint marker — today for the current month. */}
          <circle
            cx={x(endpoint)}
            cy={y(cum[endpoint])}
            r={4}
            fill={SERIES.spending}
            stroke={CHART.surface}
            strokeWidth={2}
          />

          {/* Invisible per-day hit targets. */}
          {Array.from({ length: daysInMonth }, (_, i) => i + 1).map((d) => (
            <rect
              key={d}
              x={x(d) - PLOT_W / daysInMonth / 2}
              y={PAD.top}
              width={PLOT_W / daysInMonth}
              height={PLOT_H}
              fill="transparent"
              onMouseEnter={() => setActive(d)}
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
              Day {active}
            </p>
            <p className="tabular text-mist-300">
              Spent{' '}
              <span className="ml-1 text-mist-100">{formatMoney(String(activeCum))}</span>
            </p>
            <p className="tabular text-mist-500">
              Even pace {formatMoney(String(activePace))}
            </p>
          </div>
        )}
      </div>
    </div>
  )
}

/** The one-line verdict comparing actual cumulative spend to even pace. */
function PaceVerdict({ actual, pace }: { actual: number; pace: number }) {
  const diff = actual - pace
  if (Math.abs(diff) < 1) {
    return (
      <span className="text-xs text-mist-500">Roughly on pace so far</span>
    )
  }
  const ahead = diff < 0
  return (
    <span
      className="tabular text-xs"
      style={{ color: ahead ? SERIES.leftover : SERIES.spending }}
    >
      {formatMoney(String(Math.abs(diff)))} {ahead ? 'under' : 'over'} pace so far
    </span>
  )
}
