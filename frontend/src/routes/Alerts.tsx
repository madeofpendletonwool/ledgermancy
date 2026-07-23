import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  api,
  type Alert,
  type AlertEvent,
  type AlertType,
  type ParseRuleResult,
} from '../lib/api'
import { formatMoney, formatRelative } from '../lib/money'

// A seed pushed into a rule form when the user chooses "Edit" on a parsed
// proposal: the config to prefill, plus a nonce so re-picking the same values
// still re-applies.
type RuleSeed = { config: Record<string, string | number>; nonce: number }

// Field describes one editable value in an alert's config, so each type's form
// is rendered from data rather than four near-identical blocks of JSX.
type Field = {
  key: string
  label: string
  kind: 'money' | 'percent' | 'int'
}

type TypeMeta = {
  label: string
  description: string
  defaults: Record<string, string | number>
  fields: Field[]
}

const TYPE_META: Record<AlertType, TypeMeta> = {
  big_spend: {
    label: 'Big purchase',
    description: 'Flag any single purchase over an amount you set.',
    defaults: { threshold: '200.00' },
    fields: [{ key: 'threshold', label: 'When a purchase exceeds', kind: 'money' }],
  },
  budget_threshold: {
    label: 'Budget threshold',
    description: 'Warn when spending nears a category budget for the month.',
    defaults: { percent: 90 },
    fields: [{ key: 'percent', label: 'When spending reaches (% of budget)', kind: 'percent' }],
  },
  unusual_merchant: {
    label: 'New merchant',
    description: 'Notice a merchant that has only just started appearing.',
    defaults: { recent_days: 7, min_amount: '25.00' },
    fields: [
      { key: 'recent_days', label: 'First seen within (days)', kind: 'int' },
      { key: 'min_amount', label: 'Minimum amount to flag', kind: 'money' },
    ],
  },
  low_leftover: {
    label: 'Low leftover',
    description: 'Warn when money left this month (income minus spending) runs low.',
    defaults: { floor: '500.00' },
    fields: [{ key: 'floor', label: 'When money left drops below', kind: 'money' }],
  },
}

const ORDER: AlertType[] = [
  'big_spend',
  'budget_threshold',
  'unusual_merchant',
  'low_leftover',
]

export function Alerts() {
  const alerts = useQuery({ queryKey: ['alerts'], queryFn: api.alerts })
  const events = useQuery({ queryKey: ['alert-events'], queryFn: api.alertEvents })
  const capabilities = useQuery({
    queryKey: ['capabilities'],
    queryFn: api.capabilities,
    staleTime: Infinity,
  })

  const byType = new Map<AlertType, Alert>()
  for (const a of alerts.data ?? []) byType.set(a.type, a)

  // When "Edit" is chosen on a parsed alert, seed the matching rule's form.
  const [seeds, setSeeds] = useState<Partial<Record<AlertType, RuleSeed>>>({})
  const seedRule = (type: AlertType, config: Record<string, string | number>) =>
    setSeeds((prev) => ({
      ...prev,
      [type]: { config, nonce: (prev[type]?.nonce ?? 0) + 1 },
    }))

  return (
    <div className="space-y-8">
      <div>
        <h1 className="text-2xl font-semibold">Alerts</h1>
        <p className="mt-1 text-mist-300">
          Rules that watch your spending and surface anything worth a glance.
        </p>
      </div>

      {capabilities.data?.ai_enabled && (
        <NLAlertParser existingByType={byType} onEdit={seedRule} />
      )}

      <RecentEvents events={events.data} isPending={events.isPending} />

      <section className="space-y-4">
        <h2 className="text-lg font-medium">Rules</h2>
        {ORDER.map((type) => (
          <AlertRule
            key={type}
            type={type}
            existing={byType.get(type)}
            seed={seeds[type]}
          />
        ))}
      </section>
    </div>
  )
}

// NLAlertParser lets a user describe an alert or budget in their own words. The
// parse is a proposal only — it renders a confirmation card showing exactly what
// the engine will enforce (plus any caveats), and only on Confirm does it write
// through the existing mutations. Unsupported requests show a reason and no save.
function NLAlertParser({
  existingByType,
  onEdit,
}: {
  existingByType: Map<AlertType, Alert>
  onEdit: (type: AlertType, config: Record<string, string | number>) => void
}) {
  const qc = useQueryClient()
  const [text, setText] = useState('')
  const [result, setResult] = useState<ParseRuleResult | null>(null)

  const parse = useMutation({
    mutationFn: (t: string) => api.parseAlert(t),
    onSuccess: setResult,
  })

  const invalidate = () => {
    qc.invalidateQueries({ queryKey: ['alerts'] })
    qc.invalidateQueries({ queryKey: ['alert-events'] })
    qc.invalidateQueries({ queryKey: ['alerts', 'unread'] })
    qc.invalidateQueries({ queryKey: ['budgets'] })
  }

  const confirm = useMutation({
    mutationFn: async (r: ParseRuleResult) => {
      if (r.kind === 'alert' && r.alert) {
        const existing = existingByType.get(r.alert.type)
        // Preserve an existing rule's push choice; a brand-new rule starts
        // in-app only (push off), matching the schema default — the user opts
        // into push explicitly on the rule below.
        return existing
          ? api.updateAlert(existing.id, r.alert.config, true, existing.push)
          : api.createAlert(r.alert.type, r.alert.config, true, false)
      }
      if (r.kind === 'budget' && r.budget) {
        return api.setBudget(r.budget.category_id, r.budget.amount)
      }
    },
    onSuccess: () => {
      invalidate()
      setResult(null)
      setText('')
    },
  })

  const clear = () => {
    setResult(null)
    parse.reset()
    confirm.reset()
  }

  return (
    <section className="glass p-6">
      <h2 className="text-lg font-medium">Describe an alert</h2>
      <p className="mt-1 mb-4 text-sm text-mist-300">
        Say what you want in plain English — “tell me about any purchase over
        $300”, or “budget $600 a month for groceries”.
      </p>

      <div className="flex flex-wrap items-end gap-3">
        <input
          className="field min-w-0 flex-1"
          placeholder="Describe an alert in your own words…"
          value={text}
          onChange={(e) => setText(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter' && text.trim()) parse.mutate(text)
          }}
        />
        <button
          className="btn-primary px-4 py-2 text-sm"
          disabled={parse.isPending || text.trim() === ''}
          onClick={() => parse.mutate(text)}
        >
          {parse.isPending ? 'Reading…' : 'Parse'}
        </button>
      </div>

      {parse.isError && (
        <p role="alert" className="mt-3 text-sm text-ember-400">
          {parse.error.message}
        </p>
      )}

      {result && (
        <div className="mt-5 rounded-xl border border-white/10 bg-white/5 p-4">
          {result.kind === 'unsupported' ? (
            <>
              <p className="text-sm text-mist-200">
                That isn’t something the app can set up yet.
              </p>
              {result.reason && (
                <p className="mt-1 text-sm text-mist-400">{result.reason}</p>
              )}
              <div className="mt-4">
                <button
                  className="btn-ghost px-3 py-1.5 text-sm text-mist-300"
                  onClick={clear}
                >
                  Dismiss
                </button>
              </div>
            </>
          ) : (
            <>
              <p className="text-sm font-medium text-mist-100">Will enforce</p>
              <p className="mt-1 text-sm text-mist-300">{result.summary}</p>
              {result.caveats && result.caveats.length > 0 && (
                <ul className="mt-2 space-y-1">
                  {result.caveats.map((c, i) => (
                    <li key={i} className="text-xs text-amber-300/90">
                      Note: {c}
                    </li>
                  ))}
                </ul>
              )}
              <div className="mt-4 flex flex-wrap items-center gap-2">
                <button
                  className="btn-primary px-4 py-1.5 text-sm"
                  disabled={confirm.isPending}
                  onClick={() => confirm.mutate(result)}
                >
                  {confirm.isPending ? 'Saving…' : 'Confirm'}
                </button>
                {result.kind === 'alert' && result.alert && (
                  <button
                    className="btn-ghost px-3 py-1.5 text-sm"
                    onClick={() => {
                      onEdit(result.alert!.type, result.alert!.config)
                      clear()
                    }}
                  >
                    Edit in form
                  </button>
                )}
                <button
                  className="btn-ghost px-3 py-1.5 text-sm text-mist-300"
                  onClick={clear}
                >
                  Cancel
                </button>
                {confirm.isError && (
                  <span role="alert" className="text-sm text-ember-400">
                    {confirm.error.message}
                  </span>
                )}
              </div>
            </>
          )}
        </div>
      )}
    </section>
  )
}

function RecentEvents({
  events,
  isPending,
}: {
  events: AlertEvent[] | undefined
  isPending: boolean
}) {
  const qc = useQueryClient()

  // AI explanations live as alert_explanation insights linked back by event id.
  // Match against the feed the app already fetches — no per-event endpoint — and
  // only when AI is on; the deterministic title/detail always render.
  const capabilities = useQuery({
    queryKey: ['capabilities'],
    queryFn: api.capabilities,
    staleTime: Infinity,
  })
  const insights = useQuery({
    queryKey: ['insights', 'all'],
    queryFn: () => api.insights({ state: 'all' }),
    enabled: capabilities.data?.ai_enabled ?? false,
  })
  const explByEvent = new Map<string, string>()
  for (const i of insights.data ?? []) {
    if (i.kind === 'alert_explanation') {
      explByEvent.set(String(i.data.alert_event_id), i.body)
    }
  }

  const markAll = useMutation({
    mutationFn: api.markAllAlertsRead,
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['alert-events'] })
      qc.invalidateQueries({ queryKey: ['alerts', 'unread'] })
    },
  })
  const markOne = useMutation({
    mutationFn: api.markAlertRead,
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['alert-events'] })
      qc.invalidateQueries({ queryKey: ['alerts', 'unread'] })
    },
  })

  const hasUnread = (events ?? []).some((e) => !e.read)

  return (
    <section className="glass p-6">
      <div className="flex items-center justify-between">
        <h2 className="text-lg font-medium">Recent</h2>
        {hasUnread && (
          <button
            className="btn-ghost px-3 py-1.5 text-sm"
            disabled={markAll.isPending}
            onClick={() => markAll.mutate()}
          >
            Mark all read
          </button>
        )}
      </div>

      <ul className="mt-4 divide-y divide-white/5">
        {isPending && <li className="py-3 text-sm text-mist-500">Loading…</li>}
        {!isPending && (events?.length ?? 0) === 0 && (
          <li className="py-3 text-sm text-mist-500">
            Nothing yet. When a rule triggers, it shows up here.
          </li>
        )}
        {events?.map((e) => {
          const { title, detail } = describeEvent(e)
          const explanation = explByEvent.get(e.id)
          return (
            <li key={e.id} className="flex items-start gap-3 py-3">
              {!e.read && (
                <span
                  className="mt-1.5 h-2 w-2 shrink-0 rounded-full bg-arcane-400"
                  aria-label="unread"
                />
              )}
              <div className={`min-w-0 ${e.read ? 'opacity-60' : ''}`}>
                <p className="font-medium">{title}</p>
                <p className="truncate text-sm text-mist-400">{detail}</p>
                {explanation && (
                  <p className="mt-1 text-sm italic text-mist-500">{explanation}</p>
                )}
                <p className="mt-0.5 text-xs text-mist-500">
                  {formatRelative(e.triggered_at)}
                </p>
              </div>
              {!e.read && (
                <button
                  className="btn-ghost ml-auto shrink-0 px-2.5 py-1 text-xs"
                  disabled={markOne.isPending}
                  onClick={() => markOne.mutate(e.id)}
                >
                  Mark read
                </button>
              )}
            </li>
          )
        })}
      </ul>
    </section>
  )
}

function AlertRule({
  type,
  existing,
  seed,
}: {
  type: AlertType
  existing: Alert | undefined
  seed?: RuleSeed
}) {
  const qc = useQueryClient()
  const meta = TYPE_META[type]

  const [enabled, setEnabled] = useState(existing?.enabled ?? false)
  const [push, setPush] = useState(existing?.push ?? false)
  const [config, setConfig] = useState<Record<string, string>>(() =>
    initialConfig(meta, existing),
  )

  // Re-seed local state whenever the server row changes (e.g. after a save from
  // another tab), so the form reflects the source of truth.
  useEffect(() => {
    setEnabled(existing?.enabled ?? false)
    setPush(existing?.push ?? false)
    setConfig(initialConfig(meta, existing))
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [existing?.id, existing?.enabled, existing?.push, JSON.stringify(existing?.config)])

  // Apply a parsed proposal's config when the user chose "Edit in form". Keyed on
  // the seed nonce so re-picking the same values still prefills.
  useEffect(() => {
    if (!seed) return
    setConfig((prev) => {
      const next = { ...prev }
      for (const field of meta.fields) {
        const v = seed.config[field.key]
        if (v !== undefined) next[field.key] = String(v)
      }
      return next
    })
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [seed?.nonce])

  const invalidate = () => {
    qc.invalidateQueries({ queryKey: ['alerts'] })
    qc.invalidateQueries({ queryKey: ['alert-events'] })
    qc.invalidateQueries({ queryKey: ['alerts', 'unread'] })
  }

  const save = useMutation({
    mutationFn: (next: { enabled: boolean; push: boolean }) => {
      const payload = coerceConfig(meta, config)
      return existing
        ? api.updateAlert(existing.id, payload, next.enabled, next.push)
        : api.createAlert(type, payload, next.enabled, next.push)
    },
    onSuccess: invalidate,
  })

  const toggleEnabled = (value: boolean) => {
    setEnabled(value)
    // A rule that's off can't push; clear push alongside so the two never
    // disagree in the stored row.
    const nextPush = value ? push : false
    setPush(nextPush)
    save.mutate({ enabled: value, push: nextPush })
  }

  const togglePush = (value: boolean) => {
    setPush(value)
    save.mutate({ enabled, push: value })
  }

  return (
    <div className="glass p-5">
      <div className="flex items-start justify-between gap-4">
        <div>
          <h3 className="font-medium">{meta.label}</h3>
          <p className="mt-0.5 text-sm text-mist-400">{meta.description}</p>
        </div>
        <label className="flex shrink-0 items-center gap-2 text-sm text-mist-300">
          <input
            type="checkbox"
            className="h-4 w-4 accent-arcane-500"
            checked={enabled}
            onChange={(e) => toggleEnabled(e.target.checked)}
          />
          {enabled ? 'On' : 'Off'}
        </label>
      </div>

      <label
        className={`mt-3 flex items-center gap-2 text-sm ${
          enabled ? 'text-mist-300' : 'text-mist-500'
        }`}
      >
        <input
          type="checkbox"
          className="h-4 w-4 accent-arcane-500"
          checked={push}
          disabled={!enabled || save.isPending}
          onChange={(e) => togglePush(e.target.checked)}
        />
        Also push this to my notification channel
        {!enabled && (
          <span className="text-xs text-mist-500">— turn the rule on first</span>
        )}
      </label>
      {push && enabled && (
        <p className="mt-1 text-xs text-mist-500">
          Sent to every household member who has set a channel in Settings →
          Notifications. Off means it only appears here, in-app.
        </p>
      )}

      <div className="mt-4 flex flex-wrap items-end gap-4">
        {meta.fields.map((field) => (
          <label key={field.key} className="flex flex-col gap-1 text-sm">
            <span className="text-mist-400">{field.label}</span>
            <div className="flex items-center gap-1">
              {field.kind === 'money' && <span className="text-mist-500">$</span>}
              <input
                className="field w-32"
                inputMode="decimal"
                value={config[field.key] ?? ''}
                onChange={(e) =>
                  setConfig((c) => ({ ...c, [field.key]: e.target.value }))
                }
              />
              {field.kind === 'percent' && <span className="text-mist-500">%</span>}
            </div>
          </label>
        ))}

        <button
          className="btn-primary"
          disabled={save.isPending}
          onClick={() => save.mutate({ enabled, push })}
        >
          {save.isPending ? 'Saving…' : 'Save'}
        </button>
      </div>

      {save.isError && (
        <p role="alert" className="mt-3 text-sm text-ember-400">
          {save.error.message}
        </p>
      )}
    </div>
  )
}

function initialConfig(meta: TypeMeta, existing: Alert | undefined): Record<string, string> {
  const source = existing?.config ?? meta.defaults
  const out: Record<string, string> = {}
  for (const field of meta.fields) {
    const value = source[field.key] ?? meta.defaults[field.key]
    out[field.key] = value === undefined ? '' : String(value)
  }
  return out
}

// coerceConfig turns the string form values back into the shapes the backend
// validates: money stays a decimal string (never a float), counts become ints.
function coerceConfig(
  meta: TypeMeta,
  config: Record<string, string>,
): Record<string, string | number> {
  const out: Record<string, string | number> = {}
  for (const field of meta.fields) {
    const raw = (config[field.key] ?? '').trim()
    if (field.kind === 'percent' || field.kind === 'int') {
      out[field.key] = Number.parseInt(raw, 10) || 0
    } else {
      out[field.key] = raw
    }
  }
  return out
}

function describeEvent(e: AlertEvent): { title: string; detail: string } {
  const p = e.payload
  switch (e.alert_type) {
    case 'big_spend':
      return {
        title: `Large purchase: ${p.merchant ?? 'unknown'}`,
        detail: `${formatMoney(p.amount)} on ${p.date} (over ${formatMoney(p.threshold)})`,
      }
    case 'unusual_merchant':
      return {
        title: `New merchant: ${p.merchant ?? 'unknown'}`,
        detail: `${formatMoney(p.amount)} on ${p.date}`,
      }
    case 'budget_threshold':
      return {
        title: `${p.category_name ?? 'A category'} budget at ${p.percent}%`,
        detail: `Spent ${formatMoney(p.spent)} of ${formatMoney(p.budgeted)} this month`,
      }
    case 'low_leftover':
      return {
        title: `Low leftover for ${p.period}`,
        detail: `${formatMoney(p.leftover)} left (below ${formatMoney(p.floor)})`,
      }
    default:
      return { title: 'Alert', detail: '' }
  }
}
