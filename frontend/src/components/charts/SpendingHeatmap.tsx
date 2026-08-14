import { useState } from 'react'
import type { SpendingHeatmapCategory } from '../../lib/api'
import { formatDate, formatMoney } from '../../lib/money'
import { CategoryIcon } from '../CategoryIcon'
import { CHART, SINGLE_SERIES } from './tokens'
import { ChartBoundary } from './ChartBoundary'
import { PartialMonthTooltipNote } from './partialMonth'

/**
 * The cap on real-category rows before the remainder folds into a synthetic
 * "Other". The app's fold rule is "past ~8"; a heatmap's cells are compact (no
 * per-row bar art or label stride to crowd), so a couple more fit comfortably
 * and the rule's intent — "don't grow indefinitely" — is preserved.
 */
const MAX_CATEGORIES = 10

const ROW_H = 26
const LABEL_W = 116
const MONTH_W = 44
const MONTH_LABEL_H = 22
const VALUE_GUTTER = 76
const PAD = { top: 8, right: 8, bottom: 8, left: 8 }

/**
 * Spending by category × month, as an intensity grid.
 *
 * Cell colour is a SINGLE-HUE intensity ramp on the single-series hue — a legit
 * exception to "single solid fill", because here hue IS encoding magnitude,
 * which is information. The ramp stays single-hue (violet) rather than a
 * rainbow, matching the validated-palette rule and the rest of the app's chart
 * surface. Category identity rides on the row label, never on colour.
 *
 * Cells with no spend in a month render as the recessive surface tint, so an
 * empty month reads as "nothing" rather than as a missing column. Past the row
 * cap the tail folds into "Other" exactly as CategoryBars does, so the heatmap
 * and the period bar chart agree on what the top categories are.
 */
function SpendingHeatmapUnguarded({
  months,
  categories,
  inProgressMonth,
  asOf,
}: {
  months: string[]
  categories: SpendingHeatmapCategory[]
  /** The one month in `months` that has not finished. */
  inProgressMonth?: string
  asOf?: string
}) {
  const [hover, setHover] = useState<{ row: number; col: number } | null>(null)

  if (categories.length === 0 || months.length === 0) {
    return (
      <p className="py-12 text-center text-sm" style={{ color: CHART.textMuted }}>
        No spending in this range yet.
      </p>
    )
  }

  const rows = foldToOther(categories, MAX_CATEGORIES)

  // The cells allowed to SET the ramp: whole months only. A part-month column is
  // one artificially small cell in every row at once, and the ceiling below is a
  // quantile — feeding it a whole column of small values drags it down and clips
  // real months that should have fitted under it.
  //
  // Those cells are still DRAWN against the ramp, at their true values. They are
  // excluded from setting the scale, not from the chart.
  const scaleValues: number[] = []
  for (const r of rows) {
    for (const m of months) {
      if (m === inProgressMonth) continue
      scaleValues.push(Number(r.cells[m] ?? 0))
    }
  }

  // The ramp's ceiling. Computed in the browser ONLY to scale colour; the dollar
  // a reader pulls off a cell comes straight from the server's decimal string.
  const { ceiling, peak, clipped } = rampCeiling(scaleValues)

  const innerW = LABEL_W + months.length * MONTH_W + VALUE_GUTTER
  const innerH = MONTH_LABEL_H + rows.length * ROW_H
  const W = PAD.left + PAD.right + innerW
  const H = PAD.top + PAD.bottom + innerH

  const cellX = (col: number) => PAD.left + LABEL_W + col * MONTH_W
  const cellY = (row: number) => PAD.top + MONTH_LABEL_H + row * ROW_H

  const hoverRow = hover ? rows[hover.row] : null
  const hoverMonth = hover ? months[hover.col] : null
  const hoverValue =
    hoverRow && hoverMonth ? hoverRow.cells[hoverMonth] ?? '0' : null

  return (
    <div className="space-y-3">
      <div className="chart-scroll relative overflow-x-auto">
        <span className="sr-only">This chart scrolls horizontally — swipe to see the rest.</span>
        <svg
          viewBox={`0 0 ${W} ${H}`}
          className="w-full"
          style={{ minWidth: `${W}px` }}
          role="img"
          aria-label="Spending by category and month"
          onMouseLeave={() => setHover(null)}
        >
          {/* Month labels across the top, one per column. Twelve short-month
              labels fit at this width; thinned vertically there is nothing to
              thin — every column is a category-month pair the reader names by
              hovering. */}
          {months.map((m, i) => (
            <text
              key={m}
              x={cellX(i) + MONTH_W / 2}
              y={PAD.top + MONTH_LABEL_H - 6}
              textAnchor="middle"
              fontSize="10"
              fill={CHART.textMuted}
              fontStyle={m === inProgressMonth ? 'italic' : undefined}
            >
              {m === inProgressMonth ? `${shortMonth(m)}*` : shortMonth(m)}
            </text>
          ))}

          {rows.map((r, row) => (
            <g key={r.slug + '-' + row}>
              {/* Row label. Colour is redundant (identity is the text), so it
                  wears the secondary text token rather than the category chip
                  colour — which would imply hue encodes identity here, when it
                  encodes magnitude. */}
              <text
                x={PAD.left + LABEL_W - 8}
                y={cellY(row) + ROW_H / 2 + 4}
                textAnchor="end"
                fontSize="11"
                fill={CHART.textSecondary}
              >
                {truncate(r.name, 16)}
              </text>

              {months.map((m, col) => {
                const value = Number(r.cells[m] ?? 0)
                const ratio = ceiling > 0 ? value / ceiling : 0
                const dim =
                  hover !== null &&
                  (hover.row !== row || hover.col !== col)
                return (
                  <g key={m}>
                    <rect
                      x={cellX(col) + 1}
                      y={cellY(row) + 2}
                      width={MONTH_W - 2}
                      height={ROW_H - 4}
                      rx={3}
                      fill={intensityFill(ratio)}
                      opacity={dim ? 0.55 : 1}
                      style={{ cursor: 'pointer' }}
                      onMouseEnter={() => setHover({ row, col })}
                    />
                    {/* A cell past the ceiling wears the darkest tint like any
                        other saturated cell, so the corner notch is the only
                        thing that distinguishes "at the top of the ramp" from
                        "off the top of it". Without it the clipping would be
                        invisible, which is the dishonest version of this fix. */}
                    {value > ceiling && (
                      <path
                        d={offRampNotch(cellX(col) + 1, cellY(row) + 2, MONTH_W - 2)}
                        fill={CHART.textPrimary}
                        opacity={dim ? 0.4 : 0.75}
                        pointerEvents="none"
                      />
                    )}
                  </g>
                )
              })}

              {/* Row total — the ranking figure, on the right so it does not
                  compete with the row label. */}
              <text
                x={cellX(months.length) + 8}
                y={cellY(row) + ROW_H / 2 + 4}
                fontSize="11"
                fill={CHART.textMuted}
                className="tabular"
              >
                {formatMoney(r.total)}
              </text>
            </g>
          ))}
        </svg>

        {hoverRow && hoverMonth && hoverValue !== null && (
          <div
            className="pointer-events-none absolute top-2 rounded-xl border border-white/10 bg-ink-900/95 px-3 py-2 text-xs shadow-xl backdrop-blur"
            style={{
              left: `${((cellX(hover!.col) + MONTH_W / 2) / W) * 100}%`,
              transform: 'translateX(-50%)',
            }}
          >
            <p className="mb-0.5 flex items-center gap-1.5 font-medium text-mist-100">
              <CategoryIcon
                slug={hoverRow.slug}
                name={hoverRow.name}
                color={hoverRow.color}
                className="size-3.5 shrink-0"
              />
              {hoverRow.name}
            </p>
            <p className="text-mist-300">{shortMonth(hoverMonth, true)}</p>
            <p className="tabular mt-1 text-mist-100">
              {Number(hoverValue) > 0 ? formatMoney(hoverValue) : 'No spend'}
            </p>
            {hoverMonth === inProgressMonth && (
              <PartialMonthTooltipNote asOf={asOf} />
            )}
            {Number(hoverValue) > ceiling && (
              <p className="mt-1 border-t border-white/10 pt-1 text-[11px] text-mist-500">
                Above the colour ramp&rsquo;s top — the tint is capped, the figure
                is not.
              </p>
            )}
          </div>
        )}
      </div>

      {/* The intensity ramp explained, once, so a reader does not have to guess
          what "darker" means. One legend for the whole grid, not per cell. */}
      <div
        className="flex items-center gap-2 text-[11px]"
        style={{ color: CHART.textMuted }}
      >
        <span>Less</span>
        <span
          className="inline-block h-2.5 w-24 rounded-full"
          style={{
            background: `linear-gradient(to right, ${intensityFill(0)}, ${intensityFill(
              1,
            )})`,
          }}
        />
        <span>More</span>
        <span className="ml-2">
          ramp tops out at {formatMoney(String(ceiling))} / cell
        </span>
        {clipped > 0 && (
          <span>
            · {clipped} cell{clipped === 1 ? '' : 's'} above it (marked), to{' '}
            {formatMoney(String(peak))}
          </span>
        )}
        {inProgressMonth && (
          <span>
            · *{shortMonth(inProgressMonth)} is still running
            {asOf ? `, through ${formatDate(asOf)}` : ''}
          </span>
        )}
      </div>
    </div>
  )
}

/**
 * The share of cells the ramp is allowed to reach its top on. Cells above the
 * resulting ceiling are drawn saturated and marked rather than stretching it.
 *
 * 0.95 rather than something more aggressive because clipping is a cost, not a
 * feature: every clipped cell is one the reader can no longer rank by eye. At
 * 95% a twelve-by-ten grid clips about six cells — enough to absorb a genuine
 * one-off, not enough to flatten a category that is simply expensive.
 */
const RAMP_QUANTILE = 0.95

/**
 * The intensity ramp's ceiling, winsorised.
 *
 * The ramp used to top out at the single largest cell in the grid. One
 * knowingly-paid-off $18k loan then set the ceiling for everything, and a $514
 * grocery month rendered at 3% intensity — indistinguishable from an empty cell.
 * The chart's promise ("darker cells are bigger months; seasonality and creep
 * show as a row darkening left to right") became unreadable for every row except
 * the one containing the outlier (MAD-110).
 *
 * Taking the ceiling at a high quantile instead restores the ramp's resolution
 * across the cells a reader actually compares, and it self-heals: no flag to
 * set, no toggle to find. The outliers do not disappear — they saturate, get a
 * corner notch, and are counted in the legend — so the chart still says "there
 * is something bigger here than the ramp can show" rather than quietly clipping.
 *
 * Rejected alternatives, so this is not relitigated:
 *
 *   - Per-row normalisation. Matches the "row darkening left to right" reading
 *     exactly, but breaks the cross-row one: a $50 cell in a small category
 *     would render as dark as a $10k cell in a large one, and the grid stops
 *     being one comparable surface.
 *   - A log or sqrt ramp. Compresses the outlier without clipping anything, but
 *     makes colour non-linear in dollars — which quietly falsifies the chart's
 *     stated contract that colour carries magnitude. Wrong trade here.
 *
 * Returns the ceiling, the true peak (for the legend), and how many cells sit
 * above the ceiling. An empty or all-zero grid yields a zero ceiling, which the
 * caller already handles as "every cell at the base tint".
 */
function rampCeiling(values: number[]): {
  ceiling: number
  peak: number
  clipped: number
} {
  const positive = values.filter((v) => v > 0).sort((a, b) => a - b)
  if (positive.length === 0) return { ceiling: 0, peak: 0, clipped: 0 }

  const peak = positive[positive.length - 1]
  // Nearest-rank quantile: no interpolation, so the ceiling is always a real
  // cell value and the legend quotes a figure that exists in the data.
  const idx = Math.min(
    positive.length - 1,
    Math.max(0, Math.ceil(RAMP_QUANTILE * positive.length) - 1),
  )
  const ceiling = positive[idx]

  return { ceiling, peak, clipped: positive.filter((v) => v > ceiling).length }
}

/** The corner wedge marking a cell whose value is past the ramp's ceiling. */
function offRampNotch(x: number, y: number, w: number): string {
  const size = 5
  const right = x + w
  return `M${right - size} ${y}H${right}V${y + size}Z`
}

/**
 * Keeps the top N categories and sums the remainder into a single "Other" row,
 * matching CategoryBars' foldToOther. The folded row's cells sum across every
 * tail category per month — summed here in the client ONLY to fill the grid;
 * the period totals the reader compares against come from the server.
 */
function foldToOther(
  categories: SpendingHeatmapCategory[],
  max: number,
): SpendingHeatmapCategory[] {
  if (categories.length <= max) return categories

  const head = categories.slice(0, max - 1)
  const tail = categories.slice(max - 1)

  const cells: Record<string, string> = {}
  let total = 0
  for (const c of tail) {
    for (const [m, v] of Object.entries(c.cells)) {
      const summed = Number(cells[m] ?? 0) + Number(v)
      cells[m] = summed.toFixed(2)
      total += Number(v)
    }
  }

  return [
    ...head,
    {
      category_id: 'other',
      name: `Other (${tail.length})`,
      slug: 'other',
      color: null,
      is_fixed: false,
      total: total.toFixed(2),
      cells,
    },
  ]
}

/**
 * The single-hue intensity ramp: a linear interpolation from the chart surface
 * toward the single-series hue. Perceptually this reads as "lightness of one
 * hue" — the same hue throughout, varying only in intensity — which is the
 * ramp the issue calls for and the one the validated-palette rule permits.
 *
 * `ratio` is clamped to [0, 1]; the very low end is pinned above zero so a
 * "no spend" cell still renders a visible tile rather than vanishing into the
 * page background (a 0.04 base tint reads as "empty cell", not as "missing").
 */
function intensityFill(ratio: number): string {
  const r = Math.max(0, Math.min(1, ratio))
  if (r === 0) return `${SINGLE_SERIES}0a` // ~4% on the dark surface
  // Lerp from a dim floor toward the full single-series hue.
  return mixHex(surfaceTint(), SINGLE_SERIES, 0.12 + r * 0.88)
}

// The chart surface as a colour, used as the cold end of the ramp. Defined here
// rather than read out of CHART (an rgba string) so the lerp stays in opaque
// hex — interpolating an rgba is the one place a stray alpha channel would
// quietly darken every cell.
function surfaceTint(): string {
  return '#1d1a2e'
}

/** Linear interpolation between two hex colours. Returns an opaque hex string. */
function mixHex(a: string, b: string, t: number): string {
  const pa = parseHex(a)
  const pb = parseHex(b)
  const r = Math.round(pa[0] + (pb[0] - pa[0]) * t)
  const g = Math.round(pa[1] + (pb[1] - pa[1]) * t)
  const bl = Math.round(pa[2] + (pb[2] - pa[2]) * t)
  return `#${toHex(r)}${toHex(g)}${toHex(bl)}`
}

function parseHex(h: string): [number, number, number] {
  const s = h.replace('#', '')
  return [
    parseInt(s.slice(0, 2), 16),
    parseInt(s.slice(2, 4), 16),
    parseInt(s.slice(4, 6), 16),
  ]
}

function toHex(n: number): string {
  return n.toString(16).padStart(2, '0')
}

function shortMonth(key: string, long = false): string {
  const [y, m] = key.split('-').map(Number)
  return new Date(y, m - 1, 1).toLocaleDateString('en-US', {
    month: 'short',
    year: long ? '2-digit' : undefined,
  })
}

function truncate(s: string, n: number): string {
  return s.length <= n ? s : s.slice(0, n - 1) + '…'
}

// The export is the guarded chart: a throw inside costs the reader the chart,
// not the page (MAD-61).
export function SpendingHeatmap(props: Parameters<typeof SpendingHeatmapUnguarded>[0]) {
  return (
    <ChartBoundary label="spending heatmap">
      <SpendingHeatmapUnguarded {...props} />
    </ChartBoundary>
  )
}
