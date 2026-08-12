import { useCallback } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from './api'

/**
 * The "Hide one-time charges" toggle: the reader-driven lens over the trailing
 * spending charts.
 *
 * Three rules, and they are the ones the server enforces too:
 *
 *  1. Off is the default. The money did leave, and the trailing views show
 *     every month as it actually happened until the reader says otherwise.
 *     Nothing flips this but the user.
 *  2. The toggle is a LENS, not a correction. It re-asks the same real-period
 *     queries the trailing charts already use, with `exclude_one_time=true`,
 *     so the reader can look at the trailing year without the charges they
 *     have flagged (in Transactions) as not repeating. Flip it back and the
 *     payoff is right there again.
 *  3. The state is the reader's, not the household's. Whether you want to read
 *     your year that way is a reading preference, so it is stored per user —
 *     the same call `useRealPreference` makes for nominal-vs-real.
 */

/**
 * The user-scoped preference key for the hide-one-time toggle.
 *
 * User-scoped rather than household-scoped for the same reason `views.real` is:
 * two people sharing a ledger can disagree about how to read a trailing chart
 * harmlessly. Whether a transaction IS one-time is a fact about the money and
 * lives on the transaction; whether to hide them on a given chart is a question
 * about the reader.
 */
export const ONE_TIME_PREFERENCE_KEY = 'views.hide_one_time'

/**
 * The hide-one-time choice, persisted per user.
 *
 * `enabled` is false until the preference has loaded, so a chart never renders
 * the filtered view for a moment and then snaps back — the same flicker
 * `useRealPreference` avoids, for the same reason.
 */
export function useHideOneTimePreference() {
  const qc = useQueryClient()
  const prefs = useQuery({ queryKey: ['preferences'], queryFn: api.preferences })

  const save = useMutation({
    mutationFn: (value: boolean) =>
      api.setPreferences([
        { scope: 'user', key: ONE_TIME_PREFERENCE_KEY, value },
      ]),
    // Optimistic on the client cache only — the toggle should feel instant,
    // and a failed write leaves the next load showing the stored value, which
    // is the honest outcome rather than a UI that has silently diverged.
    onSuccess: () => qc.invalidateQueries({ queryKey: ['preferences'] }),
  })

  const stored = prefs.data?.user?.[ONE_TIME_PREFERENCE_KEY]
  const enabled = stored === true

  const setEnabled = useCallback(
    (value: boolean) => {
      qc.setQueryData(['preferences'], (old: typeof prefs.data) =>
        old
          ? { ...old, user: { ...old.user, [ONE_TIME_PREFERENCE_KEY]: value } }
          : old,
      )
      save.mutate(value)
    },
    [qc, save],
  )

  return { enabled: prefs.isSuccess && enabled, ready: prefs.isSuccess, setEnabled }
}
