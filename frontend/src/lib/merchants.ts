/**
 * The link to a merchant's detail page.
 *
 * Lives here rather than beside the route so callers cannot drift from the URL
 * shape, and so importing it does not pull a component module into a page that
 * only needs a string.
 *
 * The key is a RESOLVED merchant key — an entity id for a grouped merchant, the
 * raw descriptor otherwise. It is encoded, and it travels as a query parameter
 * rather than a path segment, because a raw descriptor routinely contains
 * characters that would otherwise break the URL: "ab* abebooks.co",
 * "park/the reserve fdesk", "etsy, inc.".
 */
export function merchantDetailPath(
  key: string,
  range?: { from: string; to: string },
): string {
  const params = new URLSearchParams({ key })
  if (range) {
    params.set('from', range.from)
    params.set('to', range.to)
  }
  return `/merchants/detail?${params.toString()}`
}
