import { Link } from 'react-router-dom'
import { categoryDetailPath } from '../lib/categories'

/**
 * A category name that opens its breakdown.
 *
 * The category twin of MerchantLink. Every category name in the app was plain
 * text, and every category *click* (the bars on Spending, the Dashboard and the
 * merchant page) went to a filtered transaction list — which answers "which
 * charges" and never "how much, how often, trending which way, and to whom".
 *
 * The colour dot is the same identifier the charts use, so a category reads the
 * same whether it appears as a bar, a row or a link.
 */
export function CategoryLink({
  name,
  categoryID,
  color,
  range,
  showDot = false,
  className = '',
}: {
  name: string
  /** Empty/null renders plain text — the folded "Other" bar has no real id. */
  categoryID: string | null | undefined
  color?: string | null
  /** Carries the current window through, so the breakdown opens on the same period. */
  range?: { from: string; to: string }
  showDot?: boolean
  className?: string
}) {
  const dot = showDot ? (
    <span
      aria-hidden
      className="inline-block size-2 shrink-0 rounded-full"
      style={{ backgroundColor: color ?? 'rgba(255,255,255,0.25)' }}
    />
  ) : null

  // 'other' is CategoryBars' synthetic folded row, not a category anyone can open.
  if (!categoryID || categoryID === 'other') {
    return (
      <span className={`inline-flex items-center gap-2 ${className}`}>
        {dot}
        {name}
      </span>
    )
  }
  return (
    <Link
      to={categoryDetailPath(categoryID, range)}
      className={`inline-flex items-center gap-2 underline decoration-white/20 underline-offset-4 transition-colors hover:decoration-white/60 ${className}`}
      title={`See the ${name} breakdown`}
    >
      {dot}
      {name}
    </Link>
  )
}
