import { Link } from 'react-router-dom'
import { merchantDetailPath } from '../lib/merchants'

/**
 * A merchant name that opens its breakdown.
 *
 * Exists because five surfaces now need the same affordance and each had
 * hand-rolled it (or, more often, rendered plain text). The two rules worth
 * centralising are easy to get wrong per-site:
 *
 *   * The key must be the RESOLVED one. A raw descriptor addresses one fragment
 *     of a grouped merchant, so linking with it strands exactly the merchants a
 *     household has bothered to group.
 *   * No key means no link. A merchant we cannot address must not look clickable —
 *     the Dashboard's top-merchant card has always guarded this, and a link to
 *     nowhere is worse than plain text.
 */
export function MerchantLink({
  name,
  merchantKey,
  range,
  variant = 'inline',
  className = '',
}: {
  /** What to show. Usually merchant_name ?? name. */
  name: string
  /**
   * The RESOLVED merchant key — `merchant_key_resolved` on a transaction, or
   * `merchant_key` on anything the reports API returns. Empty, null or undefined
   * renders plain text.
   */
  merchantKey: string | null | undefined
  /** Carries the current window through, so the breakdown opens on the same period. */
  range?: { from: string; to: string }
  variant?: 'inline' | 'chip'
  className?: string
}) {
  const style = variant === 'chip' ? CHIP : INLINE

  if (!merchantKey) {
    return <span className={`${style} ${className}`}>{name}</span>
  }
  return (
    <Link
      to={merchantDetailPath(merchantKey, range)}
      className={`${style} ${LINK} ${className}`}
      title={`See everything from ${name}`}
    >
      {name}
    </Link>
  )
}

// Matching the treatment already used for merchant links in the recurring table,
// so a merchant name reads the same wherever it appears.
const INLINE = 'truncate'
const CHIP = ''
const LINK =
  'underline decoration-white/20 underline-offset-4 transition-colors hover:decoration-white/60'
