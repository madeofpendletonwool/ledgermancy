/**
 * The link to a category's detail page.
 *
 * Mirrors lib/merchants.ts, and lives here for the same reason: callers cannot
 * drift from the URL shape, and importing it does not pull a component module
 * into a page that only needs a string.
 *
 * A category id is a UUID, so unlike a merchant key it travels safely as a path
 * segment — merchant descriptors contain slashes and commas and cannot.
 */
export function categoryDetailPath(
  categoryID: string,
  range?: { from: string; to: string },
): string {
  const path = `/categories/${encodeURIComponent(categoryID)}`
  if (!range) return path
  const params = new URLSearchParams({ from: range.from, to: range.to })
  return `${path}?${params.toString()}`
}
