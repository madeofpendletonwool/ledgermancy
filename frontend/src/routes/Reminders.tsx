import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, type Insight } from '../lib/api'
import { formatRelative } from '../lib/money'
import { SkeletonRows } from '../components/Skeleton'

/**
 * Reminders — the action inbox (MAD-85).
 *
 * Where /insights is the whole proactive feed, this view filters it to the
 * time-sensitive, action-shaped kinds: bills that are overdue or due soon,
 * payoff goals that have fallen behind, and document expiries. Overdue bills
 * carry a "mark paid" action so a member who paid outside the matcher's reach
 * (by check, under a different name) can clear the nudge without waiting for the
 * next insight generation pass.
 *
 * Everything here is a deterministic feed row raised server-side; AI only warms
 * the wording, so the page is fully populated with or without an AI key.
 */

// The kinds this view treats as reminders. Drawn from the feed the producers in
// internal/insights raise; kept here rather than server-side so a reminder kind
// can be added without a new endpoint, and so the general /insights feed and
// this view share one data source.
const ACTION_KINDS = new Set(['overdue_bill', 'bill_out_of_range', 'payoff_progress'])
const COMING_UP_KINDS = new Set(['upcoming_bill', 'document_expiry'])
const REMINDER_KINDS = new Set([
  'overdue_bill',
  'bill_out_of_range',
  'payoff_progress',
  'upcoming_bill',
  'document_expiry',
  'goal',
  // The plan's "keep it current" nudge (MAD-258). Not an action with a date,
  // but it IS a decision the household owes itself: re-read the plan and stamp
  // it, or change it.
  'plan_stale',
])

// Kinds whose action is "yes, that was the payment" — both settle one occurrence
// through the same satisfy endpoint. They ask different questions ("did anything
// pay this?" vs "was that the right amount?") but take the same answer.
const SATISFIABLE_KINDS = new Set(['overdue_bill', 'bill_out_of_range'])

const KIND_LABELS: Record<string, string> = {
  overdue_bill: 'Overdue',
  bill_out_of_range: 'Off expected',
  payoff_progress: 'Payoff behind',
  upcoming_bill: 'Due soon',
  document_expiry: 'Expires',
  goal: 'Goal behind',
  plan_stale: 'Plan stale',
}

export function Reminders() {
  const qc = useQueryClient()
  const [showDismissed, setShowDismissed] = useState(false)

  const feed = useQuery({
    queryKey: ['insights', showDismissed ? 'all' : 'unread'],
    queryFn: () => api.insights({ state: showDismissed ? 'all' : 'unread' }),
  })

  const invalidate = () => qc.invalidateQueries({ queryKey: ['insights'] })
  const dismiss = useMutation({ mutationFn: api.dismissInsight, onSuccess: invalidate })
  const satisfy = useMutation({
    mutationFn: (i: Insight) =>
      api.satisfyObligation(String(i.data.obligation_id), String(i.data.due_date)),
    onSuccess: invalidate,
  })

  const all = (feed.data ?? []).filter((i) => REMINDER_KINDS.has(i.kind))
  const action = all.filter((i) => ACTION_KINDS.has(i.kind))
  const comingUp = all.filter((i) => COMING_UP_KINDS.has(i.kind))
  const goals = all.filter((i) => i.kind === 'goal')

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-baseline justify-between gap-3">
        <div>
          <h1 className="text-2xl font-semibold">Reminders</h1>
          <p className="mt-1 text-mist-300">
            Bills to pay, goals to fund, paperwork about to lapse — the things with a date on them.
          </p>
        </div>
        <label className="flex items-center gap-2 text-sm text-mist-300">
          <input
            type="checkbox"
            className="accent-arcane-500"
            checked={showDismissed}
            onChange={(e) => setShowDismissed(e.target.checked)}
          />
          Show dismissed
        </label>
      </div>

      {feed.isPending ? (
        <SkeletonRows count={3} />
      ) : all.length === 0 ? (
        <section className="glass p-8 text-center">
          <p className="text-mist-300">You're caught up — nothing due or overdue.</p>
          <p className="mt-1 text-sm text-mist-500">
            Bills, payoff goals and document expiries will land here as they come up.
          </p>
        </section>
      ) : (
        <div className="space-y-8">
          {action.length > 0 && (
            <Section title="Needs action" rows={action}>
              {(i) => (
                <ReminderRow
                  key={i.id}
                  insight={i}
                  onDismiss={() => dismiss.mutate(i.id)}
                  onSatisfy={
                    SATISFIABLE_KINDS.has(i.kind)
                      ? () => satisfy.mutate(i)
                      : undefined
                  }
                  busy={dismiss.isPending || satisfy.isPending}
                />
              )}
            </Section>
          )}
          {comingUp.length > 0 && (
            <Section title="Coming up" rows={comingUp}>
              {(i) => (
                <ReminderRow
                  key={i.id}
                  insight={i}
                  onDismiss={() => dismiss.mutate(i.id)}
                  busy={dismiss.isPending}
                />
              )}
            </Section>
          )}
          {goals.length > 0 && (
            <Section title="Goals behind" rows={goals}>
              {(i) => (
                <ReminderRow
                  key={i.id}
                  insight={i}
                  onDismiss={() => dismiss.mutate(i.id)}
                  busy={dismiss.isPending}
                />
              )}
            </Section>
          )}
        </div>
      )}
    </div>
  )
}

function Section({
  title,
  rows,
  children,
}: {
  title: string
  rows: Insight[]
  children: (i: Insight) => React.ReactNode
}) {
  return (
    <section>
      <h2 className="mb-3 text-sm font-semibold uppercase tracking-wide text-mist-400">
        {title}
      </h2>
      <ul className="space-y-3">{rows.map((i) => children(i))}</ul>
    </section>
  )
}

function ReminderRow({
  insight,
  onDismiss,
  onSatisfy,
  busy,
}: {
  insight: Insight
  onDismiss: () => void
  onSatisfy?: () => void
  busy: boolean
}) {
  // Closed either way — a member dismissed it, or the app retracted it once the
  // payment landed. Both drop the action buttons; only the second earns the
  // "Resolved" tag, since nothing was asked of the member.
  const retracted = insight.retracted_at != null
  const closed = insight.dismissed_at != null || retracted
  return (
    <li
      className={`rounded-xl border p-4 transition ${
        closed ? 'border-white/5 bg-transparent opacity-60' : 'border-white/10 bg-white/[0.03]'
      }`}
    >
      <div className="flex items-start gap-3">
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-2">
            <span
              className={`rounded-full border px-2 py-0.5 text-[10px] font-semibold uppercase tracking-wide ${toneClasses(
                insight.priority,
                insight.kind,
              )}`}
            >
              {KIND_LABELS[insight.kind] ?? insight.kind}
            </span>
            {retracted && (
              <span
                className="rounded-full border border-white/10 px-2 py-0.5 text-[10px] font-semibold uppercase tracking-wide text-mist-500"
                title="This stopped being true — the bill was paid, or the amount came back into range"
              >
                Resolved
              </span>
            )}
          </div>
          <p className="mt-2 font-medium text-mist-100">{insight.title}</p>
          <p className="mt-1 text-sm text-mist-300">{insight.body}</p>
          <p className="mt-1.5 text-xs text-mist-500">{formatRelative(insight.created_at)}</p>
        </div>
        {!closed && (
          <div className="flex shrink-0 flex-col items-end gap-1.5">
            {onSatisfy && (
              <button
                className="whitespace-nowrap rounded-md border border-verdant-400/30 bg-verdant-400/10 px-2.5 py-1 text-xs font-medium text-verdant-400 transition hover:bg-verdant-400/20 disabled:opacity-50"
                onClick={onSatisfy}
                disabled={busy}
                title={
                  insight.kind === 'bill_out_of_range'
                    ? 'Accept this charge as the payment for this cycle'
                    : 'Mark this occurrence paid'
                }
              >
                {insight.kind === 'bill_out_of_range' ? 'Accept charge' : 'Mark paid'}
              </button>
            )}
            <button
              className="text-xs text-mist-500 transition hover:text-ember-400 disabled:opacity-50"
              onClick={onDismiss}
              disabled={busy}
            >
              Dismiss
            </button>
          </div>
        )}
      </div>
    </li>
  )
}

// A priority- and kind-scaled chip tone: an overdue bill reads as a warning, a
// payoff gap as accent, everything else as muted.
function toneClasses(priority: number, kind: string): string {
  if (kind === 'overdue_bill' || kind === 'bill_out_of_range' || priority >= 5)
    return 'border-ember-400/30 bg-ember-400/10 text-ember-400'
  if (kind === 'payoff_progress' || priority >= 3)
    return 'border-rune-400/30 bg-rune-400/10 text-rune-300'
  return 'border-white/10 bg-white/5 text-mist-300'
}
