import { useEffect, useState } from 'react'
import { useRegisterSW } from 'virtual:pwa-register/react'
import { isStandalone } from '../lib/offline'

/**
 * Registers the service worker and owns the update bar.
 *
 * Mounted once at the root of the app rather than inside the signed-in layout,
 * so the worker registers on every route including login — an app that only
 * becomes installable after sign-in is one most people never discover.
 */
export function ServiceWorkerHost() {
  return <UpdateBar />
}

/**
 * Offers a reload when a new build is waiting.
 *
 * Registration is `prompt`, not `autoUpdate`, and this bar is the reason.
 * Auto-updating swaps the shell and reloads whenever a deploy lands, which on
 * a finance app means a half-typed budget or a part-filled manual transaction
 * disappearing without explanation. The user picks the moment.
 *
 * Without something like this an installed client can sit on a precached shell
 * indefinitely, which is the classic PWA failure: shipping a fix that nobody
 * ever receives.
 */
function UpdateBar() {
  const {
    needRefresh: [needRefresh, setNeedRefresh],
    updateServiceWorker,
  } = useRegisterSW({
    onRegisterError(error) {
      // Registration fails on insecure origins and in some private modes. The
      // app works fine without a worker; it just is not installable or
      // offline-capable, so this is a log line and not a user-facing error.
      console.warn('service worker registration failed', error)
    },
  })

  if (!needRefresh) return null

  return (
    <div className="fixed inset-x-0 bottom-0 z-50 px-4 pb-4 sm:px-6">
      <div className="mx-auto flex max-w-2xl flex-wrap items-center gap-3 rounded-xl border border-white/10 bg-ink-850/90 px-4 py-3 shadow-lg backdrop-blur-xl">
        <p className="flex-1 text-sm text-mist-100">
          A new version of Ledgermancy is ready.
        </p>
        <button
          type="button"
          className="btn-ghost px-3 py-1.5 text-sm"
          onClick={() => setNeedRefresh(false)}
        >
          Later
        </button>
        <button
          type="button"
          className="btn-primary px-3 py-1.5 text-sm"
          // Reloads once the waiting worker takes over, so the tab comes back
          // on the new shell rather than a mix of both.
          onClick={() => void updateServiceWorker(true)}
        >
          Reload
        </button>
      </div>
    </div>
  )
}

/**
 * The install event, captured before React is mounted.
 *
 * Chrome fires `beforeinstallprompt` early — frequently before the first
 * render — and the event is only usable if its default was prevented at the
 * moment it fired. Waiting for a component to mount loses it, so the listener
 * is installed at module scope and the event is parked here for whoever asks.
 */
interface BeforeInstallPromptEvent extends Event {
  prompt: () => Promise<void>
  userChoice: Promise<{ outcome: 'accepted' | 'dismissed' }>
}

let deferredPrompt: BeforeInstallPromptEvent | null = null
const promptListeners = new Set<() => void>()

if (typeof window !== 'undefined') {
  window.addEventListener('beforeinstallprompt', (event) => {
    event.preventDefault()
    deferredPrompt = event as BeforeInstallPromptEvent
    for (const listener of promptListeners) listener()
  })

  window.addEventListener('appinstalled', () => {
    deferredPrompt = null
    for (const listener of promptListeners) listener()
  })
}

const DISMISSED_KEY = 'ledgermancy.install-dismissed'

function wasDismissed(): boolean {
  try {
    return localStorage.getItem(DISMISSED_KEY) === '1'
  } catch {
    return false
  }
}

function rememberDismissal() {
  try {
    localStorage.setItem(DISMISSED_KEY, '1')
  } catch {
    // Nothing to do; the worst case is being asked once more.
  }
}

/** iPhone and iPad, including iPadOS reporting itself as a Mac. */
function isIOS(): boolean {
  if (typeof navigator === 'undefined') return false
  return (
    /iPhone|iPad|iPod/.test(navigator.userAgent) ||
    (navigator.platform === 'MacIntel' && navigator.maxTouchPoints > 1)
  )
}

/**
 * A single, dismissible offer to install — and then never again.
 *
 * Dismissal is persistent by design. An install nag that returns every session
 * is the most reliable way to make people stop reading the app's banners
 * altogether, including the offline one, which they cannot afford to ignore.
 */
export function InstallPrompt() {
  const [canPrompt, setCanPrompt] = useState(deferredPrompt !== null)
  const [dismissed, setDismissed] = useState(wasDismissed)

  useEffect(() => {
    const listener = () => setCanPrompt(deferredPrompt !== null)
    promptListeners.add(listener)
    return () => {
      promptListeners.delete(listener)
    }
  }, [])

  // Already installed: there is nothing to offer.
  if (isStandalone() || dismissed) return null

  // iOS Safari never fires beforeinstallprompt and has no programmatic install,
  // so the only honest thing to offer is the manual route.
  const iosHint = !canPrompt && isIOS()
  if (!canPrompt && !iosHint) return null

  const dismiss = () => {
    rememberDismissal()
    setDismissed(true)
  }

  const install = async () => {
    const prompt = deferredPrompt
    if (!prompt) return
    // A given event can only be shown once, so it is released either way.
    deferredPrompt = null
    setCanPrompt(false)
    await prompt.prompt()
    const { outcome } = await prompt.userChoice
    if (outcome === 'dismissed') dismiss()
  }

  return (
    <div className="mx-auto mb-4 flex max-w-6xl flex-wrap items-center gap-3 rounded-xl border border-white/10 bg-ink-850/60 px-4 py-3 backdrop-blur-xl">
      <p className="flex-1 text-sm text-mist-300">
        {iosHint ? (
          <>
            Add Ledgermancy to your home screen for a full-screen app that opens
            offline — tap <span className="text-mist-100">Share</span>, then{' '}
            <span className="text-mist-100">Add to Home Screen</span>.
          </>
        ) : (
          <>
            Install Ledgermancy for a full-screen app that still opens when
            you're offline.
          </>
        )}
      </p>
      {!iosHint && (
        <button
          type="button"
          className="btn-primary px-3 py-1.5 text-sm"
          onClick={() => void install()}
        >
          Install
        </button>
      )}
      <button
        type="button"
        className="btn-ghost px-3 py-1.5 text-sm"
        onClick={dismiss}
      >
        {iosHint ? 'Got it' : 'Not now'}
      </button>
    </div>
  )
}
