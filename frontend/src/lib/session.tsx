import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ApiError, api, type User } from './api'

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

export function useLogin() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: api.login,
    onSuccess: (user) => qc.setQueryData(SESSION_KEY, user),
  })
}

export function useRegister() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: api.register,
    onSuccess: (user) => qc.setQueryData(SESSION_KEY, user),
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
