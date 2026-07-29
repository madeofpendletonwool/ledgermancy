import { useOfflineState } from '../lib/offline'

/**
 * States, unmissably, that the figures on screen are saved rather than live.
 *
 * This banner is the price of admission for offline mode. Cached financial
 * data is genuinely useful — and genuinely dangerous, because a balance from
 * this morning is visually identical to a balance from this second. So it is a
 * full-width bar in a warning colour carrying an actual timestamp, not a
 * tasteful cloud icon in a corner. If a user can mistake a saved number for a
 * current one, this feature is a bug.
 *
 * It sits directly under the sticky header and scrolls with the page, which is
 * deliberate: it is a statement about everything below it, not a toast to be
 * dismissed.
 */
export function OfflineBanner() {
  const { online, servingCachedSince } = useOfflineState()

  // Online and everything came off the network: nothing to disclose.
  if (online && !servingCachedSince) return null

  return (
    <div
      role="status"
      aria-live="polite"
      className="border-b border-rune-400/30 bg-rune-400/10 px-4 py-2.5 text-sm text-rune-300 sm:px-6"
    >
      <div className="mx-auto flex max-w-6xl items-start gap-2.5">
        <svg
          className="mt-0.5 h-4 w-4 shrink-0"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          strokeWidth={2}
          strokeLinecap="round"
          strokeLinejoin="round"
          aria-hidden="true"
        >
          <path d="M12 9v4M12 17h.01" />
          <path d="M10.29 3.86 1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0Z" />
        </svg>
        <p>
          <span className="font-medium text-rune-300">{headline(online)}</span>{' '}
          <span className="text-mist-300">{detail(servingCachedSince)}</span>
        </p>
      </div>
    </div>
  )
}

function headline(online: boolean): string {
  return online ? 'Showing saved data.' : 'Offline.'
}

function detail(servingCachedSince: string | null): string {
  if (!servingCachedSince) {
    // Offline before anything was saved — the screens will be empty, and
    // saying so beats letting the user wonder why.
    return 'Figures may be missing or out of date, and changes cannot be saved until you reconnect.'
  }
  return `These figures were saved ${formatSavedAt(servingCachedSince)} and are not live. Changes cannot be saved until you reconnect.`
}

/**
 * Renders the stamp as something a person can judge staleness against.
 *
 * A bare "14:32" is ambiguous the moment the data is a day old — which is
 * exactly when it matters — so anything from a previous day carries its date.
 */
function formatSavedAt(iso: string): string {
  const saved = new Date(iso)
  if (Number.isNaN(saved.getTime())) return 'earlier'

  const now = new Date()
  const sameDay =
    saved.getFullYear() === now.getFullYear() &&
    saved.getMonth() === now.getMonth() &&
    saved.getDate() === now.getDate()

  const time = saved.toLocaleTimeString(undefined, {
    hour: 'numeric',
    minute: '2-digit',
  })
  if (sameDay) return `at ${time}`

  const date = saved.toLocaleDateString(undefined, {
    weekday: 'short',
    day: 'numeric',
    month: 'short',
  })
  return `on ${date} at ${time}`
}
