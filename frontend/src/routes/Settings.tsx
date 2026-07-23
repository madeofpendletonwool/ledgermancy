import { useEffect, useState, type ReactNode } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '../lib/api'
import type { PreferenceWrite } from '../lib/api'
import { Security } from './Security'

type Tab = 'security' | 'notifications' | 'digest'

const TABS: { id: Tab; label: string }[] = [
  { id: 'security', label: 'Security' },
  { id: 'notifications', label: 'Notifications' },
  { id: 'digest', label: 'Digest' },
]

export function Settings() {
  const [tab, setTab] = useState<Tab>('security')

  return (
    <div className="space-y-8">
      <div>
        <h1 className="text-2xl font-semibold">Settings</h1>
        <p className="mt-1 text-mist-300">
          Your account, how the app reaches you, and what it sends.
        </p>
      </div>

      <div className="flex gap-1 border-b border-white/10">
        {TABS.map((t) => (
          <button
            key={t.id}
            onClick={() => setTab(t.id)}
            className={`-mb-px border-b-2 px-4 py-2 text-sm transition ${
              tab === t.id
                ? 'border-arcane-500 text-mist-100'
                : 'border-transparent text-mist-300 hover:text-mist-100'
            }`}
          >
            {t.label}
          </button>
        ))}
      </div>

      {tab === 'security' && <Security />}
      {tab === 'notifications' && <NotificationsSection />}
      {tab === 'digest' && <DigestSection />}
    </div>
  )
}

// The insight/alert kinds a user can choose to have pushed. Hardcoded for now —
// 04 owns the canonical enumeration; this list mirrors the alert types plus the
// initial insight kinds so the control is useful before those land.
const PUSH_KINDS: { value: string; label: string }[] = [
  { value: 'big_spend', label: 'Big spend' },
  { value: 'budget_threshold', label: 'Budget threshold' },
  { value: 'unusual_merchant', label: 'Unusual merchant' },
  { value: 'low_leftover', label: 'Low leftover' },
  { value: 'spending_spike', label: 'Spending spike' },
  { value: 'new_recurring', label: 'New recurring charge' },
  { value: 'subscription', label: 'Subscription' },
  { value: 'forecast', label: 'Forecast' },
]

function NotificationsSection() {
  const prefs = useQuery({ queryKey: ['preferences'], queryFn: api.preferences })
  const capabilities = useQuery({
    queryKey: ['capabilities'],
    queryFn: api.capabilities,
    staleTime: Infinity,
  })
  const save = useSavePreferences()

  const [channel, setChannel] = useState('none')
  const [topic, setTopic] = useState('')
  const [kinds, setKinds] = useState<string[]>([])

  // Seed the form once the stored values arrive. Keyed on the fetched object so
  // it re-seeds after a refetch but not on every keystroke.
  useEffect(() => {
    const u = prefs.data?.user
    if (!u) return
    setChannel(asString(u['notify.channel'], 'none'))
    setTopic(asString(u['notify.ntfy_topic'], ''))
    setKinds(asStringArray(u['notify.push_kinds']))
  }, [prefs.data])

  const toggleKind = (value: string) =>
    setKinds((prev) =>
      prev.includes(value) ? prev.filter((k) => k !== value) : [...prev, value],
    )

  const onSave = () =>
    save.mutate([
      { scope: 'user', key: 'notify.channel', value: channel },
      { scope: 'user', key: 'notify.ntfy_topic', value: topic },
      { scope: 'user', key: 'notify.push_kinds', value: kinds },
    ])

  return (
    <Section
      title="Notifications"
      description="Where the app pushes alerts and insights. Delivery turns on once a channel is configured."
    >
      {prefs.isPending ? (
        <Loading />
      ) : (
        <div className="space-y-5">
          {capabilities.data && !capabilities.data.notify_enabled && (
            <p className="rounded-xl border border-white/10 bg-white/5 px-4 py-2.5 text-sm text-mist-300">
              Notifications are unavailable — no ntfy server is configured on this
              deployment. You can still save a preference; it takes effect once a
              server is set up.
            </p>
          )}
          <div>
            <label className="label" htmlFor="notify-channel">
              Channel
            </label>
            <select
              id="notify-channel"
              className="field"
              value={channel}
              onChange={(e) => setChannel(e.target.value)}
            >
              <option value="none">None</option>
              <option value="ntfy">ntfy</option>
            </select>
          </div>

          {channel === 'ntfy' && (
            <div>
              <label className="label" htmlFor="notify-topic">
                ntfy topic
              </label>
              <input
                id="notify-topic"
                className="field"
                placeholder="your-private-topic"
                value={topic}
                onChange={(e) => setTopic(e.target.value)}
              />
              <p className="mt-1 text-xs text-mist-500">
                A private topic name only you know. Subscribe to it in the ntfy app
                to receive pushes.
              </p>
            </div>
          )}

          <fieldset>
            <legend className="label">What to push</legend>
            <div className="mt-2 grid gap-2 sm:grid-cols-2">
              {PUSH_KINDS.map((k) => (
                <label key={k.value} className="flex items-center gap-2 text-sm">
                  <input
                    type="checkbox"
                    checked={kinds.includes(k.value)}
                    onChange={() => toggleKind(k.value)}
                  />
                  {k.label}
                </label>
              ))}
            </div>
          </fieldset>

          <SaveRow save={save} onSave={onSave} />
        </div>
      )}
    </Section>
  )
}

function DigestSection() {
  const prefs = useQuery({ queryKey: ['preferences'], queryFn: api.preferences })
  const save = useSavePreferences()

  const [enabled, setEnabled] = useState(false)
  const [cadence, setCadence] = useState('weekly')

  useEffect(() => {
    const u = prefs.data?.user
    if (!u) return
    setEnabled(asBool(u['digest.enabled']))
    setCadence(asString(u['digest.cadence'], 'weekly'))
  }, [prefs.data])

  const onSave = () =>
    save.mutate([
      { scope: 'user', key: 'digest.enabled', value: enabled },
      { scope: 'user', key: 'digest.cadence', value: cadence },
    ])

  return (
    <Section
      title="Digest"
      description="A periodic recap of your finances. Sending begins once the digest job is enabled."
    >
      {prefs.isPending ? (
        <Loading />
      ) : (
        <div className="space-y-5">
          <label className="flex items-center gap-2 text-sm">
            <input
              type="checkbox"
              checked={enabled}
              onChange={(e) => setEnabled(e.target.checked)}
            />
            Send me a digest
          </label>

          <div>
            <label className="label" htmlFor="digest-cadence">
              Cadence
            </label>
            <select
              id="digest-cadence"
              className="field"
              value={cadence}
              disabled={!enabled}
              onChange={(e) => setCadence(e.target.value)}
            >
              <option value="weekly">Weekly</option>
              <option value="monthly">Monthly</option>
            </select>
          </div>

          <SaveRow save={save} onSave={onSave} />
        </div>
      )}
    </Section>
  )
}

// useSavePreferences wraps the PUT and refreshes the cached preferences so both
// tabs stay in step after a write.
function useSavePreferences() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (items: PreferenceWrite[]) => api.setPreferences(items),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['preferences'] }),
  })
}

function SaveRow({
  save,
  onSave,
}: {
  save: ReturnType<typeof useSavePreferences>
  onSave: () => void
}) {
  return (
    <div className="flex items-center gap-3">
      <button
        className="btn-primary px-4 py-2 text-sm"
        disabled={save.isPending}
        onClick={onSave}
      >
        {save.isPending ? 'Saving…' : 'Save'}
      </button>
      {save.isError && (
        <span role="alert" className="text-sm text-ember-400">
          {save.error.message}
        </span>
      )}
      {save.isSuccess && !save.isPending && (
        <span className="text-sm text-rune-300">Saved</span>
      )}
    </div>
  )
}

function Section({
  title,
  description,
  children,
}: {
  title: string
  description?: string
  children: ReactNode
}) {
  return (
    <section className="glass p-6">
      <h2 className="text-lg font-medium">{title}</h2>
      {description && <p className="mt-1 mb-4 text-sm text-mist-300">{description}</p>}
      {!description && <div className="mt-4" />}
      {children}
    </section>
  )
}

function Loading() {
  return <p className="py-8 text-center text-sm text-mist-500">Loading…</p>
}

// The preference values arrive as parsed JSON of unknown shape; these coerce a
// value to the type a control expects, falling back when a key is unset.
function asString(v: unknown, fallback: string): string {
  return typeof v === 'string' ? v : fallback
}

function asBool(v: unknown): boolean {
  return v === true
}

function asStringArray(v: unknown): string[] {
  return Array.isArray(v) ? v.filter((x): x is string => typeof x === 'string') : []
}
