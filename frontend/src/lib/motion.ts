import type { TargetAndTransition, Transition } from 'motion/react'

/**
 * Non-hook motion helpers for staggered list entrances.
 *
 * Deliberately NOT a hook so it can be spread inside a `.map()` callback —
 * the row element is created in the loop body, where the rules of hooks
 * forbid calling `useReducedMotion()`. Instead we read the user's
 * `prefers-reduced-motion` preference at module load and keep it current via
 * a listener, so a change between mounts is reflected on the next render.
 *
 * `~25ms/item`, saturating at `400ms` so a list of fifty does not trickle in
 * for a second and a half. Under reduced motion the row renders at its final
 * state with no transition.
 */
const STAGGER_PER_ITEM = 0.025
const STAGGER_MAX = 0.4

/** Delay before item `index` enters, capped so long lists don't trickle. */
export function enterDelay(index: number): number {
  return Math.min(index * STAGGER_PER_ITEM, STAGGER_MAX)
}

const ENTER_INITIAL: TargetAndTransition = { opacity: 0, y: 6 }
const ENTER_ANIMATE: TargetAndTransition = { opacity: 1, y: 0 }
const ENTER_EASE: [number, number, number, number] = [0.16, 1, 0.3, 1]

function readReducedMotion(): boolean {
  return typeof window !== 'undefined' && 'matchMedia' in window
    ? window.matchMedia('(prefers-reduced-motion: reduce)').matches
    : false
}

// Read once at module load, then keep current. Entrance animations fire on
// mount, so this reflects the latest preference the next time a row appears.
let prefersReducedMotion = readReducedMotion()
if (typeof window !== 'undefined' && 'matchMedia' in window) {
  const mql = window.matchMedia('(prefers-reduced-motion: reduce)')
  mql.addEventListener('change', (e) => {
    prefersReducedMotion = e.matches
  })
}

export type EnterProps = {
  initial: false | TargetAndTransition
  animate: TargetAndTransition
  transition: Transition
}

/**
 * Motion props for a staggered list-row entrance. Spread onto a `motion.li`,
 * `motion.tr`, `motion.div`, etc. — the caller picks the tag so the list's
 * DOM stays valid (`<li>` inside `<ul>`, `<tr>` inside `<tbody>`).
 */
export function enterProps(index: number): EnterProps {
  if (prefersReducedMotion) {
    return { initial: false, animate: ENTER_ANIMATE, transition: { duration: 0 } }
  }
  return {
    initial: ENTER_INITIAL,
    animate: ENTER_ANIMATE,
    transition: { duration: 0.3, delay: enterDelay(index), ease: ENTER_EASE },
  }
}
