/**
 * A merchant monogram avatar — the app's default merchant imagery.
 *
 * Privacy-pure (MAD-39): a rounded square filled with a colour deterministic
 * from a hash of the merchant name, carrying the name's first letter/glyph.
 * Same name → same avatar, always; no network call, no third-party logo fetch.
 * (The opt-in logo fetcher is a separate issue and falls back to this.)
 *
 * The palette is NOT the chart palette. Charts reserve the validated series
 * tokens in `charts/tokens.ts`; avatars sit beside a text label where colour
 * is redundant (the app's own AllocationList/CategoryBars rule), so a curated
 * brand-adjacent palette is both allowed and warmer. Brand arcane-600 is the
 * first slot so a household full of "A" merchants still spreads across hues.
 *
 * Determinism, not beauty, is the hard requirement: two merchants that share a
 * resolved name must share an avatar, and that must hold across reloads and
 * browsers. FNV-1a gives that without depending on a crypto hash.
 */

/**
 * Curated 12-hue palette. All shades hold white text at large/bold sizes (the
 * only place the palette is used is behind a semibold glyph). Warm hues use the
 * 700-step so white-on-gold keeps its contrast; the brand violet is slot 0.
 */
const PALETTE = [
  '#7c3aed', // arcane-600 (brand violet)
  '#4f46e5', // indigo
  '#2563eb', // blue
  '#0891b2', // cyan
  '#0d9488', // teal
  '#15803d', // green
  '#a16207', // gold
  '#b45309', // amber
  '#dc2626', // red
  '#db2777', // pink
  '#c026d3', // fuchsia
  '#475569', // slate
] as const

/**
 * FNV-1a 32-bit. Stable across browsers and runs (no dependence on V8's
 * string-hash internals the way `Math.abs(str.hashCode())` would be in a
 * language with one). `Math.imul` keeps the multiply in 32-bit space.
 */
function hash32(s: string): number {
  let h = 0x811c9dc5 >>> 0
  for (let i = 0; i < s.length; i++) {
    h ^= s.charCodeAt(i)
    h = Math.imul(h, 0x01000193) >>> 0
  }
  return h >>> 0
}

/** Lowercased + trimmed so "Amazon" and "amazon " share an avatar. */
function monogramColor(name: string): string {
  return PALETTE[hash32(name.trim().toLowerCase()) % PALETTE.length]
}

/** First letter or digit, skipping leading punctuation/symbols (a raw
 * descriptor occasionally arrives quoted or prefixed). `Array.from` walks by
 * code point so an emoji-led name yields the emoji, not half a surrogate pair.
 */
function monogramLetter(name: string): string {
  const chars = Array.from(name.trim())
  const found = chars.find((c) => /[\p{L}\p{N}]/u.test(c))
  return (found ?? chars[0] ?? '?').toUpperCase()
}

export type MonogramSize = 'xs' | 'sm' | 'md' | 'lg' | 'xl'

/**
 * Box size, corner radius and glyph size move together so proportions hold.
 *
 * Exported because MerchantAvatar renders a fetched logo in the same slot when
 * the opt-in logo fetcher is on. A logo that is a pixel off the monogram it
 * replaces makes every list jump as images arrive, so the two must share one
 * definition rather than agreeing by hand.
 */
export const MONOGRAM_BOX: Record<MonogramSize, string> = {
  xs: 'size-5 rounded-md text-[11px]',
  sm: 'size-6 rounded-md text-xs',
  md: 'size-8 rounded-lg text-[15px]',
  lg: 'size-10 rounded-lg text-lg',
  xl: 'size-12 rounded-xl text-xl',
}

/**
 * Render-only. The merchant name is always shown as text beside the avatar, so
 * the glyph is decorative (`aria-hidden`) — it must never be the only copy of
 * a name on screen. Static by design: the avatar is imagery, not motion, and
 * nothing here is disabled under `prefers-reduced-motion` because nothing moves.
 */
export function Monogram({
  name,
  size = 'md',
  className = '',
}: {
  name: string
  size?: MonogramSize
  className?: string
}) {
  return (
    <span
      aria-hidden
      className={`inline-flex shrink-0 items-center justify-center font-semibold tracking-tight text-white/95 ${MONOGRAM_BOX[size]} ${className}`}
      style={{ backgroundColor: monogramColor(name) }}
    >
      {monogramLetter(name)}
    </span>
  )
}
