import { useState } from 'react'
import type { CategorySpend } from '../../lib/api'
import { formatMoney } from '../../lib/money'
import { CHART, SINGLE_SERIES } from './tokens'

const MAX_BARS = 8

/**
 * Spending by category.
 *
 * A horizontal bar chart of one measure across a categorical dimension: that is
 * ONE series, so every bar carries the same colour. Category identity comes
 * from the axis label, not hue. Anything past the top few folds into "Other"
 * rather than growing the chart indefinitely.
 */
export function CategoryBars({
  data,
  onSelect,
}: {
  data: CategorySpend[]
  /** Clicking a real category (not the folded "Other" row) calls this. */
  onSelect?: (categoryId: string) => void
}) {
  const [hovered, setHovered] = useState<string | null>(null)

  if (data.length === 0) {
    return <Empty>No spending in this period.</Empty>
  }

  const bars = foldToOther(data, MAX_BARS)
  const max = Math.max(...bars.map((b) => Number(b.total)))

  return (
    <div className="space-y-2.5">
      {bars.map((bar) => {
        const value = Number(bar.total)
        // Guard the divide: a period whose largest category is 0 would
        // otherwise produce NaN widths.
        const pct = max > 0 ? (value / max) * 100 : 0
        const isHovered = hovered === bar.slug
        const clickable = onSelect !== undefined && bar.slug !== 'other'

        return (
          <div
            key={bar.slug}
            className={`group grid grid-cols-[10rem_1fr_6rem] items-center gap-3 text-sm ${
              clickable ? 'cursor-pointer' : ''
            }`}
            onMouseEnter={() => setHovered(bar.slug)}
            onMouseLeave={() => setHovered(null)}
            onClick={clickable ? () => onSelect(bar.category_id) : undefined}
            title={clickable ? `See ${bar.name} transactions` : undefined}
          >
            <span className="truncate text-mist-300" title={bar.name}>
              {bar.name}
            </span>

            {/* Track sits at low contrast so the bar itself carries the signal. */}
            <div className="relative h-6 overflow-hidden rounded-md bg-white/5">
              <div
                className="h-full rounded-r-[4px] transition-[filter]"
                style={{
                  width: `${Math.max(pct, 0.5)}%`,
                  backgroundColor: SINGLE_SERIES,
                  filter: isHovered ? 'brightness(1.15)' : undefined,
                }}
              />
              {isHovered && (
                <span
                  className="pointer-events-none absolute inset-y-0 right-2 flex items-center text-xs"
                  style={{ color: CHART.textSecondary }}
                >
                  {bar.transaction_count} txn
                  {bar.transaction_count === 1 ? '' : 's'}
                </span>
              )}
            </div>

            {/* Values wear text tokens, never the series colour. */}
            <span className="tabular text-right text-mist-100">
              {formatMoney(bar.total)}
            </span>
          </div>
        )
      })}
    </div>
  )
}

/**
 * Keeps the top N categories and sums the remainder into a single "Other" row,
 * so the chart never grows past its readable series cap and the total still
 * adds up.
 */
function foldToOther(data: CategorySpend[], max: number): CategorySpend[] {
  if (data.length <= max) return data

  const head = data.slice(0, max - 1)
  const tail = data.slice(max - 1)

  // Summed for display only; every figure that feeds analysis is computed
  // server-side in exact decimal.
  const otherTotal = tail.reduce((sum, c) => sum + Number(c.total), 0)
  const otherCount = tail.reduce((sum, c) => sum + c.transaction_count, 0)

  return [
    ...head,
    {
      category_id: 'other',
      name: `Other (${tail.length} categories)`,
      slug: 'other',
      color: null,
      is_fixed: false,
      total: otherTotal.toFixed(2),
      transaction_count: otherCount,
    },
  ]
}

function Empty({ children }: { children: React.ReactNode }) {
  return (
    <p className="py-8 text-center text-sm" style={{ color: CHART.textMuted }}>
      {children}
    </p>
  )
}
