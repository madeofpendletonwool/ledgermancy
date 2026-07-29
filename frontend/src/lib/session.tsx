import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ApiError, api, isMFARequired, type LoginResult, type User } from './api'
import { clearApiCache } from './offline'

const SESSION_KEY = ['session'] as const

/**
 * The last user this browser saw signed in.
 *
 * WHY THIS EXISTS, AND WHY IT IS NOT A SECURITY HOLE
 *
 * `GET /api/auth/me` is deliberately never cached by the service worker — a
 * cached 200 would let an expired session keep rendering the app indefinitely.
 * But the app cannot draw anything at all without knowing who is signed in, so
 * with no answer to `me` an offline launch bounces straight to the login
 * screen and the entire offline feature is unreachable.
 *
 * So the *identity* is remembered here, and it is used on exactly one
 * condition: the network is unreachable AND the request failed to transmit. A
 * reachable server that answers 401 always wins and clears this immediately.
 *
 * It grants nothing. Every byte of financial data still comes from a cookie
 * the server validates, and anyone holding this device already has the cached
 * figures themselves. It is cleared on sign-out and on any observed 401.
 */
const LAST_USER_KEY = 'ledgermancy.last-user'

function readLastUser(): User | null {
  try {
    const raw = localStorage.getItem(LAST_USER_KEY)
    return raw ? (JSON.parse(raw) as User) : null
  } catch {
    return null
  }
}

function writeLastUser(user: User | null) {
  try {
    if (user) localStorage.setItem(LAST_USER_KEY, JSON.stringify(user))
    else localStorage.removeItem(LAST_USER_KEY)
  } catch {
    // Private-mode Safari throws on write. Offline launch degrades to a login
    // redirect, which is a worse experience but never a wrong one.
  }
}

/**
 * Forgets everything this browser holds about the last session.
 *
 * Both halves matter and neither is sufficient alone: the identity record
 * here, and the saved API responses in the service worker cache. Leaving
 * either behind means the next person to open the app on a shared device can
 * pull the previous user's balances up by going offline.
 */
function forgetSession() {
  writeLastUser(null)
  void clearApiCache()
}

/**
 * Current signed-in user, or null.
 *
 * A 401 is a normal answer here ("nobody is logged in"), not a failure, so it
 * resolves to null instead of throwing and is never retried.
 *
 * A transport failure is a different thing entirely and must not be read as a
 * sign-out: see `LAST_USER_KEY`. `refetchOnReconnect` (a TanStack default) is
 * what closes the loop — the moment connectivity returns, this re-asks the
 * server, and an expired session collapses to a clean redirect to login rather
 * than a wall of failed requests behind a cached shell.
 */
export function useSession() {
  return useQuery<User | null>({
    queryKey: SESSION_KEY,
    queryFn: async () => {
      try {
        const user = await api.me()
        writeLastUser(user)
        return user
      } catch (err) {
        if (err instanceof ApiError && err.status === 401) {
          forgetSession()
          return null
        }
        // fetch rejects (rather than resolving with a status) only when the
        // request never made it out. If the browser also says it has no route,
        // this is an offline launch, not an expired session.
        if (!navigator.onLine) {
          const remembered = readLastUser()
          if (remembered) return remembered
        }
        throw err
      }
    },
    retry: false,
    staleTime: 30_000,
  })
}

/**
 * Caches a login result as the session, unless it is only an MFA challenge.
 *
 * The distinction is the whole point: a challenge means the server created no
 * session, so writing one into the cache here would let the UI render the app
 * to someone who has not finished signing in. Every request would still 401,
 * but the shell — and whatever it leaks — should never appear at all.
 */
function cacheIfAuthenticated(
  qc: ReturnType<typeof useQueryClient>,
  result: LoginResult,
) {
  if (isMFARequired(result)) return

  // Drop whatever the previous sign-in left cached before recording this one.
  // Two people share a household but not a view of it, and the saved responses
  // on disk were fetched under someone else's scoping.
  void clearApiCache()
  writeLastUser(result)
  qc.setQueryData(SESSION_KEY, result)
}

export function useLogin() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: api.login,
    onSuccess: (result) => cacheIfAuthenticated(qc, result),
  })
}

/** Completes a login that stopped at the second factor. */
export function useVerifyMFA() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: api.verifyMFA,
    onSuccess: (result) => cacheIfAuthenticated(qc, result),
  })
}

export function useRegister() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: api.register,
    onSuccess: (user) => {
      void clearApiCache()
      writeLastUser(user)
      qc.setQueryData(SESSION_KEY, user)
    },
  })
}

/**
 * Changing the password invalidates every session and issues a fresh one for
 * this browser. Clearing the cache afterwards drops anything fetched under the
 * old session rather than trusting it to still be current.
 */
export function useChangePassword() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: api.changePassword,
    onSuccess: () => qc.invalidateQueries({ queryKey: SESSION_KEY }),
  })
}

export function useLogout() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: api.logout,
    // Drop every cached query, not just the session: household data belongs to
    // the user who just signed out and must not survive into the next login.
    // The service worker's on-disk copies outlive this tab entirely, so they
    // have to be told separately — clearing memory alone would leave the
    // balances readable to the next person who opens the app offline.
    onSuccess: () => {
      forgetSession()
      qc.clear()
    },
  })
}
