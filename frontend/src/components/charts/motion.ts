import type { TargetAndTransition, Transition } from 'motion/react'

/**
 * Chart motion helpers — draw-in on mount and morph on data change for the
 * hand-rolled SVG charts. Reuses the `motion` (motion.dev) library the rest of
 * the app already ships (see `components/motion.tsx`); this module is the
 * chart-specific vocabulary on top of it.
 *
 * Two animation kinds, matching the two ways a chart mark is drawn:
 *
 *  - Stroked lines (no fill) draw in via `pathLength`, which motion turns into
 *    a stroke-dashoffset internally. When the line's `d` changes, motion
 *    tweens between matching point sequences, so the line morphs to its new
 *    shape instead of snapping. (`pathLength` manages `stroke-dasharray`
 *    itself, so it is only used on SOLID strokes — a dashed reference line
 *    would lose its dash pattern; those use `lineFade`.)
 *
 *  - Filled marks (bars, areas) cannot use `pathLength` because they are
 *    filled, not stroked. Rect bars animate their SVG `height` and `y`
 *    attributes (grow from the baseline); path bars animate a `scaleY`
 *    transform anchored at the baseline via `transform-box: fill-box`.
 *
 * Every helper takes a `reduce` flag (from `useReducedMotion()`, read ONCE per
 * chart and passed down) so it stays a plain function — safe to call inside a
 * `.map()`, where the rules of hooks forbid calling `useReducedMotion`
 * per-item. Under reduced motion each returns the final state with a
 * zero-duration transition: the chart renders exactly as it did before motion
 * existed.
 *
 * Durations sit in the 400–600ms band the design calls for, shared between
 * entrance and morph so a period change feels like one coherent motion.
 */

const DRAW_DURATION = 0.5
const BAR_DURATION = 0.5
const EASE: [number, number, number, number] = [0.16, 1, 0.3, 1]

/** Shared return shape: spread onto a `motion.*` element as initial/animate/transition. */
export type MotionDraw = {
  initial: false | TargetAndTransition
  animate: TargetAndTransition
  transition: Transition
}

const INSTANT: Transition = { duration: 0 }

/**
 * A solid stroked line that draws in on mount and morphs when its `d` changes.
 *
 * `pathLength` drives the entrance (motion sets the dasharray/offset); `d` is
 * in `animate` so a data change tweens the geometry. `opacity` is included in
 * the entrance only so the path is hidden on the very first paint, before
 * motion has committed its dasharray — without it the fully-drawn line would
 * flash for a frame before drawing back in.
 */
export function lineDraw(d: string, reduce: boolean): MotionDraw {
  if (reduce) {
    return { initial: false, animate: { d }, transition: INSTANT }
  }
  return {
    initial: { pathLength: 0, opacity: 0 },
    animate: { pathLength: 1, opacity: 1, d },
    transition: { duration: DRAW_DURATION, ease: EASE },
  }
}

/**
 * A line that cannot use `pathLength` — typically a dashed reference or
 * estimate line, whose `stroke-dasharray` must be preserved. Fades in on mount
 * and morphs its `d` on change. `delay` staggers it behind a primary draw-in.
 */
export function lineFade(d: string, reduce: boolean, delay = 0): MotionDraw {
  if (reduce) {
    return { initial: false, animate: { d }, transition: INSTANT }
  }
  return {
    initial: { opacity: 0 },
    animate: { opacity: 1, d },
    transition: { duration: DRAW_DURATION, ease: EASE, delay },
  }
}

/**
 * A filled rect bar that grows from the baseline on mount and morphs its
 * height/top when the data changes.
 *
 * Animates the SVG `height` attribute and `y` (via `attrY` — motion reserves
 * the shorthand `x`/`y` for transforms, so the SVG attribute needs `attrX`/
 * `attrY`). On mount both start at the baseline (`height: 0`, `y: baseline`)
 * and grow to the bar's top; on a data change they tween straight to the new
 * geometry. `baseline` is the chart's zero line (PAD.top + PLOT_H).
 */
export function barRect(
  top: number,
  height: number,
  baseline: number,
  reduce: boolean,
): MotionDraw {
  if (reduce) {
    return { initial: false, animate: { attrY: top, height }, transition: INSTANT }
  }
  return {
    initial: { attrY: baseline, height: 0 },
    animate: { attrY: top, height },
    transition: { duration: BAR_DURATION, ease: EASE },
  }
}

/**
 * A filled path bar (rounded data-end, square baseline) that grows from the
 * baseline on mount and morphs its `d` on change.
 *
 * `scaleY` from 0→1 grows the bar out of its baseline; `transform-box:
 * fill-box` + `originY: 1` (set by the caller as `style`) anchor that scale at
 * the bar's bottom edge. `d` is in `animate` so a window change tweens the
 * geometry rather than snapping it.
 */
export function barPath(d: string, reduce: boolean): MotionDraw {
  if (reduce) {
    return { initial: false, animate: { d }, transition: INSTANT }
  }
  return {
    initial: { scaleY: 0 },
    animate: { scaleY: 1, d },
    transition: { duration: BAR_DURATION, ease: EASE },
  }
}

/**
 * A background tint (area between two lines, an overdraft band) that fades in
 * on mount. These marks carry sign, not detail, so they fade rather than draw;
 * their points are not tweened (motion cannot interpolate a `points` string),
 * which is fine because at low opacity a reshaped tint is imperceptible.
 */
export function areaFade(toOpacity: number, reduce: boolean): MotionDraw {
  if (reduce) {
    return { initial: false, animate: { opacity: toOpacity }, transition: INSTANT }
  }
  return {
    initial: { opacity: 0 },
    animate: { opacity: toOpacity },
    transition: { duration: DRAW_DURATION, ease: EASE },
  }
}

/**
 * A horizontal fill (CategoryBars) that grows from the left on mount and
 * morphs its width when the mix changes. Width is animated as a percentage so
 * it tracks a responsive track.
 */
export function barWidth(pct: number, reduce: boolean): MotionDraw {
  const target = `${Math.max(pct, 0.5)}%`
  if (reduce) {
    return { initial: false, animate: { width: target }, transition: INSTANT }
  }
  return {
    initial: { width: 0 },
    animate: { width: target },
    transition: { duration: BAR_DURATION, ease: EASE },
  }
}
