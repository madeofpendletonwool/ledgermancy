import { useState } from 'react'
import type { TrendPoint } from '../../lib/api'
import { formatMoney } from '../../lib/money'
import { CHART, SERIES } from './tokens'

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
 */
export function TrendChart({ data }: { data: TrendPoint[] }) {
  const [active, setActive] = useState<number | null>(null)

  if (data.length === 0) {
    return (
      <p className="py-12 text-center text-sm" style={{ color: CHART.textMuted }}>
        Not enough history yet to chart a trend.
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

  const point = active !== null ? data[active] : null

  return (
    <div className="space-y-3">
      {/* Two series, so a legend is always present — identity is never
          carried by colour alone. */}
      <div className="flex items-center gap-5 text-xs">
        <LegendKey color={SERIES.income} label="Income" />
        <LegendKey color={SERIES.spending} label="Spending" />
      </div>

      <div className="relative overflow-x-auto">
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

          {/* Month labels, thinned so they never collide. */}
          {data.map((d, i) =>
            i % labelStride(data.length) === 0 ? (
              <text
                key={d.month}
                x={x(i)}
                y={HEIGHT - 8}
                textAnchor="middle"
                fontSize="11"
                fill={CHART.textMuted}
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

          <path d={line('income')} fill="none" stroke={SERIES.income} strokeWidth={2} />
          <path d={line('spending')} fill="none" stroke={SERIES.spending} strokeWidth={2} />

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
          </div>
        )}
      </div>
    </div>
  )
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

/**
 * Builds axis ticks that land on round numbers.
 *
 * Splitting the maximum into fixed fractions (max/4 and so on) puts ticks on
 * values like 1250 and 3750, which then render as "$1k, $3k, $4k" — a sequence
 * that skips $2k and reads as though a gridline is missing. Instead the step
 * itself is snapped to 1, 2, 2.5 or 5 times a power of ten, so every label is a
 * round number and the spacing between them is uniform.
 */
function axisTicks(max: number): { ticks: number[]; niceMax: number } {
  if (max <= 0) return { ticks: [0, 25, 50, 75, 100], niceMax: 100 }

  const targetSteps = 4
  const rawStep = max / targetSteps
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
    // Keep a decimal for steps like 2.5k rather than rounding two different
    // ticks onto the same label.
    return `$${Number.isInteger(k) ? k : k.toFixed(1)}k`
  }
  return `$${Number.isInteger(v) ? v : v.toFixed(0)}`
}

/** Thins x labels so they never overlap on a narrow chart. */
function labelStride(count: number): number {
  return count > 12 ? 3 : count > 8 ? 2 : 1
}

function monthLabel(month: string, long = false): string {
  const [year, m] = month.split('-').map(Number)
  return new Date(year, m - 1, 1).toLocaleDateString('en-US', {
    month: 'short',
    year: long ? 'numeric' : '2-digit',
  })
}
