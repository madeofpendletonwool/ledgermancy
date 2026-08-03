import { useState } from 'react'
import type { SpendingHeatmapCategory } from '../../lib/api'
import { formatMoney } from '../../lib/money'
import { CHART, SINGLE_SERIES } from './tokens'

/**
 * Small multiples cap. The issue calls for "~8 tiny lines": each line gets its
 * own mini-axis, so they are not overlaid, which is the one honest place for
 * this many "series". Identity rides on each panel's label, never on hue.
 */
const MAX_PANELS = 8

/**
 * Per-category spend over time as small multiples: one tiny line chart per
 * category, each against its OWN mini-axis.
 *
 * Small multiples are one of the few honest places for several "series",
 * because the lines are not overlaid — each panel is its own coordinate space,
 * so a category's seasonal swing reads against its own range rather than
 * against a global max that flattens every other panel. Hue stays single-series
 * throughout (one measure, one colour); identity is the panel label.
 *
 * Rides the same payload as the heatmap (item #8); the two are different
 * renderings of one round trip.
 */
export function CategoryMultiples({
  months,
  categories,
}: {
  months: string[]
  categories: SpendingHeatmapCategory[]
}) {
  const [hover, setHover] = useState<{ panel: number; month: number } | null>(null)

  if (categories.length === 0 || months.length === 0) {
    return (
      <p className="py-8 text-center text-sm" style={{ color: CHART.textMuted }}>
        No per-category history yet.
      </p>
    )
  }

  // Top N by whole-range total — the categories that earn a panel of their own.
  // The remainder are intentionally NOT folded to "Other" here: a summed line
  // across heterogeneous categories is a meaningless squiggle, and small
  // multiples' value is per-category shape.
  const panels = categories.slice(0, MAX_PANELS)

  // Columns wraps the grid on narrow viewports; two columns keeps each panel
  // wide enough for a twelve-month line to read.
  return (
    <div className="grid gap-3 sm:grid-cols-2">
      {panels.map((c, panel) => (
        <MultiplesPanel
          key={c.slug + '-' + c.category_id}
          category={c}
          months={months}
          panelIndex={panel}
          hover={hover}
          setHover={setHover}
        />
      ))}
    </div>
  )
}

const PANEL_W = 320
const PANEL_H = 96
const PANEL_PAD = { top: 8, right: 10, bottom: 16, left: 10 }
const PLOT_W = PANEL_W - PANEL_PAD.left - PANEL_PAD.right
const PLOT_H = PANEL_H - PANEL_PAD.top - PANEL_PAD.bottom

function MultiplesPanel({
  category,
  months,
  panelIndex,
  hover,
  setHover,
}: {
  category: SpendingHeatmapCategory
  months: string[]
  panelIndex: number
  hover: { panel: number; month: number } | null
  setHover: (h: { panel: number; month: number } | null) => void
}) {
  // Per-panel y-axis: each line scales to its own peak, which is the whole
  // point of small multiples. Tallied in the client only to fit the line into
  // its panel; the figure a reader pulls off a point is the server's decimal.
  const values = months.map((m) => Number(category.cells[m] ?? 0))
  const peak = Math.max(...values, 0)

  const x = (i: number) =>
    months.length === 1
      ? PANEL_PAD.left + PLOT_W / 2
      : PANEL_PAD.left + (i / (months.length - 1)) * PLOT_W
  const y = (v: number) =>
    PANEL_PAD.top + PLOT_H - (peak > 0 ? (v / peak) * PLOT_H : 0)

  const hasData = peak > 0
  // A line needs at least two non-zero points to be a line; a single month or
  // a flat-zero category renders as an empty panel rather than a misleading
  // dot or a flat line through the axis.
  const nonZero = values.filter((v) => v > 0).length

  const path =
    hasData && nonZero >= 1
      ? months
          .map((m, i) => `${i === 0 ? 'M' : 'L'} ${x(i)} ${y(Number(category.cells[m] ?? 0))}`)
          .join(' ')
      : ''

  // Stride month labels so they never collide in a panel this narrow: at twelve
  // months label the first, middle and last; at fewer, label the ends.
  const labelStride = months.length > 8 ? 4 : months.length > 4 ? 2 : 1

  const hoverPoint =
    hover && hover.panel === panelIndex ? category.cells[months[hover.month]] ?? '0' : null

  return (
    <div
      className="rounded-xl border border-white/5 p-3"
      style={{ backgroundColor: 'rgba(255,255,255,0.02)' }}
    >
      <div className="mb-1 flex items-baseline justify-between gap-2">
        <span className="truncate text-xs font-medium text-mist-200">{category.name}</span>
        <span className="tabular shrink-0 text-[11px] text-mist-500">
          {formatMoney(category.total)}
        </span>
      </div>

      <div className="relative">
        <svg
          viewBox={`0 0 ${PANEL_W} ${PANEL_H}`}
          className="w-full"
          role="img"
          aria-label={`${category.name} spend over time`}
          onMouseLeave={() => setHover(null)}
        >
          {/* A recessive baseline at zero so the panel's floor is visible even
              when a line rides high above it. */}
          <line
            x1={PANEL_PAD.left}
            x2={PANEL_W - PANEL_PAD.right}
            y1={y(0)}
            y2={y(0)}
            stroke={CHART.grid}
            strokeWidth={1}
          />

          {path && (
            <path d={path} fill="none" stroke={SINGLE_SERIES} strokeWidth={1.5} />
          )}

          {/* A dot on each month that had spend, so a single isolated charge
              still has a mark rather than vanishing between two zero line
              endpoints. */}
          {hasData &&
            months.map((m, i) =>
              Number(category.cells[m] ?? 0) > 0 ? (
                <circle
                  key={m}
                  cx={x(i)}
                  cy={y(Number(category.cells[m] ?? 0))}
                  r={hover && hover.panel === panelIndex && hover.month === i ? 3 : 1.8}
                  fill={SINGLE_SERIES}
                  stroke={CHART.surface}
                  strokeWidth={1}
                />
              ) : null,
            )}

          {/* Month labels, thinned to fit the panel width. */}
          {months.map((m, i) =>
            i % labelStride === 0 || i === months.length - 1 ? (
              <text
                key={m}
                x={x(i)}
                y={PANEL_H - 4}
                textAnchor={i === 0 ? 'start' : i === months.length - 1 ? 'end' : 'middle'}
                fontSize="9"
                fill={CHART.textMuted}
              >
                {shortMonth(m)}
              </text>
            ) : null,
          )}

          {/* Hover hit targets, one per month column. */}
          {months.map((m, i) => (
            <rect
              key={m}
              x={x(i) - PLOT_W / months.length / 2}
              y={PANEL_PAD.top}
              width={PLOT_W / months.length}
              height={PLOT_H}
              fill="transparent"
              onMouseEnter={() => setHover({ panel: panelIndex, month: i })}
            />
          ))}
        </svg>

        {hoverPoint && Number(hoverPoint) > 0 && (
          <div
            className="pointer-events-none absolute -top-1 rounded-md border border-white/10 bg-ink-900/95 px-2 py-1 text-[10px] shadow-lg backdrop-blur"
            style={{
              left: `${((x(hover!.month) / PANEL_W) * 100).toFixed(1)}%`,
              transform: 'translateX(-50%)',
            }}
          >
            <span className="tabular text-mist-100">{formatMoney(hoverPoint)}</span>
          </div>
        )}
      </div>

      {!hasData && (
        <p className="py-4 text-center text-[11px]" style={{ color: CHART.textMuted }}>
          No spend in this range.
        </p>
      )}
    </div>
  )
}

function shortMonth(key: string): string {
  const [, m] = key.split('-').map(Number)
  return new Date(2026, m - 1, 1).toLocaleDateString('en-US', { month: 'short' })
}
