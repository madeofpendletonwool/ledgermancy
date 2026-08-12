import { formatDate } from '../../lib/money'
import { CHART } from './tokens'

/**
 * The shared treatment for the ONE month a trailing chart is still inside.
 *
 * The problem it solves is not the same as the savings-rate chart's. There, the
 * in-progress month's value is a ratio that does not exist yet, so it is dropped.
 * Here the value is an AMOUNT — dollars actually spent, exact and complete up to
 * today — so dropping it would be its own lie. The failure mode is subtler: a
 * twelve-bar chart whose last bar is a third of its neighbours reads as "spending
 * collapsed this month" when it means "this month is a third over" (MAD-110).
 *
 * So the bar stays at its true height and gets hatched. Hatching is the right
 * encoding because it does not touch the value — it overlays texture on the fill
 * the series already owns, so hue still means what the legend says it means and
 * the bar still measures what the axis says it measures. Anything that changed
 * the height (annualising, projecting to month-end) would be inventing a number.
 */

/** The single pattern id, so one <defs> serves every chart on a page. */
export const PARTIAL_HATCH_ID = 'partial-month-hatch'

/**
 * The <defs> entry the hatch references. Render once inside a chart's <svg>.
 *
 * Diagonal lines in the recessive furniture colour, over transparent, so the
 * pattern layers ON TOP of the series fill rather than replacing it. Kept faint:
 * the bar's job is still to be read as a bar.
 */
export function PartialMonthHatch() {
  return (
    <defs>
      <pattern
        id={PARTIAL_HATCH_ID}
        width={6}
        height={6}
        patternUnits="userSpaceOnUse"
        patternTransform="rotate(45)"
      >
        <line x1={0} y1={0} x2={0} y2={6} stroke={CHART.surface} strokeWidth={2.5} opacity={0.55} />
      </pattern>
    </defs>
  )
}

/**
 * The hatch overlay for one bar. Draw immediately after the bar it covers, with
 * the same geometry. Pointer events stay off so it never steals a hover from the
 * hit target beneath it.
 */
export function PartialMonthOverlay({
  x,
  y,
  width,
  height,
  rx = 0,
}: {
  x: number
  y: number
  width: number
  height: number
  rx?: number
}) {
  if (height <= 0 || width <= 0) return null
  return (
    <rect
      x={x}
      y={y}
      width={width}
      height={height}
      rx={rx}
      fill={`url(#${PARTIAL_HATCH_ID})`}
      pointerEvents="none"
    />
  )
}

/**
 * The line a tooltip adds when the hovered month is the one in flight. Says the
 * figures are month-to-date and names the day they run through, because "so far"
 * without a date is not a window anyone can check.
 */
export function PartialMonthTooltipNote({ asOf }: { asOf?: string }) {
  return (
    <p className="mt-1 border-t border-white/10 pt-1 text-[11px] text-mist-500">
      Month in progress{asOf ? ` — through ${formatDate(asOf)}` : ''}
    </p>
  )
}
