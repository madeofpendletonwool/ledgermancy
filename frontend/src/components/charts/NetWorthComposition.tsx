import { useState } from 'react'
import type { NetWorthPoint } from '../../lib/api'
import { formatMoney } from '../../lib/money'
import { CHART, SERIES } from './tokens'

const WIDTH = 760
const HEIGHT = 260
const PAD = { top: 16, right: 16, bottom: 28, left: 64 }
const PLOT_W = WIDTH - PAD.left - PAD.right
const PLOT_H = HEIGHT - PAD.top - PAD.bottom

/**
 * Net-worth composition over time, as a stacked area.
 *
 * The composition is what the snapshot already recorded per point: cash +
 * investments + other + manual assets above the axis, credit + loan + manual
 * debt below it (liabilities drawn as negative area so the line between assets
 * and liabilities is the literal net-worth path — the same single line the
 * "Over time" chart plots on its own, kept here as the boundary between the two
 * stacks).
 *
 * Two stacks, two categorical slots: assets wear leftover green, liabilities
 * wear spending orange. Net worth — the difference — sits as the gap between
 * them and is the figure the existing single-line chart carries, so this view
 * is a decomposition of that one rather than a replacement for it.
 *
 * The two stacks share ONE y-axis. Both are dollars; a second axis would let
 * them cross wherever the scales met and imply a relationship the data does
 * not contain.
 */
export function NetWorthComposition({ data }: { data: NetWorthPoint[] }) {
  const [active, setActive] = useState<number | null>(null)

  // Need at least two points with a breakdown to draw any area at all. A single
  // point or a run of pre-composition snapshots renders the existing line chart
  // instead — this view is additive, never the only one.
  const usable = data.filter((d) => d.breakdown)
  if (usable.length < 2) {
    return (
      <p className="py-12 text-center text-sm" style={{ color: CHART.textMuted }}>
        Composition appears once there are at least two snapshots with a
        recorded breakdown.
      </p>
    )
  }

  // Per-point asset and liability totals from the breakdown. Done in the client
  // ONLY to choose the y-axis ceiling and stack the bands; every figure the
  // reader pulls off a hover is the server's decimal string.
  const assetsPerPoint = usable.map((d) => assetSum(d))
  const liabsPerPoint = usable.map((d) => liabilitySum(d))

  const maxAsset = Math.max(...assetsPerPoint, 0)
  const maxLiab = Math.max(...liabsPerPoint, 0)
  // Zero is always on the axis: net worth can legitimately be negative, and a
  // truncated axis would hide the sign — the most important thing on the chart.
  const max = Math.max(maxAsset, maxLiab)
  const { ticks, niceMax } = axisRange(max)

  const x = (i: number) =>
    PAD.left + (usable.length === 1 ? PLOT_W / 2 : (i / (usable.length - 1)) * PLOT_W)
  const y = (v: number) =>
    PAD.top + PLOT_H / 2 - (niceMax > 0 ? (v / niceMax) * (PLOT_H / 2) : 0)

  const assetBands = stackBands(usable, assetBandSpec)
  const liabilityBands = stackBands(usable, liabilityBandSpec)

  // Where the asset stack meets the liability stack on each point is the net
  // worth — the same single line NetWorthChart plots. Carrying it here as a
  // thin path keeps the relationship explicit rather than asking the reader to
  // infer it from the gap between the two areas.
  const netWorthPath = usable
    .map((d, i) => {
      const net = Number(d.net_worth)
      return `${i === 0 ? 'M' : 'L'} ${x(i)} ${y(net)}`
    })
    .join(' ')

  const point = active !== null ? usable[active] : null
  const stride = labelStride(usable.length)

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-5 text-xs">
        <LegendKey color={SERIES.leftover} label="Assets" />
        <LegendKey color={SERIES.spending} label="Liabilities" />
        <span className="text-mist-500">— net worth (the line between)</span>
      </div>

      <div className="relative overflow-x-auto">
        <svg
          viewBox={`0 0 ${WIDTH} ${HEIGHT}`}
          className="w-full max-sm:min-w-0 sm:min-w-[560px]"
          role="img"
          aria-label="Net worth composition over time"
          onMouseLeave={() => setActive(null)}
        >
          {/* Zero line: assets stack up from here, liabilities stack down. */}
          <line
            x1={PAD.left}
            x2={WIDTH - PAD.right}
            y1={y(0)}
            y2={y(0)}
            stroke={CHART.axis}
            strokeWidth={1.5}
          />

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
              <line
                x1={PAD.left}
                x2={WIDTH - PAD.right}
                y1={y(-t)}
                y2={y(-t)}
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
              <text
                x={PAD.left - 10}
                y={y(-t) + 4}
                textAnchor="end"
                fontSize="11"
                fill={CHART.textMuted}
              >
                {t === 0 ? '$0' : `-${compactMoney(t)}`}
              </text>
            </g>
          ))}

          {/* Liability stack: drawn first (below), going down from zero. Each
              band is the next layer of debt piled under the previous one. */}
          {liabilityBands.map((band, i) => (
            <path
              key={liabilityBandSpec[i].key}
              d={band.area(x, (v) => y(-v))}
              fill={SERIES.spending}
              opacity={0.25 + 0.18 * i}
            />
          ))}

          {/* Asset stack: drawn on top, going up from zero. Each band piles on
              the previous, so the topmost edge IS total assets. */}
          {assetBands.map((band, i) => (
            <path
              key={assetBandSpec[i].key}
              d={band.area(x, (v) => y(v))}
              fill={SERIES.leftover}
              opacity={0.3 + 0.18 * i}
            />
          ))}

          {/* Net worth as the line between the two stacks. Same dollars, same
              axis, same single series the existing line chart draws on its own. */}
          <path
            d={netWorthPath}
            fill="none"
            stroke={CHART.textPrimary}
            strokeWidth={1.5}
            opacity={0.85}
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

          {usable.map((d, i) =>
            i % stride === 0 || i === usable.length - 1 ? (
              <text
                key={d.as_of}
                x={x(i)}
                y={HEIGHT - 8}
                textAnchor={i === 0 ? 'start' : i === usable.length - 1 ? 'end' : 'middle'}
                fontSize="11"
                fill={CHART.textMuted}
              >
                {d.as_of}
              </text>
            ) : null,
          )}

          {usable.map((d, i) => (
            <rect
              key={d.as_of}
              x={x(i) - PLOT_W / usable.length / 2}
              y={PAD.top}
              width={PLOT_W / usable.length}
              height={PLOT_H}
              fill="transparent"
              onMouseEnter={() => setActive(i)}
            />
          ))}
        </svg>

        {point && point.breakdown && (
          <div
            className="pointer-events-none absolute top-2 rounded-xl border border-white/10 bg-ink-900/95 px-3 py-2 text-xs shadow-xl backdrop-blur"
            style={{
              left: `${(x(active!) / WIDTH) * 100}%`,
              transform: 'translateX(-50%)',
            }}
          >
            <p className="mb-1 font-medium text-mist-100">{point.as_of}</p>
            <TooltipRow
              color={SERIES.leftover}
              label="Assets"
              value={String(assetSum(point))}
            />
            <TooltipRow
              color={SERIES.spending}
              label="Liabilities"
              value={String(liabilitySum(point))}
            />
            <p className="mt-1 border-t border-white/10 pt-1 text-mist-300">
              Net <span className="tabular">{formatMoney(point.net_worth)}</span>
            </p>
          </div>
        )}
      </div>
    </div>
  )
}

// The band specs describe which breakdown fields pile into each stack and in
// what order. Asset bands go up; liability bands go down. Within a stack the
// order is "most visible first" so cash sits on top of the asset stack (the
// band a reader scans first) and credit card debt on top of the liability
// stack.
const assetBandSpec: BandSpec[] = [
  { key: 'cash', pick: (b) => Number(b.cash) },
  { key: 'investments', pick: (b) => Number(b.investments) },
  { key: 'other', pick: (b) => Number(b.other_assets) + Number(b.manual_assets) },
]
const liabilityBandSpec: BandSpec[] = [
  { key: 'credit', pick: (b) => Number(b.credit_debt) },
  { key: 'loan', pick: (b) => Number(b.loan_debt) },
  { key: 'manual', pick: (b) => Number(b.manual_debt) },
]

type BreakdownView = NonNullable<NetWorthPoint['breakdown']>
interface BandSpec {
  key: string
  pick: (b: BreakdownView) => number
}

interface StackedBand {
  // Returns an SVG path for this band: a top edge across all points, then a
  // bottom edge back. `top` and `bottom` already carry the cumulative offset.
  area: (
    x: (i: number) => number,
    y: (v: number) => number,
  ) => string
}

/**
 * stackBands turns a per-point breakdown and a band spec into one stacked-band
 * path per spec entry. Each band's top edge is the cumulative total through
 * that band; its bottom edge is the cumulative total through the previous band
 * (zero for the first). Sums are display-only — every figure the reader sees
 * comes from the server.
 */
function stackBands(points: NetWorthPoint[], specs: BandSpec[]): StackedBand[] {
  // Cumulative top edges per band index across all points.
  const tops: number[][] = specs.map(() => new Array(points.length).fill(0))
  for (let s = 0; s < specs.length; s++) {
    for (let i = 0; i < points.length; i++) {
      const prev = s === 0 ? 0 : tops[s - 1][i]
      const b = points[i].breakdown
      tops[s][i] = prev + (b ? specs[s].pick(b) : 0)
    }
  }

  return specs.map((_, s) => ({
    area: (x: (i: number) => number, y: (v: number) => number) => {
      const top = tops[s]
      const bottom = s === 0 ? tops[0].map(() => 0) : tops[s - 1]
      // Forward along the top edge, then back along the bottom edge.
      const forward = top
        .map((v, i) => `${i === 0 ? 'M' : 'L'} ${x(i)} ${y(v)}`)
        .join(' ')
      const back = bottom
        .map((_v, i) => {
          const idx = points.length - 1 - i
          return `L ${x(idx)} ${y(bottom[idx])}`
        })
        .join(' ')
      return `${forward} ${back} Z`
    },
  }))
}

function assetSum(d: NetWorthPoint): number {
  if (!d.breakdown) return Number(d.assets_total)
  return (
    Number(d.breakdown.cash) +
    Number(d.breakdown.investments) +
    Number(d.breakdown.other_assets) +
    Number(d.breakdown.manual_assets)
  )
}

function liabilitySum(d: NetWorthPoint): number {
  if (!d.breakdown) return Number(d.liabilities_total)
  return (
    Number(d.breakdown.credit_debt) +
    Number(d.breakdown.loan_debt) +
    Number(d.breakdown.manual_debt)
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
 * A tick range that always contains zero and lands on round numbers. Net worth
 * spans both directions from zero, so the range is mirrored: the same magnitude
 * above and below, which is what keeps the asset and liability stacks at the
 * same scale.
 */
function axisRange(maxAbs: number): { ticks: number[]; niceMax: number } {
  if (maxAbs <= 0) return { ticks: [0], niceMax: 1000 }
  const step = niceStep(maxAbs / 3)
  const niceMax = Math.ceil(maxAbs / step) * step
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
  return count > 24 ? 4 : count > 12 ? 3 : count > 8 ? 2 : 1
}
