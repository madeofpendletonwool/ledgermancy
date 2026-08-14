import { useCallback } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from './api'

/**
 * The "Net linked refunds" toggle: the reader-driven lens that lets a refund
 * cancel the charge it refunds.
 *
 * The third of these, after nominal-vs-real and hide-one-time, and it follows
 * the same three rules:
 *
 *  1. Off is the default. Un-netted is what every spending figure in this app
 *     has always meant — the money left the account in June, and the fact that
 *     some of it came back in July is a second fact, not a correction to the
 *     first. Nothing flips this but the user.
 *  2. The toggle is a LENS, not a write. It re-asks the same queries with
 *     `net_refunds=true`; the links themselves, and both transactions, are
 *     untouched either way. Flip it back and the original figures are right
 *     there again.
 *  3. The state is the reader's, not the household's — two people sharing a
 *     ledger can disagree about how to read a trailing chart harmlessly.
 *     Whether two transactions ARE linked is a fact and lives in the database;
 *     whether to net them on a given chart is a question about the reader.
 *
 * What netting does, so the label can be honest about it: a linked refund's
 * amount comes off the ORIGINAL CHARGE, in the charge's own month and category.
 * It is never pushed into the refund's month as a negative — that would move the
 * confusing figure rather than remove it.
 */

/**
 * The user-scoped preference key for the net-refunds toggle.
 *
 * User-scoped for the same reason `views.hide_one_time` is: the link is a fact
 * about the money and is stored on the link; whether a chart honours it is a
 * reading preference.
 */
export const NET_REFUNDS_PREFERENCE_KEY = 'views.net_refunds'

/**
 * The net-refunds choice, persisted per user.
 *
 * `enabled` is false until the preference has loaded, so a chart never renders
 * netted figures for a moment and then snaps back — the same flicker
 * `useHideOneTimePreference` avoids, for the same reason.
 */
export function useNetRefundsPreference() {
  const qc = useQueryClient()
  const prefs = useQuery({ queryKey: ['preferences'], queryFn: api.preferences })

  const save = useMutation({
    mutationFn: (value: boolean) =>
      api.setPreferences([
        { scope: 'user', key: NET_REFUNDS_PREFERENCE_KEY, value },
      ]),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['preferences'] }),
  })

  const stored = prefs.data?.user?.[NET_REFUNDS_PREFERENCE_KEY]
  const enabled = stored === true

  const setEnabled = useCallback(
    (value: boolean) => {
      qc.setQueryData(['preferences'], (old: typeof prefs.data) =>
        old
          ? { ...old, user: { ...old.user, [NET_REFUNDS_PREFERENCE_KEY]: value } }
          : old,
      )
      save.mutate(value)
    },
    [qc, save],
  )

  return { enabled: prefs.isSuccess && enabled, ready: prefs.isSuccess, setEnabled }
}
