import type { TrendPoint } from '../../lib/api'
import { SERIES } from './tokens'
import { ChartBoundary } from './ChartBoundary'

/**
 * A thumb-sized, no-axes spend line — orients this month against its arc.
 *
 * One measure (spending) over time, so it wears the spending slot and nothing
 * else: no grid, no ticks, no tooltip. The detail lives on the Spending page;
 * this is just the silhouette, sized to sit under a headline figure.
 */
function SpendSparklineUnguarded({ data }: { data: TrendPoint[] }) {
  if (data.length < 2) return null

  const W = 120
  const H = 32
  const PAD = 2

  const values = data.map((d) => Number(d.spending))
  const max = Math.max(...values, 0)
  const min = Math.min(...values, 0)
  const span = max - min || 1

  const x = (i: number) => PAD + (i / (data.length - 1)) * (W - PAD * 2)
  const y = (v: number) => PAD + (H - PAD * 2) - ((v - min) / span) * (H - PAD * 2)

  const path = data
    .map((d, i) => `${i === 0 ? 'M' : 'L'} ${x(i).toFixed(1)} ${y(Number(d.spending)).toFixed(1)}`)
    .join(' ')

  const last = data.length - 1

  return (
    <svg
      viewBox={`0 0 ${W} ${H}`}
      className="mt-2 h-8 w-full max-w-[140px]"
      role="img"
      aria-label="Spending over the trailing twelve months"
      preserveAspectRatio="none"
    >
      <path d={path} fill="none" stroke={SERIES.spending} strokeWidth={1.5} vectorEffect="non-scaling-stroke" />
      <circle cx={x(last)} cy={y(values[last])} r={1.8} fill={SERIES.spending} vectorEffect="non-scaling-stroke" />
    </svg>
  )
}

// The export is the guarded chart: a throw inside costs the reader the chart,
// not the page (MAD-61).
export function SpendSparkline(props: Parameters<typeof SpendSparklineUnguarded>[0]) {
  return (
    <ChartBoundary label="spend sparkline">
      <SpendSparklineUnguarded {...props} />
    </ChartBoundary>
  )
}
