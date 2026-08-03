import { useState } from 'react'
import type { TrendPoint } from '../../lib/api'
import { formatMoney } from '../../lib/money'
import { axisTicks, compactMoney, labelStride } from './scale'
import { CHART, SERIES } from './tokens'

const WIDTH = 760
const HEIGHT = 240
const PAD = { top: 16, right: 16, bottom: 28, left: 56 }
const BAR_GAP = 2

const PLOT_W = WIDTH - PAD.left - PAD.right
const PLOT_H = HEIGHT - PAD.top - PAD.bottom

/**
 * Fixed vs discretionary spend, stacked, month by month.
 *
 * The two segments are the same two buckets the period summary reports — same
 * FILTER clauses in SQL — so each month's stacked height equals that month's
 * total spending to the cent. Two segments = two categorical slots, which sits
 * inside the palette's three-slot cap.
 *
 * Which slot gets which colour is the one decision worth stating. Fixed spend
 * (rent, utilities, loan payments) is the spending the household has the least
 * flex in, and discretionary is the spending it has the most; the spending slot
 * (`#d95926`, orange) reads as the "hotter", more urgent colour, so discretionary
 * wears it and fixed wears the calmer leftover slot. The leftover slot is named
 * for the savings use-case, but it is just slot 3 of the validated palette — a
 * calm aqua — and reusing it here does not assert that fixed spend IS leftover.
 */
export function FixedDiscretionaryChart({ data }: { data: TrendPoint[] }) {
  const [active, setActive] = useState<number | null>(null)

  if (data.length === 0) {
    return (
      <p className="py-12 text-center text-sm" style={{ color: CHART.textMuted }}>
        Not enough history yet to chart fixed vs discretionary.
      </p>
    )
  }

  // Tallied in the browser ONLY to choose the y-axis ceiling; every figure the
  // user reads comes straight off the server's decimal strings.
  const totals = data.map((d) => Number(d.fixed_spending) + Number(d.discretionary_spending))
  const max = Math.max(...totals, 0)
  const { ticks, niceMax } = axisTicks(max)

  const band = PLOT_W / data.length
  const barW = Math.max(band - BAR_GAP, 1)
  const x = (i: number) => PAD.left + (i + 0.5) * band
  const y = (v: number) =>
    PAD.top + PLOT_H - (niceMax > 0 ? (v / niceMax) * PLOT_H : 0)
  const stride = labelStride(data.length)

  const activePoint = active !== null ? data[active] : null

  return (
    <div className="space-y-3">
      {/* Two segments, so identity rides on a legend — never on colour alone. */}
      <div className="flex items-center gap-5 text-xs">
        <LegendKey color={SERIES.leftover} label="Fixed" />
        <LegendKey color={SERIES.spending} label="Discretionary" />
      </div>

      <div className="relative overflow-x-auto">
        <svg
          viewBox={`0 0 ${WIDTH} ${HEIGHT}`}
          className="w-full min-w-[560px]"
          role="img"
          aria-label="Fixed versus discretionary spending per month"
          onMouseLeave={() => setActive(null)}
        >
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

          {data.map((d, i) =>
            i % stride === 0 ? (
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

          {data.map((d, i) => {
            const fixed = Number(d.fixed_spending)
            const disc = Number(d.discretionary_spending)
            const total = fixed + disc
            if (total <= 0) return null

            // Fixed sits at the base (calmer), discretionary stacks on top
            // (hotter). The baseline segment keeps its square base on the axis;
            // the stacked segment rounds only its data-end, matching MonthlyBars.
            const fixedH = niceMax > 0 ? (fixed / niceMax) * PLOT_H : 0
            const discH = niceMax > 0 ? (disc / niceMax) * PLOT_H : 0
            const fixedTop = PAD.top + PLOT_H - fixedH
            const discTop = fixedTop - discH
            const dim = active !== null && active !== i
            return (
              <g key={d.month}>
                {fixed > 0 && (
                  <rect
                    x={x(i) - barW / 2}
                    y={fixedTop}
                    width={barW}
                    height={fixedH}
                    fill={SERIES.leftover}
                    opacity={dim ? 0.45 : 0.9}
                  />
                )}
                {disc > 0 && (
                  <path
                    d={topRoundedRect(x(i) - barW / 2, discTop, barW, discH)}
                    fill={SERIES.spending}
                    opacity={dim ? 0.45 : 0.9}
                  />
                )}
              </g>
            )
          })}

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

          {data.map((d, i) => (
            <rect
              key={d.month}
              x={x(i) - band / 2}
              y={PAD.top}
              width={band}
              height={PLOT_H}
              fill="transparent"
              onMouseEnter={() => setActive(i)}
            />
          ))}
        </svg>

        {activePoint && (
          <div
            className="pointer-events-none absolute top-2 rounded-xl border border-white/10 bg-ink-900/95 px-3 py-2 text-xs shadow-xl backdrop-blur"
            style={{
              left: `${(x(active!) / WIDTH) * 100}%`,
              transform: 'translateX(-50%)',
            }}
          >
            <p className="mb-1 font-medium text-mist-100">{monthLabel(activePoint.month, true)}</p>
            <TooltipRow color={SERIES.leftover} label="Fixed" value={activePoint.fixed_spending} />
            <TooltipRow
              color={SERIES.spending}
              label="Discretionary"
              value={activePoint.discretionary_spending}
            />
            <p className="mt-1 border-t border-white/10 pt-1 text-mist-300">
              Total <span className="tabular">{formatMoney(activePoint.spending)}</span>
            </p>
          </div>
        )}
      </div>
    </div>
  )
}

const BAR_RADIUS = 4

/**
 * A rectangle rounded only along its top edge, so the stacked segment sits
 * flush on the segment below it (and the bottom segment sits on the axis). The
 * radius shrinks for a short or narrow segment so a small month renders as a
 * small bar, not a lozenge.
 */
function topRoundedRect(x: number, y: number, w: number, h: number): string {
  if (h <= 0) return ''
  const r = Math.max(0, Math.min(BAR_RADIUS, w / 2, h))
  if (r === 0) return `M${x} ${y}H${x + w}V${y + h}H${x}Z`
  return [
    `M${x} ${y + r}`,
    `Q${x} ${y} ${x + r} ${y}`,
    `H${x + w - r}`,
    `Q${x + w} ${y} ${x + w} ${y + r}`,
    `V${y + h}`,
    `H${x}`,
    'Z',
  ].join(' ')
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
