import { useState } from 'react'
import type { AccountProjection } from '../../lib/api'
import { formatMoney } from '../../lib/money'
import { CHART, SINGLE_SERIES, STATUS } from './tokens'

const WIDTH = 760
const HEIGHT = 280
const PAD = { top: 16, right: 16, bottom: 28, left: 72 }

const PLOT_W = WIDTH - PAD.left - PAD.right
const PLOT_H = HEIGHT - PAD.top - PAD.bottom

/**
 * One account's balance carried forward through its known bills.
 *
 * Three decisions worth stating, because they are what make this chart honest:
 *
 *  - ONE series at a time. The caller picks which account to show rather than
 *    overlaying every one, so hue never has to identify more accounts than the
 *    validated palette has slots for.
 *  - Zero is always on the axis and always drawn, whether or not the balance
 *    goes near it. A projected dip below zero is the entire point of this chart,
 *    and a truncated axis would hide how close it came.
 *  - The line is a step function, not a smooth curve. A bill clears on one day;
 *    interpolating between days would draw a balance that never existed.
 */
export function ProjectionChart({ series }: { series: AccountProjection }) {
  const [active, setActive] = useState<number | null>(null)

  const points = series.points
  if (points.length < 2) {
    return (
      <p className="py-12 text-center text-sm" style={{ color: CHART.textMuted }}>
        Not enough of a horizon to project a balance.
      </p>
    )
  }

  const values = points.map((p) => Number(p.balance))
  const { ticks, min, max } = axisRange(Math.min(...values, 0), Math.max(...values, 0))

  const x = (i: number) => PAD.left + (i / (points.length - 1)) * PLOT_W
  const y = (v: number) =>
    PAD.top + PLOT_H - (max > min ? ((v - min) / (max - min)) * PLOT_H : PLOT_H / 2)

  // Step path: hold the balance flat across a day, then drop on the day a bill
  // clears.
  const path = points
    .map((p, i) => {
      const cmd = i === 0 ? 'M' : 'L'
      const prev = i === 0 ? null : Number(points[i - 1].balance)
      const here = Number(p.balance)
      return prev !== null && prev !== here
        ? `L ${x(i)} ${y(prev)} L ${x(i)} ${y(here)}`
        : `${cmd} ${x(i)} ${y(here)}`
    })
    .join(' ')

  const zeroY = y(0)
  const point = active !== null ? points[active] : null
  const stride = labelStride(points.length)

  return (
    <div className="relative overflow-x-auto">
      <svg
        viewBox={`0 0 ${WIDTH} ${HEIGHT}`}
        className="w-full min-w-[560px]"
        role="img"
        aria-label={`Projected balance for ${series.name}`}
        onMouseLeave={() => setActive(null)}
      >
        {/* Everything below zero is tinted, so an overdraft is visible at a
            glance rather than something you have to read off the axis. */}
        {min < 0 && (
          <rect
            x={PAD.left}
            y={zeroY}
            width={PLOT_W}
            height={Math.max(PAD.top + PLOT_H - zeroY, 0)}
            fill={STATUS.critical}
            opacity={0.08}
          />
        )}

        {ticks.map((t) => (
          <g key={t}>
            <line
              x1={PAD.left}
              x2={WIDTH - PAD.right}
              y1={y(t)}
              y2={y(t)}
              stroke={t === 0 ? CHART.axis : CHART.grid}
              strokeWidth={t === 0 ? 1.5 : 1}
            />
            <text
              x={PAD.left - 10}
              y={y(t) + 4}
              textAnchor="end"
              fontSize="11"
              fill={t === 0 ? CHART.textSecondary : CHART.textMuted}
            >
              {compactMoney(t)}
            </text>
          </g>
        ))}

        {points.map((p, i) =>
          i % stride === 0 ? (
            <text
              key={p.date}
              x={x(i)}
              y={HEIGHT - 8}
              textAnchor="middle"
              fontSize="11"
              fill={CHART.textMuted}
            >
              {dayLabel(p.date)}
            </text>
          ) : null,
        )}

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

        <path d={path} fill="none" stroke={SINGLE_SERIES} strokeWidth={2} />

        {/* Days a bill clears get a marker, so the drops are attributable. */}
        {points.map((p, i) =>
          Number(p.due) > 0 ? (
            <circle
              key={`due-${p.date}`}
              cx={x(i)}
              cy={y(Number(p.balance))}
              r={3}
              fill={SINGLE_SERIES}
              stroke={CHART.surface}
              strokeWidth={1.5}
            />
          ) : null,
        )}

        {active !== null && (
          <circle
            cx={x(active)}
            cy={y(Number(points[active].balance))}
            r={5}
            fill={SINGLE_SERIES}
            stroke={CHART.surface}
            strokeWidth={2}
          />
        )}

        {points.map((p, i) => (
          <rect
            key={`hit-${p.date}`}
            x={x(i) - PLOT_W / points.length / 2}
            y={PAD.top}
            width={PLOT_W / points.length}
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
          <p className="mb-1 font-medium text-mist-100">{dayLabel(point.date, true)}</p>
          <p style={{ color: CHART.textSecondary }}>
            Projected{' '}
            <span className="tabular ml-1 text-mist-100">{formatMoney(point.balance)}</span>
          </p>
          {Number(point.due) > 0 && (
            <p className="mt-1 border-t border-white/10 pt-1" style={{ color: CHART.textSecondary }}>
              Bills due <span className="tabular ml-1 text-mist-100">{formatMoney(point.due)}</span>
            </p>
          )}
        </div>
      )}
    </div>
  )
}

/**
 * A tick range that always contains zero and lands on round numbers. Returning
 * the padded min/max alongside the ticks keeps the axis and the plot on exactly
 * the same scale.
 */
function axisRange(rawMin: number, rawMax: number) {
  if (rawMax === rawMin) return { ticks: [0], min: rawMin - 1, max: rawMax + 1 }

  const step = niceStep((rawMax - rawMin) / 4)
  const min = Math.floor(rawMin / step) * step
  const max = Math.ceil(rawMax / step) * step

  const ticks: number[] = []
  for (let t = min; t <= max + step / 2; t += step) {
    // Rounding here kills the float drift that would otherwise render a tick
    // labelled "-0" or "1.0000000000000002".
    ticks.push(Math.round(t))
  }
  return { ticks, min, max }
}

function niceStep(rough: number): number {
  const magnitude = Math.pow(10, Math.floor(Math.log10(Math.max(rough, 1))))
  for (const m of [1, 2, 2.5, 5, 10]) {
    if (magnitude * m >= rough) return magnitude * m
  }
  return magnitude * 10
}

/** Keeps day labels from colliding on a long horizon. */
function labelStride(count: number): number {
  return Math.max(1, Math.ceil(count / 8))
}

function compactMoney(value: number): string {
  const sign = value < 0 ? '-' : ''
  const abs = Math.abs(value)
  if (abs >= 1000) return `${sign}$${Math.round(abs / 100) / 10}k`
  return `${sign}$${abs}`
}

/**
 * Formats a calendar date. Parsed from its parts rather than through Date's ISO
 * handling, which would render the previous day in any timezone west of UTC.
 */
function dayLabel(iso: string, long = false): string {
  const [year, month, day] = iso.slice(0, 10).split('-').map(Number)
  return new Date(year, month - 1, day).toLocaleDateString('en-US',
    long ? { weekday: 'short', month: 'short', day: 'numeric' } : { month: 'short', day: 'numeric' })
}
