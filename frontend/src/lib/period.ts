/**
 * The trailing-window helpers the detail and explorer views share.
 *
 * These lived in MerchantDetail.tsx and were copied into Merchants.tsx; a third
 * copy for the category page was the point to stop. Four hand-rolled versions of
 * "the last twelve months" is four chances for one page to disagree with another
 * about which months it is showing.
 *
 * Every date here is a local calendar date formatted "YYYY-MM-DD", never a
 * timestamp. Postgres DATE serialises as midnight UTC, so building these with
 * toISOString() would shift the boundary a day west of UTC and move
 * month-boundary charges into the wrong month.
 */

/** A local calendar date as "YYYY-MM-DD". */
export function iso(d: Date): string {
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(
    d.getDate(),
  ).padStart(2, '0')}`
}

export interface DateRange {
  from: string
  to: string
}

/** Window options, longest last — a merchant's story is usually a year-shaped one. */
export const WINDOWS = [
  { label: 'Last 3 months', months: 3 },
  { label: 'Last 6 months', months: 6 },
  { label: 'Last 12 months', months: 12 },
  { label: 'Last 24 months', months: 24 },
] as const

/**
 * The trailing `months` calendar months, ending with the whole current month.
 * Whole months rather than "today minus N days" so the monthly bars line up with
 * the range they are drawn over.
 */
export function windowRange(months: number): DateRange {
  const now = new Date()
  const to = new Date(now.getFullYear(), now.getMonth() + 1, 0)
  const from = new Date(now.getFullYear(), now.getMonth() - (months - 1), 1)
  return { from: iso(from), to: iso(to) }
}

/** Default window: the trailing twelve months. */
export function defaultRange(): DateRange {
  return windowRange(12)
}

/**
 * Months spanned by the range, floored at 1. Used for per-month averages, so an
 * occasional merchant is not overstated by dividing only by the months it
 * happened to appear in — the same reasoning as GetCategoryAverages.
 */
export function monthsInRange(from: string, to: string): number {
  const [fy, fm] = from.split('-').map(Number)
  const [ty, tm] = to.split('-').map(Number)
  if (!fy || !fm || !ty || !tm) return 1
  return Math.max(1, (ty - fy) * 12 + (tm - fm) + 1)
}

/** Which preset window the current range corresponds to, for the window select. */
export function matchedWindow(from: string, to: string): number {
  const span = monthsInRange(from, to)
  const match = WINDOWS.find((w) => w.months === span)
  return match ? match.months : 12
}
