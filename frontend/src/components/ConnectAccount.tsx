import { useCallback, useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { usePlaidLink } from 'react-plaid-link'
import { api } from '../lib/api'

/**
 * Opens Plaid Link and completes the exchange.
 *
 * The flow is: ask our API for a link token, hand it to Plaid's widget, and
 * post the resulting public token back. The bank credentials the user types
 * go straight to Plaid and never touch this app or its server.
 */
export function ConnectAccount() {
  const qc = useQueryClient()
  const [linkToken, setLinkToken] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)

  const exchange = useMutation({
    mutationFn: api.exchangePublicToken,
    onSuccess: () => {
      setLinkToken(null)
      // The backfill runs in the background, so refresh these as it lands.
      qc.invalidateQueries({ queryKey: ['items'] })
      qc.invalidateQueries({ queryKey: ['accounts'] })
      qc.invalidateQueries({ queryKey: ['transactions'] })
    },
    onError: (err: Error) => setError(err.message),
  })

  const onSuccess = useCallback(
    (publicToken: string) => exchange.mutate(publicToken),
    [exchange],
  )

  const { open, ready } = usePlaidLink({
    token: linkToken,
    onSuccess,
    // The user closing Link is a normal outcome, not an error; only surface a
    // genuine failure.
    onExit: (err) => {
      setLinkToken(null)
      if (err) setError(err.display_message ?? err.error_message ?? null)
    },
  })

  const startLink = useMutation({
    mutationFn: api.createLinkToken,
    onSuccess: (res) => setLinkToken(res.link_token),
    onError: (err: Error) => setError(err.message),
  })

  // Plaid's widget can only be opened once its token is loaded and ready.
  const canOpen = linkToken !== null && ready

  return (
    <div className="flex flex-col items-start gap-3">
      <button
        className="btn-primary"
        disabled={startLink.isPending || exchange.isPending}
        onClick={() => {
          setError(null)
          if (canOpen) open()
          else startLink.mutate()
        }}
      >
        {exchange.isPending
          ? 'Linking…'
          : startLink.isPending
            ? 'Preparing…'
            : canOpen
              ? 'Open Plaid'
              : '+ Connect an account'}
      </button>

      {canOpen && (
        <p className="text-xs text-mist-500">
          Ready — press “Open Plaid” to choose your institution.
        </p>
      )}

      {error && (
        <p
          role="alert"
          className="rounded-xl border border-ember-400/30 bg-ember-400/10 px-4 py-2.5 text-sm text-ember-400"
        >
          {error}
        </p>
      )}
    </div>
  )
}
