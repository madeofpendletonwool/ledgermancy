import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import { api, type Insight } from '../lib/api'
import { formatMoney, formatRelative } from '../lib/money'

/**
 * The proactive-insight feed. Renders in two shapes off one data source:
 *  - `variant="card"` — a compact Dashboard section showing the top few unread
 *    insights, or nothing at all when the feed is empty.
 *  - `variant="page"` — the full /insights list, with a "show dismissed" toggle.
 *
 * The feed is deterministic: it populates with or without an AI key (AI only
 * warms the wording), so nothing here is capability-gated.
 */
export function InsightFeed({
  variant,
  state = 'unread',
  limit,
}: {
  variant: 'card' | 'page'
  state?: 'unread' | 'all'
  limit?: number
}) {
  const qc = useQueryClient()
  const feed = useQuery({
    queryKey: ['insights', state],
    queryFn: () => api.insights({ state }),
  })

  const invalidate = () => {
    qc.invalidateQueries({ queryKey: ['insights'] })
  }
  const read = useMutation({ mutationFn: api.markInsightRead, onSuccess: invalidate })
  const dismiss = useMutation({ mutationFn: api.dismissInsight, onSuccess: invalidate })
  const markNormal = useMutation({
    mutationFn: api.markInsightNormal,
    onSuccess: () => {
      invalidate()
      // The restore list in Settings is now one merchant longer.
      qc.invalidateQueries({ queryKey: ['suppressed-anomalies'] })
    },
  })

  const all = feed.data ?? []
  // The card only surfaces things not yet dismissed; the page decides via state.
  const shown = limit ? all.slice(0, limit) : all

  if (variant === 'card') {
    // The card is presence-gated on there being insights — no empty box.
    if (feed.isPending || shown.length === 0) return null
    return (
      <section className="glass p-6">
        <div className="mb-5 flex items-baseline justify-between">
          <h2 className="text-lg font-medium">For you</h2>
          {all.length > shown.length && (
            <Link to="/insights" className="text-sm text-rune-300 hover:underline">
              See all →
            </Link>
          )}
        </div>
        <ul className="space-y-3">
          {shown.map((i) => (
            <InsightRow
              key={i.id}
              insight={i}
              onRead={() => read.mutate(i.id)}
              onDismiss={() => dismiss.mutate(i.id)}
              onMarkNormal={() => markNormal.mutate(i.id)}
              busy={read.isPending || dismiss.isPending || markNormal.isPending}
            />
          ))}
        </ul>
      </section>
    )
  }

  // Page variant.
  if (feed.isPending) {
    return <p className="py-8 text-center text-sm text-mist-500">Loading…</p>
  }
  if (shown.length === 0) {
    return (
      <p className="py-8 text-center text-sm text-mist-500">
        Nothing to flag right now. New observations show up here as they happen.
      </p>
    )
  }
  return (
    <ul className="space-y-3">
      {shown.map((i) => (
        <InsightRow
          key={i.id}
          insight={i}
          onRead={() => read.mutate(i.id)}
          onDismiss={() => dismiss.mutate(i.id)}
          onMarkNormal={() => markNormal.mutate(i.id)}
          busy={read.isPending || dismiss.isPending || markNormal.isPending}
        />
      ))}
    </ul>
  )
}

function InsightRow({
  insight,
  onRead,
  onDismiss,
  onMarkNormal,
  busy,
}: {
  insight: Insight
  onRead: () => void
  onDismiss: () => void
  onMarkNormal: () => void
  busy: boolean
}) {
  const dismissed = insight.dismissed_at != null
  const unread = insight.read_at == null && !dismissed
  const anomaly = ANOMALY_KINDS.has(insight.kind)
  return (
    <li
      className={`rounded-xl border p-4 transition ${
        unread ? 'border-white/10 bg-white/[0.03]' : 'border-white/5 bg-transparent'
      }`}
    >
      <div className="flex items-start gap-3">
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-2">
            <span
              className={`rounded-full border px-2 py-0.5 text-[10px] font-semibold uppercase tracking-wide ${toneClasses(
                insight.priority,
              )}`}
            >
              {kindLabel(insight.kind)}
            </span>
            {unread && (
              <span className="h-1.5 w-1.5 rounded-full bg-arcane-500" aria-label="Unread" />
            )}
          </div>
          <p className="mt-2 font-medium text-mist-100">{insight.title}</p>
          <p className="mt-1 text-sm text-mist-300">{insight.body}</p>
          <AnomalyDetail insight={insight} />
          <p className="mt-1.5 text-xs text-mist-500">{formatRelative(insight.created_at)}</p>
        </div>
        {!dismissed && (
          <div className="flex shrink-0 flex-col items-end gap-1.5">
            {unread && (
              <button
                className="text-xs text-mist-500 transition hover:text-mist-100 disabled:opacity-50"
                onClick={onRead}
                disabled={busy}
              >
                Mark read
              </button>
            )}
            {anomaly && (
              <button
                className="whitespace-nowrap text-xs text-mist-500 transition hover:text-mist-100 disabled:opacity-50"
                onClick={onMarkNormal}
                disabled={busy}
                title="Stop flagging this merchant"
              >
                This is normal
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

const ANOMALY_KINDS = new Set(['merchant_outlier', 'duplicate_charge'])

/**
 * Inline detail for the anomaly kinds — the only insights that render anything
 * out of `insight.data`.
 *
 * "Unusual" with no baseline shown is unactionable: the whole claim is a
 * comparison, so the comparison has to be visible. Every field is guarded and
 * the strip disappears rather than half-renders, so an insight row stored
 * before a payload change degrades to title-and-body instead of breaking the
 * feed.
 *
 * Deliberately kind-switched rather than a generic key/value dump — the other
 * kinds have no designed presentation, and dumping their payloads would leak
 * internal field names into the UI.
 */
function AnomalyDetail({ insight }: { insight: Insight }) {
  const d = insight.data ?? {}
  const str = (k: string): string | null => {
    const v = d[k]
    return typeof v === 'string' && v !== '' ? v : null
  }
  const num = (k: string): number | null => {
    const v = d[k]
    return typeof v === 'number' ? v : null
  }

  let cells: { label: string; value: string }[] = []

  if (insight.kind === 'merchant_outlier') {
    const amount = str('amount')
    const typical = str('typical')
    const samples = num('sample_count')
    if (amount && typical && samples !== null) {
      cells = [
        { label: 'This charge', value: formatMoney(amount) },
        { label: 'Typical here', value: formatMoney(typical) },
        { label: 'Based on', value: `${samples} charge${samples === 1 ? '' : 's'}` },
      ]
    }
  } else if (insight.kind === 'duplicate_charge') {
    const amount = str('amount')
    const total = str('total')
    const count = num('charge_count')
    if (amount && total && count !== null) {
      cells = [
        { label: 'Each charge', value: formatMoney(amount) },
        { label: 'Times charged', value: String(count) },
        { label: 'Total', value: formatMoney(total) },
      ]
    }
  }

  if (cells.length === 0) return null

  return (
    <dl className="mt-3 flex flex-wrap gap-x-6 gap-y-2 rounded-lg border border-white/5 bg-white/[0.02] px-3 py-2">
      {cells.map((c) => (
        <div key={c.label}>
          <dt className="text-[10px] uppercase tracking-wide text-mist-500">{c.label}</dt>
          <dd className="text-sm font-medium tabular-nums text-mist-100">{c.value}</dd>
        </div>
      ))}
    </dl>
  )
}

const KIND_LABELS: Record<string, string> = {
  spending_spike: 'Spending',
  new_recurring: 'Recurring',
  budget_pace: 'Budget',
  low_leftover: 'Cash flow',
  month_end_projection: 'Cash flow',
  large_transaction: 'Large purchase',
  income_change: 'Income',
  savings_milestone: 'Milestone',
  budget_trend: 'Budget',
  upcoming_bill: 'Bill due',
  merchant_outlier: 'Unusual charge',
  duplicate_charge: 'Possible duplicate',
  // These kinds all shipped without a label and fell through to the
  // underscore-stripping fallback, which rendered "alert explanation".
  subscription: 'Subscription',
  forecast: 'Forecast',
  goal: 'Goal',
  alert_explanation: 'Alert',
  document_expiry: 'Document',
  receipt_match: 'Receipt',
}

function kindLabel(kind: string): string {
  return KIND_LABELS[kind] ?? kind.replace(/_/g, ' ')
}

// A priority-scaled chip tone: urgent (5) reads as a warning, mid as accent,
// low as muted.
function toneClasses(priority: number): string {
  if (priority >= 5) return 'border-ember-400/30 bg-ember-400/10 text-ember-400'
  if (priority >= 3) return 'border-rune-400/30 bg-rune-400/10 text-rune-300'
  return 'border-white/10 bg-white/5 text-mist-300'
}
