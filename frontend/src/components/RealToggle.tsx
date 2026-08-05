import { type Inflation } from '../lib/api'
import { realLabel } from '../lib/inflation'

/**
 * The nominal / real switch that sits above a long-horizon chart (doc 27).
 *
 * Two things it deliberately does NOT do.
 *
 * It does not render at all when there is no series, no base period, or the
 * window is too short — `shouldRender` decides that, and a toggle nobody can
 * meaningfully use is worse than no toggle.
 *
 * It does not hide what "real" means. The base period is in the control itself,
 * not in a tooltip: switching a chart to real changes what every number on it
 * means, and the label is the only thing that makes those numbers usable.
 */
export function RealToggle({
  enabled,
  onChange,
  inflation,
  shouldRender = true,
}: {
  enabled: boolean
  onChange: (v: boolean) => void
  inflation: Inflation | undefined
  shouldRender?: boolean
}) {
  const label = realLabel(inflation)
  if (!shouldRender || !label) return null

  return (
    <div className="flex flex-wrap items-center gap-2">
      <div className="flex gap-1" role="group" aria-label="Dollar basis">
        <Option active={!enabled} onClick={() => onChange(false)}>
          Nominal
        </Option>
        <Option active={enabled} onClick={() => onChange(true)}>
          Real
        </Option>
      </div>
      <span className="text-xs text-mist-500">
        {enabled ? label : 'dollars of the day each figure was recorded'}
      </span>
    </div>
  )
}

function Option({
  active,
  onClick,
  children,
}: {
  active: boolean
  onClick: () => void
  children: React.ReactNode
}) {
  return (
    <button
      type="button"
      aria-pressed={active}
      onClick={onClick}
      className={`rounded-lg px-3 py-1.5 text-sm transition ${
        active
          ? 'bg-white/10 text-mist-100'
          : 'text-mist-300 hover:bg-white/5 hover:text-mist-100'
      }`}
    >
      {children}
    </button>
  )
}

/**
 * The sentence that goes under a chart showing real figures.
 *
 * Carries the deflator, its freshness, and — when the series has holes — why
 * some points may be missing. A chart that quietly drops a point is
 * indistinguishable from a household that quietly stopped, which is why the
 * gaps are named rather than smoothed.
 */
export function RealBasis({
  enabled,
  inflation,
}: {
  enabled: boolean
  inflation: Inflation | undefined
}) {
  if (!enabled || !inflation?.available) return null

  return (
    <p className="mt-3 text-xs text-mist-500">
      Deflated by {inflation.series}, {realLabel(inflation)}.{' '}
      {inflation.stale && inflation.stale_note ? `${inflation.stale_note} ` : ''}
      {inflation.gaps.length > 0 && inflation.gap_note
        ? `${inflation.gap_note} Missing: ${inflation.gaps.join(', ')}.`
        : ''}
    </p>
  )
}
