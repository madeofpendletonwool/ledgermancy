import { useEffect } from 'react'
import { useLocation, useOutlet } from 'react-router-dom'
import {
  AnimatePresence,
  animate,
  motion,
  useMotionValue,
  useReducedMotion,
  useTransform,
} from 'motion/react'
import { formatMoney } from '../lib/money'

/**
 * Motion primitives for the app: an animated headline number and a
 * crossfading route outlet. The staggered list-entrance helper lives in
 * `lib/motion.ts` (it is a plain function, not a hook, so it can be spread
 * inside a `.map()`).
 *
 * Library: `motion` (motion.dev) — the actively-maintained successor to
 * Framer Motion, which now redirects here. Chosen over `framer-motion` for
 * continued maintenance and a smaller surface, with first-party React 19
 * support. This is the dependency that makes the README's "Motion" stack
 * line honest.
 *
 * Three hard rules every primitive here honours:
 *
 *  - Money is decimal strings. `AnimatedNumber` only ever tweens a *display*
 *    value — the target comes straight from the server's decimal string and
 *    is formatted on every frame. It never sums, divides, or invents a
 *    headline.
 *  - Tabular figures are preserved: the number renders inside the caller's
 *    existing `.tabular` stack, so the numeric face and `tabular-nums` carry
 *    through the count-up.
 *  - Everything degrades to static under `prefers-reduced-motion`.
 */

const COUNT_DURATION = 0.7
// A soft "back-out" ease so a figure settles with a little weight rather than
// a linear trickle: the last few percent ease in quickly so the final value
// lands without floating-point wobble at rest.
const COUNT_EASE: [number, number, number, number] = [0.16, 1, 0.3, 1]

/** Default formatter: a tweened number → a currency string. */
function defaultFormat(n: number): string {
  return formatMoney(String(n))
}

function toNumber(value: unknown): number {
  if (typeof value === 'number') return value
  if (value === null || value === undefined || value === '') return NaN
  const n = Number(value)
  return Number.isFinite(n) ? n : NaN
}

/**
 * A headline number that counts from its previous value to the new one on
 * mount and whenever `value` changes (e.g. when the month selector moves).
 *
 * Pass the server's raw decimal string (or a number). The number is tweened
 * and formatted on each frame; the headline itself is never recomputed here.
 * When `value` is not a finite number the `fallback` is shown verbatim — the
 * ordinary "no data yet" case renders as it always did.
 *
 * Under `prefers-reduced-motion` the target is rendered instantly.
 */
export function AnimatedNumber({
  value,
  format,
  fallback = '—',
  duration = COUNT_DURATION,
}: {
  value: string | number | null | undefined
  format?: (n: number) => string
  fallback?: string
  duration?: number
}) {
  const fmt = format ?? defaultFormat
  const reduce = useReducedMotion()
  const target = toNumber(value)

  // The motion value persists across renders, so a *change* in `value` tweens
  // from the currently-displayed figure to the new one. On first mount (with
  // animation) it starts at 0, which is the count-up itself.
  const mv = useMotionValue(0)
  const display = useTransform(mv, (n) => fmt(n))

  useEffect(() => {
    if (!Number.isFinite(target)) return
    if (reduce) {
      mv.set(target)
      return
    }
    const controls = animate(mv, target, { duration, ease: COUNT_EASE })
    return () => controls.stop()
  }, [target, reduce, duration, mv])

  if (!Number.isFinite(target)) return <span>{fallback}</span>
  if (reduce) return <span>{fmt(target)}</span>
  return <motion.span>{display}</motion.span>
}

/**
 * A crossfading `<Outlet/>`. Page changes fade (~150ms) instead of
 * hard-swapping.
 *
 * Uses `useOutlet()` rather than `<Outlet/>` so the exiting page is a frozen
 * snapshot of the route element — `<Outlet/>` reads router context live, so a
 * naïve keyed wrapper would show the *new* page in both the entering and
 * exiting copies. `useOutlet()` returns the element for the current match,
 * which AnimatePresence preserves per key.
 *
 * `mode="popLayout"` pops the exiting page out of flow so the entering page
 * takes its place and the two overlap cleanly during the 150ms crossfade,
 * regardless of differing page heights.
 *
 * Under `prefers-reduced-motion` the outlet renders with no motion wrapper,
 * so route changes are instant.
 */
export function AnimatedOutlet() {
  const outlet = useOutlet()
  const { pathname } = useLocation()
  const reduce = useReducedMotion()

  if (reduce) return <>{outlet}</>

  return (
    <AnimatePresence mode="popLayout" initial={false}>
      <motion.div
        key={pathname}
        initial={{ opacity: 0 }}
        animate={{ opacity: 1 }}
        exit={{ opacity: 0 }}
        transition={{ duration: 0.15, ease: 'easeOut' }}
      >
        {outlet}
      </motion.div>
    </AnimatePresence>
  )
}
