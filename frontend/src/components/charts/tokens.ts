/**
 * Chart color tokens.
 *
 * These are NOT the app's brand colors. They come from a categorical palette
 * validated with the data-viz validator against this app's chart surface
 * (#12101f), and every value here passed all six checks:
 *
 *   lightness band · chroma floor · CVD separation · normal-vision floor ·
 *   contrast vs surface
 *
 * The seeded per-category colors in the database (categories.color) failed
 * that validation badly — two of them sat at ΔE 6.7 for *normal* vision, i.e.
 * indistinguishable even without a colour-vision deficiency. Those remain in
 * use only as small chips beside a text label, where colour is redundant.
 * Charts use the tokens below. Do not swap one for the other without re-running
 * the validator.
 */

/** Categorical slots, in fixed order. Never cycle past the last one. */
export const SERIES = {
  /** slot 1 — blue */
  income: '#3987e5',
  /** slot 2 — orange */
  spending: '#d95926',
  /** slot 3 — aqua */
  leftover: '#199e70',
} as const

/**
 * Single-series fill. A bar chart of one measure across categories is ONE
 * series, so every bar gets one colour — colouring each bar differently would
 * make hue carry no information.
 */
export const SINGLE_SERIES = '#9085e9'

/** Reserved status colours. Never reused as a categorical slot. */
export const STATUS = {
  good: '#0ca30c',
  warning: '#fab219',
  serious: '#ec835a',
  critical: '#d03b3b',
} as const

/** Recessive chart furniture. */
export const CHART = {
  grid: 'rgba(255,255,255,0.07)',
  axis: 'rgba(255,255,255,0.15)',
  textPrimary: '#e8e6f5',
  textSecondary: '#b3aed0',
  textMuted: '#7b749c',
  /** The surface gap drawn between adjacent fills. */
  surface: '#12101f',
} as const
