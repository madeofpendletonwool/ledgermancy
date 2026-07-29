/// <reference lib="webworker" />

/**
 * Ledgermancy service worker.
 *
 * Two jobs, and the distinction between them is load-bearing:
 *
 *  1. Precache the app shell (hashed JS/CSS/icons) so an installed app opens
 *     instantly and opens at all without a network.
 *  2. Keep a copy of the *most recent* read-only API responses so the figures
 *     the user saw ten minutes ago are still there when the train goes into a
 *     tunnel — clearly labelled as saved, never presented as current.
 *
 * The shell is cache-first because it is versioned by build hash and cannot go
 * stale. API data is network-first and *never* cache-first: a stale balance
 * rendered as a live one is worse than an honest error, so the cache is only
 * ever reached when the network genuinely failed.
 */

import { ExpirationPlugin } from 'workbox-expiration'
import {
  cleanupOutdatedCaches,
  createHandlerBoundToURL,
  precacheAndRoute,
} from 'workbox-precaching'
import { NavigationRoute, registerRoute } from 'workbox-routing'
import { NetworkFirst } from 'workbox-strategies'
import type { WorkboxPlugin } from 'workbox-core'

declare const self: ServiceWorkerGlobalScope

const API_CACHE = 'ledgermancy-api-v1'

/**
 * Stamped onto every stored API response. The UI reads it back to say *when*
 * the numbers are from, which is the difference between an honest offline mode
 * and a lie.
 *
 * Keep in sync with `CACHED_AT_HEADER` in `src/lib/offline.ts`.
 */
const CACHED_AT_HEADER = 'x-ledgermancy-cached-at'

/**
 * GET paths whose responses may be kept for offline reading.
 *
 * An allowlist, not a denylist, and deliberately so: a route added to the API
 * later gets no offline copy until someone chooses to add it here. The failure
 * mode of forgetting is "this screen is empty offline", which is honest. The
 * failure mode of a denylist is silently caching something that should never
 * have been on disk.
 *
 * Notable exclusions:
 *   /api/auth/*      — session and identity. Caching `me` would let an expired
 *                      session keep rendering the app; see lib/session.tsx for
 *                      the offline story, which does not involve this cache.
 *   /api/documents/* — the vault decrypts on read. Its whole point is that tax
 *                      returns and receipts are encrypted at rest, and writing
 *                      the plaintext into a browser cache would undo that.
 *   /api/export/*    — CSV downloads, not screens.
 *   /api/chat        — a streamed AI response is not a document to replay.
 *   /api/preferences, /api/household/* — small, and not worth a stale copy.
 */
const CACHEABLE_API_PATHS = [
  '/api/accounts',
  '/api/alerts',
  '/api/budgets',
  '/api/capabilities',
  '/api/categories',
  '/api/goals',
  '/api/holdings',
  '/api/insights',
  '/api/investments',
  '/api/liabilities',
  '/api/manual-assets',
  '/api/merchants',
  '/api/networth',
  '/api/obligations',
  // The linked-institution list, so the Accounts page renders offline. The
  // rest of /api/plaid (link tokens, exchange, sync) is POST-only and would
  // not match a GET route regardless.
  '/api/plaid/items',
  '/api/projections',
  '/api/reports',
  '/api/transactions',
]

/** Exact segment match, so `/api/accounts-secret` never rides in on `/api/accounts`. */
function isCacheableApiPath(pathname: string): boolean {
  return CACHEABLE_API_PATHS.some(
    (prefix) => pathname === prefix || pathname.startsWith(prefix + '/'),
  )
}

const stampCachedAt: WorkboxPlugin = {
  // Runs just before a response is written to the cache. Doubles as the
  // cacheability check: returning null stores nothing, so redirects, 401s and
  // 5xx never masquerade as saved data.
  cacheWillUpdate: async ({ response }) => {
    if (response.status !== 200 || response.type === 'opaque') return null

    const headers = new Headers(response.headers)
    headers.set(CACHED_AT_HEADER, new Date().toISOString())
    return new Response(await response.blob(), {
      status: response.status,
      statusText: response.statusText,
      headers,
    })
  },
}

registerRoute(
  ({ url, sameOrigin }) => sameOrigin && isCacheableApiPath(url.pathname),
  new NetworkFirst({
    cacheName: API_CACHE,
    // No networkTimeoutSeconds on purpose. A timeout would serve saved figures
    // while the network is merely slow, which blurs the one guarantee worth
    // having here: a cached response means the request actually failed.
    plugins: [
      stampCachedAt,
      new ExpirationPlugin({
        maxEntries: 160,
        maxAgeSeconds: 7 * 24 * 60 * 60, // a week; older money data is noise
        purgeOnQuotaError: true,
      }),
    ],
  }),
  'GET',
)

// --- App shell --------------------------------------------------------------

precacheAndRoute(self.__WB_MANIFEST)

// Drops precaches left by previous Workbox versions so a long-installed client
// does not accumulate dead caches across upgrades.
cleanupOutdatedCaches()

// Deep links like /net-worth are client-side routes with no file behind them,
// so every navigation resolves to the precached shell. The denylist keeps the
// API, webhooks and the health check on the network where they belong — a
// navigation to /api/... must not be answered with index.html.
registerRoute(
  new NavigationRoute(createHandlerBoundToURL('index.html'), {
    denylist: [/^\/api\//, /^\/webhooks\//, /^\/healthz$/],
  }),
)

// --- Lifecycle --------------------------------------------------------------

self.addEventListener('message', (event) => {
  const type = (event.data as { type?: string } | undefined)?.type

  // Sent by the update bar when the user accepts a new version.
  if (type === 'SKIP_WAITING') {
    self.skipWaiting()
    return
  }

  // Sent on sign-out and sign-in. Saved figures belong to the account that was
  // signed in when they were fetched; without this, the next person to use the
  // device could pull the previous user's balances up offline.
  if (type === 'CLEAR_API_CACHE') {
    event.waitUntil(caches.delete(API_CACHE))
  }
})

// Take over open tabs as soon as this worker activates, so the first visit
// after install is already offline-capable rather than waiting for a reload.
self.addEventListener('activate', (event) => {
  event.waitUntil(self.clients.claim())
})
