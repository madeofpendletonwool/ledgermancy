import { useSyncExternalStore } from 'react'

/**
 * `prefers-reduced-motion`, with no dependencies.
 *
 * This is the eager, motion-library-free twin of `useReducedMotion` from
 * `motion/react`. It exists so the app shell can decide whether to load the
 * ~140 kB animation library *before* paying for it: a user who has asked for
 * reduced motion gets the plain, unanimated shell and never fetches `motion`
 * at all (MAD-65). Everyone else fetches it lazily, off the first-paint path.
 *
 * Reactive: toggling the OS setting re-renders subscribers, so a shell that
 * chose the plain branch will pick the animated one (and pull the chunk) the
 * moment the preference clears, and vice versa.
 */
const QUERY = '(prefers-reduced-motion: reduce)'

function subscribe(callback: () => void): () => void {
  if (typeof window === 'undefined' || !('matchMedia' in window)) return () => {}
  const mql = window.matchMedia(QUERY)
  mql.addEventListener('change', callback)
  return () => mql.removeEventListener('change', callback)
}

function getSnapshot(): boolean {
  if (typeof window === 'undefined' || !('matchMedia' in window)) return false
  return window.matchMedia(QUERY).matches
}

function getServerSnapshot(): boolean {
  return false
}

export function usePrefersReducedMotion(): boolean {
  return useSyncExternalStore(subscribe, getSnapshot, getServerSnapshot)
}
