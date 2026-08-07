import type { ReactNode } from 'react'

/**
 * Loading skeletons (MAD-37).
 *
 * Each shape is a greyed placeholder of *the thing about to load*, not a
 * generic spinner: glass-card tiles for the headline rows, row placeholders for
 * lists and tables, a faint chart frame for chart sections. They share the
 * `.skeleton` ink surface (see `index.css`), so they read as recessed panels
 * consistent with `.glass` rather than flat light-gray blocks.
 *
 * Skeletons are deliberately STATIC. No shimmer, no pulse — the only motion is
 * the crossfade (`.reveal`) when real content takes over, which snaps under
 * `prefers-reduced-motion`.
 *
 * Use `<Reveal>` to wrap the real content at a `isPending ? <Skeleton/> :
 * <Content/>` hand-off so the arrival fades in.
 */

/** A single recessed block. Width/height come from the caller's className. */
export function SkeletonBlock({ className = '' }: { className?: string }) {
  return <div className={`skeleton ${className}`} aria-hidden="true" />
}

/**
 * One headline tile placeholder, shaped like the `Tile`/`StatTile` family used
 * on every route: label line, a large figure, a small hint.
 */
function SkeletonTile() {
  return (
    <div className="glass p-5">
      <SkeletonBlock className="h-3.5 w-24" />
      <SkeletonBlock className="mt-3 h-7 w-32" />
      <SkeletonBlock className="mt-3 h-3 w-20" />
    </div>
  )
}

/**
 * A row of headline tile placeholders. `count` defaults to 4 (the most common
 * tile grid) — pass 3 for Net worth / Budgets.
 */
export function SkeletonTiles({ count = 4 }: { count?: number }) {
  return (
    <>
      {Array.from({ length: count }, (_, i) => (
        <SkeletonTile key={i} />
      ))}
    </>
  )
}

/**
 * A glass card placeholder for a single highlighted panel (Safe-to-spend, the
 * monthly recap, a projection card) — taller than a tile, with a header line
 * and two wider lines.
 */
export function SkeletonCard() {
  return (
    <div className="glass p-6">
      <SkeletonBlock className="h-4 w-40" />
      <SkeletonBlock className="mt-4 h-8 w-56" />
      <SkeletonBlock className="mt-5 h-3 w-full" />
      <SkeletonBlock className="mt-2 h-3 w-2/3" />
    </div>
  )
}

/**
 * A faint chart frame: a heading line plus an axis-and-plot area the right
 * aspect for the hand-rolled SVG charts. It evokes "a chart is about to land
 * here" without committing to any particular series.
 */
export function SkeletonChart() {
  return (
    <div>
      <SkeletonBlock className="h-3.5 w-32" />
      <SkeletonBlock className="skeleton-sunken mt-5 h-44 w-full" />
    </div>
  )
}

/**
 * Row placeholders for lists and tables. Each row is a short line plus a wider
 * line, evoking the merchant/category/transaction rows that fill these routes.
 */
export function SkeletonRows({ count = 5 }: { count?: number }) {
  return (
    <div className="space-y-3">
      {Array.from({ length: count }, (_, i) => (
        <div key={i} className="flex items-center gap-4">
          <SkeletonBlock className="h-4 flex-1" />
          <SkeletonBlock className="h-4 w-20 shrink-0" />
        </div>
      ))}
    </div>
  )
}

/**
 * A few lines of body text — for a paragraph that is loading (an AI recap, a
 * document body, a preferences explanation).
 */
export function SkeletonText({ lines = 2 }: { lines?: number }) {
  return (
    <div className="space-y-2">
      {Array.from({ length: lines }, (_, i) => (
        <SkeletonBlock key={i} className={`h-3 ${i === lines - 1 ? 'w-2/3' : 'w-full'}`} />
      ))}
    </div>
  )
}

/**
 * A full detail-page skeleton (Merchant / Category detail): a back link, a
 * title, a tile row, a chart section, and a couple of list rows. These two
 * routes render a single `Loading…` line while their one query resolves, so the
 * placeholder sketches the whole page they are about to show.
 */
export function SkeletonDetail() {
  return (
    <>
      <SkeletonBlock className="h-3 w-20" />
      <SkeletonBlock className="mt-3 h-7 w-64" />
      <SkeletonBlock className="mt-2 h-3 w-80" />
      <div className="mt-6 grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <SkeletonTiles count={4} />
      </div>
      <div className="glass mt-6 p-6">
        <SkeletonChart />
      </div>
      <div className="glass mt-6 p-6">
        <SkeletonRows count={4} />
      </div>
    </>
  )
}

/**
 * The placeholder for a route whose *code* has not arrived yet — the Suspense
 * fallback for the lazily-imported screens (MAD-65), not for their data.
 *
 * Deliberately vaguer than the per-route skeletons above: at this point the app
 * knows which path it is going to, but nothing has run, so committing to a
 * specific layout would guess wrong on most screens. A title and a tile row is
 * the shape every screen here starts with.
 *
 * This is usually invisible. Navigating within the app happens inside a
 * transition, so React keeps the current screen on-screen until the chunk
 * resolves; and once the service worker has precached, chunks come off disk.
 * It shows on a cold load of a deep link, which is exactly when a blank main
 * area would look broken.
 */
export function SkeletonPage() {
  return (
    <>
      <SkeletonBlock className="h-7 w-56" />
      <SkeletonBlock className="mt-2 h-3 w-72" />
      <div className="mt-6 grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <SkeletonTiles count={4} />
      </div>
      <div className="glass mt-6 p-6">
        <SkeletonRows count={4} />
      </div>
    </>
  )
}

/**
 * Fades real content in when it replaces a skeleton. Only fires on mount, so at
 * a `isPending ? <Skeleton/> : <Reveal><Content/></Reveal>` hand-off it animates
 * exactly once — when the data first arrives. Under `prefers-reduced-motion` the
 * `.reveal` class has no animation (see `index.css`), so the swap is instant.
 */
export function Reveal({ children }: { children: ReactNode }) {
  return <div className="reveal">{children}</div>
}
