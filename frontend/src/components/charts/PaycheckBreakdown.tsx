import { useState } from 'react'
import type { Paystub } from '../../lib/api'
import { formatMoney } from '../../lib/money'
import { CHART, SERIES } from './tokens'

const HEIGHT = 46
const RADIUS = 6

/**
 * "Where your paycheck went" — gross flowing left to right through taxes,
 * retirement and benefits into what actually landed.
 *
 * This is the chart doc 23 calls the reason to build the whole feature, and it
 * is the one thing on the page most people have genuinely never seen: for a
 * typical W-2 earner 30–45% of the bar is money the rest of this app has never
 * been able to show at all.
 *
 * WHY IT IS ONE BAR AND NOT A SANKEY. A Sankey needs two node columns to say
 * anything a stacked bar does not, and one pay period has only one source. The
 * cash-flow Sankey earns its complexity because it has many inflows AND many
 * outflows; this has one of the first and a handful of the second.
 *
 * COLOUR. The palette caps at three categorical slots and this needs four
 * segments, which is a real constraint rather than an inconvenience — so three
 * segments take the validated slots on merit and the fourth is deliberately
 * recessive rather than a fourth hue nobody validated:
 *
 *   take-home   leftover (aqua)   — literally what is left; the token's own use
 *   taxes       spending (orange) — the hotter colour, and the money truly gone
 *   retirement  income (blue)     — money kept, just not in the bank account
 *   benefits    white at low alpha — the residual band, reading as furniture
 *
 * Identity rides on the legend and on the table beneath the chart, never on hue
 * alone: every segment is also a labelled row with its own figure.
 */
export function PaycheckBreakdown({ stub }: { stub: Paystub }) {
  const [active, setActive] = useState<string | null>(null)

  const segments = buildSegments(stub)
  // Summed in the browser ONLY to compute segment widths. Every figure the user
  // reads is the server's own decimal string.
  const total = segments.reduce((sum, s) => sum + s.value, 0)

  if (total <= 0) {
    return (
      <p className="py-8 text-center text-sm" style={{ color: CHART.textMuted }}>
        This paystub has no figures to chart yet.
      </p>
    )
  }

  let offset = 0
  const placed = segments.map((s) => {
    const width = (s.value / total) * 100
    const item = { ...s, x: offset, width }
    offset += width
    return item
  })

  const activeSegment = placed.find((s) => s.key === active) ?? null

  return (
    <div className="space-y-3">
      <div className="relative">
        <svg
          viewBox={`0 0 100 ${HEIGHT}`}
          preserveAspectRatio="none"
          className="w-full"
          style={{ height: HEIGHT }}
          role="img"
          aria-label="Where this paycheck went, from gross pay to take-home"
          onMouseLeave={() => setActive(null)}
        >
          {placed.map((s) => (
            <rect
              key={s.key}
              x={s.x}
              y={0}
              width={Math.max(s.width, 0)}
              height={HEIGHT}
              fill={s.color}
              opacity={active && active !== s.key ? 0.4 : 1}
              onMouseEnter={() => setActive(s.key)}
            />
          ))}
          {/* Drawn over the fills so the rounded ends read as one bar rather
              than as a rounded first and last segment. */}
          <rect
            x={0}
            y={0}
            width={100}
            height={HEIGHT}
            rx={RADIUS}
            ry={RADIUS}
            fill="none"
            stroke={CHART.surface}
            strokeWidth={2}
            pointerEvents="none"
            vectorEffect="non-scaling-stroke"
          />
        </svg>

        {activeSegment && (
          <div className="pointer-events-none absolute -top-2 -translate-y-full rounded-xl border border-white/10 bg-ink-900/95 px-3 py-2 text-xs shadow-xl backdrop-blur"
            style={{
              left: `${Math.min(Math.max(activeSegment.x + activeSegment.width / 2, 8), 92)}%`,
              transform: 'translate(-50%, -100%)',
            }}
          >
            <p className="font-medium text-mist-100">{activeSegment.label}</p>
            <p className="tabular mt-0.5 text-mist-300">
              {formatMoney(activeSegment.amount)}
            </p>
          </div>
        )}
      </div>

      <div className="flex flex-wrap items-center gap-x-5 gap-y-2 text-xs">
        {placed.map((s) => (
          <span
            key={s.key}
            className="flex items-center gap-1.5"
            style={{ color: CHART.textSecondary }}
          >
            <span
              className="inline-block h-2.5 w-2.5 rounded-full"
              style={{ backgroundColor: s.color }}
            />
            {s.label}
            <span className="tabular text-mist-100">{formatMoney(s.amount)}</span>
          </span>
        ))}
      </div>
    </div>
  )
}

type Segment = {
  key: string
  label: string
  amount: string
  value: number
  color: string
}

/**
 * The bands, in flow order: what was taken first, what landed last.
 *
 * The server already grouped the deductions and guarantees the bands plus net
 * sum back to gross for any stub that balances — which is every confirmed stub,
 * because confirmation refuses the ones that do not. So no reconciling fudge
 * segment is needed here, and none is drawn: a chart that silently closes its
 * own gap would hide exactly the error the balance check exists to catch.
 */
function buildSegments(stub: Paystub): Segment[] {
  const colours: Record<string, string> = {
    tax: SERIES.spending,
    retirement: SERIES.income,
    health: 'rgba(255,255,255,0.28)',
    insurance: 'rgba(255,255,255,0.20)',
    other: 'rgba(255,255,255,0.14)',
  }

  const segments: Segment[] = stub.breakdown.map((band) => ({
    key: band.group,
    label: band.label,
    amount: band.amount,
    value: Number(band.amount),
    color: colours[band.group] ?? 'rgba(255,255,255,0.14)',
  }))

  segments.push({
    key: 'net',
    label: 'Take-home',
    amount: stub.net,
    value: Number(stub.net),
    color: SERIES.leftover,
  })

  return segments.filter((s) => Number.isFinite(s.value) && s.value > 0)
}
