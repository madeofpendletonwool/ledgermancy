import { useCallback, useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { usePlaidLink } from 'react-plaid-link'
import { api } from '../lib/api'
import { describePlaidExit, logPlaidExit } from '../lib/plaidError'

/**
 * Repairs a broken institution connection through Plaid Link's update mode.
 *
 * This is deliberately not the same flow as connecting a new account. Update
 * mode re-authenticates the *existing* item, so its transactions and its
 * history window survive — unlinking and relinking would discard both, and
 * Plaid fixes an item's history window at link time, so it cannot be recovered.
 *
 * There is no public token to exchange here: Link's onSuccess fires with the
 * item already repaired, and we just tell the API to clear the error state.
 */
export function ReconnectAccount({
  itemId,
  institutionName,
}: {
  itemId: string
  institutionName: string
}) {
  const qc = useQueryClient()
  const [linkToken, setLinkToken] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)

  const finish = useMutation({
    mutationFn: () => api.itemReconnected(itemId),
    onSuccess: () => {
      setLinkToken(null)
      // The status flips to active and a sync is already queued server-side.
      qc.invalidateQueries({ queryKey: ['items'] })
      qc.invalidateQueries({ queryKey: ['accounts'] })
      qc.invalidateQueries({ queryKey: ['transactions'] })
    },
    onError: (err: Error) => setError(err.message),
  })

  const onSuccess = useCallback(() => finish.mutate(), [finish])

  const { open, ready } = usePlaidLink({
    token: linkToken,
    onSuccess,
    // Abandoning the flow leaves the item exactly as it was — still broken,
    // still showing its warning. That is a normal outcome, not an error.
    onExit: (err, metadata) => {
      setLinkToken(null)
      if (err) {
        logPlaidExit(err, metadata)
        setError(describePlaidExit(err, metadata))
      }
    },
  })

  const canOpen = linkToken !== null && ready

  // The token is single-use and short lived, so it is fetched on click rather
  // than on render and never cached beyond the flow it opens.
  const start = useMutation({
    mutationFn: () => api.createLinkToken(itemId),
    onSuccess: (res) => setLinkToken(res.link_token),
    onError: (err: Error) => setError(err.message),
  })

  return (
    <div className="flex flex-col items-start gap-2">
      <button
        className="btn-primary px-3 py-1.5 text-xs"
        disabled={start.isPending || finish.isPending}
        onClick={() => {
          setError(null)
          if (canOpen) open()
          else start.mutate()
        }}
      >
        {finish.isPending
          ? 'Reconnecting…'
          : start.isPending
            ? 'Preparing…'
            : canOpen
              ? 'Open Plaid'
              : 'Reconnect'}
      </button>

      <p className="max-w-sm text-xs text-mist-500">
        {institutionName || 'This institution'} needs you to sign in again.
        Reconnecting keeps your existing transaction history — unlinking and
        relinking would not.
      </p>

      {error && (
        <p
          role="alert"
          className="rounded-xl border border-ember-400/30 bg-ember-400/10 px-3 py-2 text-xs text-ember-400"
        >
          {error}
        </p>
      )}
    </div>
  )
}
