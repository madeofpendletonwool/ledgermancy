import { useCallback } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, type Inflation } from './api'

/**
 * Inflation-adjusted ("real") views: the shared state behind every real toggle
 * in the app (doc 27).
 *
 * Three rules, and they are the same three the server enforces:
 *
 *  1. Nominal is the default. The preference starts false and nothing flips it
 *     but the user.
 *  2. A real figure is never rendered without its base period. `realLabel()`
 *     exists so no page has to remember to say "in June 2026 dollars", and so
 *     none of them say it differently.
 *  3. An undeflatable point is a GAP, not a nominal value in disguise. The API
 *     types make the real fields optional for exactly this reason; treat
 *     `undefined` as "cannot say", never as "same as nominal".
 */

/**
 * The user-scoped preference key for the real/nominal toggle.
 *
 * User-scoped rather than household-scoped: whether you want to read a chart in
 * today's dollars is a reading preference, not a fact about the household's
 * money. Two people sharing a ledger can disagree about it harmlessly, which is
 * not true of, say, anomaly sensitivity.
 */
export const REAL_PREFERENCE_KEY = 'views.real'

/** The deflator's own description. Cached — it changes at most once a month. */
export function useInflation() {
  return useQuery({
    queryKey: ['inflation'],
    queryFn: api.inflation,
    staleTime: 60 * 60 * 1000,
  })
}

/**
 * The real/nominal choice, persisted per user.
 *
 * `enabled` is false until the preference has loaded, so a page never renders a
 * real chart for a moment and then snaps back to nominal — the flicker would be
 * a chart silently changing what its numbers mean.
 */
export function useRealPreference() {
  const qc = useQueryClient()
  const prefs = useQuery({ queryKey: ['preferences'], queryFn: api.preferences })

  const save = useMutation({
    mutationFn: (value: boolean) =>
      api.setPreferences([
        { scope: 'user', key: REAL_PREFERENCE_KEY, value },
      ]),
    // Optimistic on the client cache only: the toggle should feel instant, and
    // a failed write leaves the next load showing the stored value, which is
    // the honest outcome rather than a silently diverging UI.
    onSuccess: () => qc.invalidateQueries({ queryKey: ['preferences'] }),
  })

  const stored = prefs.data?.user?.[REAL_PREFERENCE_KEY]
  const enabled = stored === true

  const setEnabled = useCallback(
    (value: boolean) => {
      qc.setQueryData(['preferences'], (old: typeof prefs.data) =>
        old
          ? { ...old, user: { ...old.user, [REAL_PREFERENCE_KEY]: value } }
          : old,
      )
      save.mutate(value)
    },
    [qc, save],
  )

  return { enabled: prefs.isSuccess && enabled, ready: prefs.isSuccess, setEnabled }
}

/**
 * How to say what a real figure is denominated in: "in June 2026 dollars".
 *
 * Returns null when there is no base period, which is the signal to render the
 * nominal figure instead — a real number with nowhere to anchor it is not a
 * number to show.
 */
export function realLabel(inflation: Inflation | undefined): string | null {
  if (!inflation?.available || !inflation.base_label) return null
  return `in ${inflation.base_label} dollars`
}

/**
 * Whether a real toggle should be offered for a window of `months`.
 *
 * Short windows are excluded on purpose: deflating one month by one month's
 * price change moves the figure by a couple of tenths of a percent, which is
 * inside the noise of the thing being deflated and invites a conclusion the
 * data cannot support. The server publishes the threshold so both sides agree.
 */
export function realWorthOffering(
  inflation: Inflation | undefined,
  months: number,
): boolean {
  if (!inflation?.available) return false
  return months >= inflation.min_span_months
}

/** Formats a decimal-string fraction as a signed percentage: "0.062" → "+6.2%". */
export function formatRate(value: string | null | undefined, digits = 1): string {
  if (value === null || value === undefined) return '—'
  const n = Number(value)
  if (!Number.isFinite(n)) return '—'
  const pct = n * 100
  return `${pct >= 0 ? '+' : ''}${pct.toFixed(digits)}%`
}
