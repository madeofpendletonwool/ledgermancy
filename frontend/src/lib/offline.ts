/**
 * Offline state for the UI.
 *
 * The service worker will happily serve a saved API response when the network
 * is gone. That is the feature — and it is also the only real risk this whole
 * thing introduces, because a balance from this morning looks exactly like a
 * balance from now. So the worker stamps every stored response with the time it
 * was stored, and this module collects those stamps so the UI can say, without
 * ambiguity, *which* moment the figures on screen belong to.
 */

import { useSyncExternalStore } from 'react'

/** Set by the service worker on every cached response. Keep in sync with `src/sw.ts`. */
const CACHED_AT_HEADER = 'x-ledgermancy-cached-at'

export interface OfflineState {
  /** The browser's own reachability guess. False is reliable; true is not. */
  online: boolean
  /**
   * When the oldest saved response now on screen was fetched, ISO-8601, or
   * null when everything rendered came from the network.
   *
   * The *oldest*, not the newest: it is the honest bound on how stale the
   * screen is. One card refreshed a minute ago does not make the page current.
   */
  servingCachedSince: string | null
}

let state: OfflineState = {
  online: typeof navigator === 'undefined' ? true : navigator.onLine,
  servingCachedSince: null,
}

const listeners = new Set<() => void>()

function setState(next: Partial<OfflineState>) {
  const merged = { ...state, ...next }
  if (
    merged.online === state.online &&
    merged.servingCachedSince === state.servingCachedSince
  ) {
    return
  }
  state = merged
  for (const listener of listeners) listener()
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener)
  return () => listeners.delete(listener)
}

function getSnapshot(): OfflineState {
  return state
}

if (typeof window !== 'undefined') {
  window.addEventListener('online', () => {
    // Coming back does not by itself refresh anything on screen, but the next
    // response will be live, and holding a "saved data" banner over a page
    // that is about to refetch reads as a stuck error.
    setState({ online: true, servingCachedSince: null })
  })
  window.addEventListener('offline', () => setState({ online: false }))
}

/**
 * Records where one API response came from.
 *
 * Called for every response the client receives; the presence of the stamp is
 * what distinguishes "the worker answered from disk" from "the server
 * answered". A live response clears the banner, because at that point the
 * network is demonstrably working.
 */
export function noteResponseOrigin(res: Response): void {
  const cachedAt = res.headers.get(CACHED_AT_HEADER)

  if (!cachedAt) {
    if (state.servingCachedSince !== null) setState({ servingCachedSince: null })
    return
  }

  const existing = state.servingCachedSince
  if (existing === null || cachedAt < existing) {
    setState({ servingCachedSince: cachedAt })
  }
}

/** Current offline state, re-rendering the caller when it changes. */
export function useOfflineState(): OfflineState {
  return useSyncExternalStore(subscribe, getSnapshot, getSnapshot)
}

/**
 * Whether the app can currently reach the server.
 *
 * Use this to disable controls that write. `navigator.onLine` false is
 * trustworthy — the browser knows it has no route. True only means "a network
 * interface exists", so this is a floor on connectivity, not a promise; writes
 * are also guarded centrally in `api.ts`.
 */
export function useOnline(): boolean {
  return useOfflineState().online
}

/**
 * Tooltip for a control switched off because the network is gone.
 *
 * Shared so every disabled write says the same thing. A control that is greyed
 * out for no stated reason reads as a bug in the app rather than a fact about
 * the train tunnel.
 */
export const OFFLINE_WRITE_HINT =
  "You're offline — this can't be saved until you reconnect."

/** True once the app is running as an installed PWA rather than in a tab. */
export function isStandalone(): boolean {
  if (typeof window === 'undefined') return false
  return (
    window.matchMedia('(display-mode: standalone)').matches ||
    // iOS Safari predates the display-mode media query and still uses this.
    (navigator as Navigator & { standalone?: boolean }).standalone === true
  )
}

/**
 * Drops every saved API response.
 *
 * Called on sign-out and on sign-in. Saved figures belong to whoever was
 * signed in when they were fetched, and a shared device must not let the next
 * person read the previous user's balances by pulling the plug.
 */
export async function clearApiCache(): Promise<void> {
  setState({ servingCachedSince: null })

  if (typeof navigator === 'undefined' || !('serviceWorker' in navigator)) return

  try {
    const registration = await navigator.serviceWorker.getRegistration()
    // `.active` rather than `.controller`: on the very first load the worker is
    // installed but not yet controlling this page, and a sign-out in that
    // window still has to clear.
    registration?.active?.postMessage({ type: 'CLEAR_API_CACHE' })
  } catch {
    // A browser with service workers disabled has no cache to clear.
  }
}
