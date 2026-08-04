import { useState } from 'react'
import type { SpendingHeatmapCategory } from '../../lib/api'
import { formatMoney } from '../../lib/money'
import { CategoryIcon } from '../CategoryIcon'
import { CHART, SINGLE_SERIES } from './tokens'

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
export function SpendingHeatmap({
  months,
  categories,
}: {
  months: string[]
  categories: SpendingHeatmapCategory[]
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

  // The intensity ramp's ceiling is the single largest cell across the rendered
  // rows, so one big month does not flatten every other cell to the same tint.
  // Computed in the browser ONLY to scale colour; the dollar a reader pulls off
  // a cell comes straight from the server's decimal string.
  let peak = 0
  for (const r of rows) {
    for (const m of months) {
      const v = Number(r.cells[m] ?? 0)
      if (v > peak) peak = v
    }
  }

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
            >
              {shortMonth(m)}
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
                const ratio = peak > 0 ? value / peak : 0
                const dim =
                  hover !== null &&
                  (hover.row !== row || hover.col !== col)
                return (
                  <rect
                    key={m}
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
        <span className="ml-2">peak {formatMoney(String(peak))} / cell</span>
      </div>
    </div>
  )
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
