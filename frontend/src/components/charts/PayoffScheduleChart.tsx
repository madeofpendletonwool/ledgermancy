import { useState } from 'react'
import type { PayoffSchedulePoint } from '../../lib/api'
import { formatMoney } from '../../lib/money'
import { CHART, SERIES, STATUS } from './tokens'

const WIDTH = 760
const HEIGHT = 240
const PAD = { top: 16, right: 16, bottom: 28, left: 64 }
const PLOT_W = WIDTH - PAD.left - PAD.right
const PLOT_H = HEIGHT - PAD.top - PAD.bottom

/**
 * A debt-payoff amortization curve: the balance declining to zero, with the
 * interest paid each month shaded underneath.
 *
 * Mirrors the Schedule page's accumulation ProjectionChart going DOWN instead
 * of up — same hand-rolled SVG approach, same single-series hue, same "zero is
 * always on the axis" rule. Every figure plotted arrives finished from the
 * server: the schedule's interest series sums exactly to the `total_interest`
 * headline quoted beside it, and the balance ends at zero rather than at a
 * fractional-cent overshoot.
 *
 * ONE series (the balance), so single-series hue throughout. The interest
 * shade is the STATUS.warning token at low opacity: it is not a second series
 * — it is the integral of the same balance curve, the area under which IS the
 * interest paid. Labelling it as a categorical slot would imply a second
 * measure where there is one.
 */
export function PayoffScheduleChart({
  schedule,
  startBalance,
  monthlyPayment,
}: {
  schedule: PayoffSchedulePoint[]
  /** The opening balance, echoed back so the curve's left edge can start at it
   *  rather than at the first point's post-payment balance — the chart opens at
   *  "what you owe now" and closes at zero. */
  startBalance: string
  monthlyPayment: string
}) {
  const [active, setActive] = useState<number | null>(null)

  if (schedule.length === 0) {
    return (
      <p className="py-10 text-center text-sm" style={{ color: CHART.textMuted }}>
        No payoff schedule to chart.
      </p>
    )
  }

  // The plot's points are the schedule's, with a synthetic t=0 starting point
  // at the opening balance so the line begins where the debt actually stood
  // before the first payment, not one payment in.
  const points: { month: number; balance: number; interest: number }[] = [
    { month: 0, balance: Number(startBalance), interest: 0 },
    ...schedule.map((p) => ({
      month: p.month,
      balance: Number(p.balance),
      interest: Number(p.interest),
    })),
  ]

  // Max is the opening balance (the schedule only declines). Zero is always on
  // the axis: a payoff curve's whole point is the trip to the floor.
  const max = Math.max(...points.map((p) => p.balance), Number(startBalance), 0)
  const { ticks, niceMax } = axisRange(max)

  const x = (i: number) =>
    points.length === 1
      ? PAD.left + PLOT_W / 2
      : PAD.left + (i / (points.length - 1)) * PLOT_W
  const y = (v: number) =>
    PAD.top + PLOT_H - (niceMax > 0 ? (v / niceMax) * PLOT_H : 0)

  // Balance line: declining from the opening balance to zero.
  const balancePath = points
    .map((p, i) => `${i === 0 ? 'M' : 'L'} ${x(i)} ${y(p.balance)}`)
    .join(' ')

  // Interest shade: the area under the balance curve, tinted with the warning
  // token at low opacity. This is NOT the interest paid — it is a visual cue
  // for "the curve is dropping because of payments, and the drop is steeper
  // than a straight line where interest is high". The actual per-month
  // interest figures ride on the hover.
  const shadePath = `${balancePath} L ${x(points.length - 1)} ${y(0)} L ${x(0)} ${y(0)} Z`

  const stride = labelStride(points.length)
  const activePoint = active !== null ? points[active] : null
  const paymentNum = active !== null && active > 0 ? schedule[active - 1] : null

  return (
    <div className="relative overflow-x-auto">
      <svg
        viewBox={`0 0 ${WIDTH} ${HEIGHT}`}
        className="w-full min-w-[560px]"
        role="img"
        aria-label="Debt payoff schedule"
        onMouseLeave={() => setActive(null)}
      >
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

        {/* The interest-portion shade: low-opacity, sits under the balance
            line, labelled in the legend so it is not mistaken for a second
            series. */}
        <path d={shadePath} fill={STATUS.warning} opacity={0.08} />

        <path d={balancePath} fill="none" stroke={SERIES.spending} strokeWidth={2} />

        {/* A marker at the payoff point so "this is where it ends" is
            attributable rather than implied. */}
        <circle
          cx={x(points.length - 1)}
          cy={y(points[points.length - 1].balance)}
          r={4}
          fill={SERIES.spending}
          stroke={CHART.surface}
          strokeWidth={2}
        />

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

        {active !== null && (
          <circle
            cx={x(active)}
            cy={y(points[active].balance)}
            r={5}
            fill={SERIES.spending}
            stroke={CHART.surface}
            strokeWidth={2}
          />
        )}

        {points.map((p, i) =>
          i % stride === 0 || i === points.length - 1 ? (
            <text
              key={p.month}
              x={x(i)}
              y={HEIGHT - 8}
              textAnchor={i === 0 ? 'start' : i === points.length - 1 ? 'end' : 'middle'}
              fontSize="11"
              fill={CHART.textMuted}
            >
              {i === 0 ? 'now' : `mo ${p.month}`}
            </text>
          ) : null,
        )}

        {points.map((_, i) => (
          <rect
            key={i}
            x={x(i) - PLOT_W / points.length / 2}
            y={PAD.top}
            width={PLOT_W / points.length}
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
          <p className="mb-1 font-medium text-mist-100">
            {active === 0 ? 'Now' : `Payment ${activePoint.month}`}
          </p>
          <p style={{ color: CHART.textSecondary }}>
            Balance{' '}
            <span className="tabular ml-1 text-mist-100">
              {formatMoney(String(activePoint.balance))}
            </span>
          </p>
          {paymentNum && Number(paymentNum.interest) > 0 && (
            <p
              className="mt-1 border-t border-white/10 pt-1"
              style={{ color: STATUS.warning }}
            >
              Interest this month{' '}
              <span className="tabular ml-1 text-mist-100">
                {formatMoney(paymentNum.interest)}
              </span>
            </p>
          )}
        </div>
      )}

      <div
        className="mt-2 flex flex-wrap gap-4 text-xs"
        style={{ color: CHART.textMuted }}
      >
        <span className="flex items-center gap-1.5">
          <span
            className="inline-block h-0.5 w-4"
            style={{ backgroundColor: SERIES.spending }}
          />
          Balance owed
        </span>
        <span className="flex items-center gap-1.5">
          <span
            className="inline-block h-2.5 w-2.5 rounded-sm"
            style={{ backgroundColor: STATUS.warning, opacity: 0.4 }}
          />
          Interest portion
        </span>
        <span>
          {formatMoney(monthlyPayment)}/mo ·{' '}
          {points.length - 1} {points.length - 1 === 1 ? 'payment' : 'payments'}
        </span>
      </div>
    </div>
  )
}

/**
 * A tick range that always contains zero. A payoff curve only ever declines, so
 * the range is [0, max] — simpler than NetWorthComposition's mirrored range
 * because there is nothing below zero to plot.
 */
function axisRange(max: number): { ticks: number[]; niceMax: number } {
  if (max <= 0) return { ticks: [0, 100, 200], niceMax: 200 }
  const step = niceStep(max / 4)
  const niceMax = Math.ceil(max / step) * step
  const ticks: number[] = []
  for (let t = 0; t <= niceMax + step / 2; t += step) ticks.push(Math.round(t))
  return { ticks, niceMax }
}

function niceStep(rough: number): number {
  const magnitude = Math.pow(10, Math.floor(Math.log10(Math.max(rough, 1))))
  for (const m of [1, 2, 2.5, 5, 10]) {
    if (magnitude * m >= rough) return magnitude * m
  }
  return magnitude * 10
}

function compactMoney(v: number): string {
  if (v === 0) return '$0'
  if (v >= 1000) {
    const k = v / 1000
    return `$${Number.isInteger(k) ? k : k.toFixed(1)}k`
  }
  return `$${Number.isInteger(v) ? v : v.toFixed(0)}`
}

function labelStride(count: number): number {
  return count > 24 ? 6 : count > 12 ? 3 : count > 8 ? 2 : 1
}
