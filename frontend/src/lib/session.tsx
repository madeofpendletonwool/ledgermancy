import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ApiError, api, isMFARequired, type LoginResult, type User } from './api'

const SESSION_KEY = ['session'] as const

/**
 * Current signed-in user, or null.
 *
 * A 401 is a normal answer here ("nobody is logged in"), not a failure, so it
 * resolves to null instead of throwing and is never retried.
 */
export function useSession() {
  return useQuery<User | null>({
    queryKey: SESSION_KEY,
    queryFn: async () => {
      try {
        return await api.me()
      } catch (err) {
        if (err instanceof ApiError && err.status === 401) return null
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
  if (!isMFARequired(result)) qc.setQueryData(SESSION_KEY, result)
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
    onSuccess: (user) => qc.setQueryData(SESSION_KEY, user),
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
    onSuccess: () => qc.clear(),
  })
}
