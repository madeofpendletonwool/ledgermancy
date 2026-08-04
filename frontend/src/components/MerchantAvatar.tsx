import { useEffect, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { api } from '../lib/api'
import { Monogram, MONOGRAM_BOX, type MonogramSize } from './Monogram'

/**
 * A merchant's avatar: its real logo when the opt-in fetcher has one, and the
 * deterministic monogram in every other case.
 *
 * The monogram is the default and stays that way (MAD-39). This component adds
 * exactly one thing on top of it — an `<img>` pointing at THIS app, never at a
 * logo host. The bytes were fetched server-side by a worker and cached; the
 * browser makes an ordinary same-origin request and has no idea a third party
 * was ever involved. That is the property the whole feature is built to keep,
 * so nothing here should ever grow an external URL.
 *
 * Every path back to the monogram is silent and identical, because they are the
 * same outcome to a reader: the operator never enabled the feature, the
 * household switched it off, this merchant has no resolved key, the logo was
 * never fetched, or the image failed to load. A merchant with no logo is not an
 * error state and must never look like one.
 */
export function MerchantAvatar({
  name,
  merchantKey,
  size = 'md',
  className = '',
}: {
  /** What the monogram falls back to, and the image's accessible name source. */
  name: string
  /**
   * The RESOLVED merchant key, the same one MerchantLink takes. The logo cache
   * is keyed by it, so a raw descriptor would miss for exactly the merchants a
   * household has bothered to group. Null or undefined renders the monogram.
   */
  merchantKey?: string | null
  size?: MonogramSize
  className?: string
}) {
  // One query for the whole page: react-query dedupes by key, so a list of
  // fifty rows asks once. Capabilities does not change while a tab is open.
  const { data: capabilities } = useQuery({
    queryKey: ['capabilities'],
    queryFn: api.capabilities,
    staleTime: Infinity,
  })

  // A logo that 404s or fails to decode falls back for the rest of this mount.
  // Keyed off merchantKey so a recycled row (virtualised lists, pagination)
  // does not inherit the previous merchant's failure.
  const [failed, setFailed] = useState(false)
  useEffect(() => setFailed(false), [merchantKey])

  if (!capabilities?.merchant_logos_enabled || !merchantKey || failed) {
    return <Monogram name={name} size={size} className={className} />
  }

  return (
    <img
      src={api.merchantLogoUrl(merchantKey)}
      alt=""
      aria-hidden
      loading="lazy"
      // `object-contain` on a padded tile rather than `object-cover`: a logo is
      // artwork with its own aspect ratio, and cropping one to a square is how
      // you end up showing the middle third of a wordmark. The faint tile keeps
      // a dark logo from vanishing into the page.
      className={`shrink-0 bg-white/5 object-contain p-1 ${MONOGRAM_BOX[size]} ${className}`}
      onError={() => setFailed(true)}
    />
  )
}
