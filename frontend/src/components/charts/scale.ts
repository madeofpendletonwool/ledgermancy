/**
 * Shared axis-scale helpers.
 *
 * These were copied verbatim into TrendChart and DayBars, and a third copy was
 * about to appear with the merchant chart. Axis ticks in particular are a place
 * where a subtle divergence between charts is invisible in review and obvious to a
 * user — two charts of the same money on the same page, stepping differently.
 *
 * ProjectionChart and Retirement keep their own compact-money variants on purpose:
 * one has to render negative balances with a sign, the other spans into millions.
 * They are different functions, not stale copies of this one.
 */

/**
 * Ticks that land on round numbers.
 *
 * Splitting the maximum into fixed fractions (max/4 and so on) puts ticks on
 * values like 1250 and 3750, which then render as "$1k, $3k, $4k" — a sequence
 * that skips $2k and reads as though a gridline is missing. Instead the step
 * itself is snapped to 1, 2, 2.5 or 5 times a power of ten, so every label is a
 * round number and the spacing between them is uniform.
 */
export function axisTicks(max: number): { ticks: number[]; niceMax: number } {
  if (max <= 0) return { ticks: [0, 25, 50, 75, 100], niceMax: 100 }

  const targetSteps = 4
  const rawStep = max / targetSteps
  const magnitude = 10 ** Math.floor(Math.log10(rawStep))
  const normalized = rawStep / magnitude

  const niceStep =
    magnitude *
    (normalized <= 1
      ? 1
      : normalized <= 2
        ? 2
        : normalized <= 2.5
          ? 2.5
          : normalized <= 5
            ? 5
            : 10)

  const niceMax = Math.ceil(max / niceStep) * niceStep

  const ticks: number[] = []
  for (let v = 0; v <= niceMax + niceStep / 2; v += niceStep) ticks.push(v)

  return { ticks, niceMax }
}

/** Axis-label money: no cents, thousands as "k". */
export function compactMoney(v: number): string {
  if (v === 0) return '$0'
  if (v >= 1000) {
    const k = v / 1000
    // Keep a decimal for steps like 2.5k rather than rounding two different
    // ticks onto the same label.
    return `$${Number.isInteger(k) ? k : k.toFixed(1)}k`
  }
  return `$${Number.isInteger(v) ? v : v.toFixed(0)}`
}

/** Thins x labels so they never overlap on a narrow chart. */
export function labelStride(count: number): number {
  return count > 12 ? 3 : count > 8 ? 2 : 1
}
