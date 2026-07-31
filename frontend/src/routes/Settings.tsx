import { useEffect, useState, type ReactNode } from 'react'
import { useSearchParams } from 'react-router-dom'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, isAdult, isOwner } from '../lib/api'
import type { AnomalyScope, PreferenceWrite } from '../lib/api'
import { useSession } from '../lib/session'
import { Security } from './Security'
import { Household } from './Household'
import { Continuity } from './Continuity'

type Tab = 'profile' | 'security' | 'notifications' | 'digest' | 'anomalies' | 'household' | 'continuity'

const TABS: { id: Tab; label: string; adultOnly?: boolean; ownerOnly?: boolean }[] = [
  { id: 'profile', label: 'Profile' },
  { id: 'security', label: 'Security' },
  { id: 'notifications', label: 'Notifications', adultOnly: true },
  { id: 'digest', label: 'Digest', adultOnly: true },
  { id: 'anomalies', label: 'Anomalies', adultOnly: true },
  { id: 'household', label: 'Household', adultOnly: true },
  // Operator surface: the instance's recovery posture, not the household's
  // data. Owner-only, and enforced server-side by auth.RequireOwner.
  { id: 'continuity', label: 'Continuity', adultOnly: true, ownerOnly: true },
]

const isTab = (v: string | null): v is Tab =>
  TABS.some((t) => t.id === v)

export function Settings() {
  // Deep links (e.g. the /household redirect) land on a specific tab via ?tab=.
  const [searchParams] = useSearchParams()
  const { data: user } = useSession()
  const initialTab = searchParams.get('tab')
  const [tab, setTab] = useState<Tab>(isTab(initialTab) ? initialTab : 'profile')

  // A child sees only the tabs that are theirs, and only the owner sees the
  // operator surface. Every tab hidden here is also refused server-side; this
  // just avoids offering a door that does not open.
  const tabs = TABS.filter(
    (t) => (!t.adultOnly || isAdult(user)) && (!t.ownerOnly || isOwner(user)),
  )
  const activeTab = tabs.some((t) => t.id === tab) ? tab : 'profile'

  return (
    <div className="space-y-8">
      <div>
        <h1 className="text-2xl font-semibold">Settings</h1>
        <p className="mt-1 text-mist-300">
          Your account, how the app reaches you, and what it sends.
        </p>
      </div>

      <div className="flex gap-1 border-b border-white/10">
        {tabs.map((t) => (
          <button
            key={t.id}
            onClick={() => setTab(t.id)}
            className={`-mb-px border-b-2 px-4 py-2 text-sm transition ${
              activeTab === t.id
                ? 'border-arcane-500 text-mist-100'
                : 'border-transparent text-mist-300 hover:text-mist-100'
            }`}
          >
            {t.label}
          </button>
        ))}
      </div>

      {activeTab === 'profile' && <ProfileSection />}
      {activeTab === 'security' && <Security />}
      {activeTab === 'notifications' && <NotificationsSection />}
      {activeTab === 'digest' && <DigestSection />}
      {activeTab === 'anomalies' && <AnomaliesSection />}
      {activeTab === 'household' && <Household />}
      {activeTab === 'continuity' && <Continuity />}
    </div>
  )
}

/**
 * Your own name and birthdate.
 *
 * This is the self-service half of the People list: an adult who never opens
 * the Household page can still fill in their own birthdate, which is what keeps
 * every age-based projection correct without anybody re-typing an age each
 * year.
 */
function ProfileSection() {
  const qc = useQueryClient()
  const person = useQuery({ queryKey: ['my-person'], queryFn: api.myPerson })

  const [name, setName] = useState('')
  const [birthdate, setBirthdate] = useState('')
  const [dirty, setDirty] = useState(false)

  // Seed the form once the person arrives, without clobbering edits in progress.
  useEffect(() => {
    if (person.data && !dirty) {
      setName(person.data.display_name)
      setBirthdate(person.data.birthdate ?? '')
    }
  }, [person.data, dirty])

  const save = useMutation({
    mutationFn: () =>
      api.updateMyPerson({
        display_name: name,
        birthdate: birthdate || null,
        is_dependent: person.data?.is_dependent ?? false,
      }),
    onSuccess: () => {
      setDirty(false)
      qc.invalidateQueries({ queryKey: ['my-person'] })
      qc.invalidateQueries({ queryKey: ['people'] })
      // The retirement projection reads the birthdate, so it is stale now.
      qc.invalidateQueries({ queryKey: ['retirement'] })
    },
  })

  return (
    <section className="glass p-6">
      <h2 className="text-lg font-medium">Profile</h2>
      <p className="mt-1 text-sm text-mist-300">
        How you appear to the rest of the household.
      </p>

      <form
        className="mt-4 max-w-md space-y-4"
        onSubmit={(e) => {
          e.preventDefault()
          save.mutate()
        }}
      >
        <label className="block">
          <span className="text-sm text-mist-300">Name</span>
          <input
            required
            className="field mt-1"
            value={name}
            onChange={(e) => {
              setDirty(true)
              setName(e.target.value)
            }}
          />
        </label>

        <label className="block">
          <span className="text-sm text-mist-300">Birthdate</span>
          <input
            type="date"
            className="field mt-1"
            value={birthdate}
            onChange={(e) => {
              setDirty(true)
              setBirthdate(e.target.value)
            }}
          />
        </label>

        {/* Why it is worth entering, stated once rather than assumed. */}
        {!person.data?.birthdate && (
          <p className="rounded-xl border border-rune-400/25 bg-rune-400/5 px-4 py-3 text-xs text-mist-300">
            Setting your birthdate lets the app work out your age itself. Catch-up
            contribution limits, a 529's college horizon and every "at 67" figure
            stop needing an age you'd otherwise have to retype every year — and
            an age typed once is wrong within twelve months.
          </p>
        )}

        {person.data?.age !== null && person.data?.age !== undefined && (
          <p className="text-xs text-mist-500">
            You are {person.data.age}. Every projection uses this rather than a
            stored number.
          </p>
        )}

        {save.isError && (
          <p role="alert" className="text-sm text-ember-400">
            {save.error.message}
          </p>
        )}

        <button className="btn-primary" disabled={save.isPending}>
          {save.isPending ? 'Saving…' : 'Save'}
        </button>
      </form>
    </section>
  )
}

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

  // Seed the form once the stored values arrive. Keyed on the fetched object so
  // it re-seeds after a refetch but not on every keystroke.
  useEffect(() => {
    const u = prefs.data?.user
    if (!u) return
    setChannel(asString(u['notify.channel'], 'none'))
    setTopic(asString(u['notify.ntfy_topic'], ''))
  }, [prefs.data])

  // Sends to the SAVED topic (the server reads preferences), so a test is only
  // meaningful once the current channel/topic have been saved.
  const test = useMutation({ mutationFn: () => api.testNotification() })

  const dirty =
    asString(prefs.data?.user?.['notify.channel'], 'none') !== channel ||
    asString(prefs.data?.user?.['notify.ntfy_topic'], '') !== topic

  const onSave = () =>
    save.mutate([
      { scope: 'user', key: 'notify.channel', value: channel },
      { scope: 'user', key: 'notify.ntfy_topic', value: topic },
    ])

  return (
    <Section
      title="Notifications"
      description="Where the app reaches you. Pick a channel and topic here; choose which alerts push on the Alerts page."
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

          <SaveRow save={save} onSave={onSave} />

          {channel === 'ntfy' && (
            <div className="border-t border-white/10 pt-4">
              <div className="flex flex-wrap items-center gap-3">
                <button
                  className="btn-ghost px-4 py-2 text-sm"
                  disabled={test.isPending || topic.trim() === '' || dirty}
                  onClick={() => test.mutate()}
                >
                  {test.isPending ? 'Sending…' : 'Send test'}
                </button>
                {dirty ? (
                  <span className="text-sm text-mist-500">
                    Save your changes first, then send a test.
                  </span>
                ) : test.isError ? (
                  <span role="alert" className="text-sm text-ember-400">
                    {test.error.message}
                  </span>
                ) : test.isSuccess ? (
                  <span className="text-sm text-rune-300">
                    Sent — check your device.
                  </span>
                ) : (
                  <span className="text-sm text-mist-500">
                    Delivers one test push to your saved topic.
                  </span>
                )}
              </div>
            </div>
          )}
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

  // Queues a digest immediately, independent of the cadence and opt-in above, so
  // you can see what one looks like without waiting for the schedule.
  const sendNow = useMutation({ mutationFn: () => api.sendDigestNow() })

  const onSave = () =>
    save.mutate([
      { scope: 'user', key: 'digest.enabled', value: enabled },
      { scope: 'user', key: 'digest.cadence', value: cadence },
    ])

  return (
    <Section
      title="Digest"
      description="A periodic recap — your monthly narrative plus the top insights — pushed to you on a schedule. It's delivered through your notification channel, so set one up in Notifications first."
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

          <div className="border-t border-white/10 pt-4">
            <div className="flex flex-wrap items-center gap-3">
              <button
                className="btn-ghost px-4 py-2 text-sm"
                disabled={sendNow.isPending}
                onClick={() => sendNow.mutate()}
              >
                {sendNow.isPending ? 'Queueing…' : 'Send one now'}
              </button>
              {sendNow.isError ? (
                <span role="alert" className="text-sm text-ember-400">
                  {sendNow.error.message}
                </span>
              ) : sendNow.isSuccess ? (
                <span className="text-sm text-rune-300">
                  Queued — it’ll arrive shortly.
                </span>
              ) : (
                <span className="text-sm text-mist-500">
                  Sends a digest to your channel right now, ignoring the schedule.
                </span>
              )}
            </div>
          </div>
        </div>
      )}
    </Section>
  )
}

const SENSITIVITY_HELP: Record<string, string> = {
  conservative:
    'Only the clearest cases. Needs a longer history at a merchant and a much bigger departure from it before saying anything.',
  balanced: 'The default. Flags a charge around 3× what a merchant normally bills, above $50.',
  sensitive:
    'Speaks up more often — around 2× the usual charge, above $25. Expect some charges you already know about.',
}

/**
 * Anomaly detection settings: how eagerly the two detectors speak, plus the
 * restore list for merchants marked normal.
 *
 * Household-scoped, not per-user: sensitivity changes what everyone's feed says,
 * so it should not be something one member can quietly tighten for the others.
 * The server enforces that only an adult can write a household preference.
 */
function AnomaliesSection() {
  const qc = useQueryClient()
  const prefs = useQuery({ queryKey: ['preferences'], queryFn: api.preferences })
  const save = useSavePreferences()

  const [sensitivity, setSensitivity] = useState('balanced')

  useEffect(() => {
    const h = prefs.data?.household
    if (!h) return
    setSensitivity(asString(h['anomaly.sensitivity'], 'balanced'))
  }, [prefs.data])

  const suppressed = useQuery({
    queryKey: ['suppressed-anomalies'],
    queryFn: api.suppressedAnomalies,
  })
  const restore = useMutation({
    mutationFn: ({ key, scope }: { key: string; scope: AnomalyScope }) =>
      api.unsuppressAnomaly(key, scope),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['suppressed-anomalies'] })
      qc.invalidateQueries({ queryKey: ['insights'] })
    },
  })

  const onSave = () =>
    save.mutate([{ scope: 'household', key: 'anomaly.sensitivity', value: sensitivity }])

  const rows = suppressed.data ?? []

  return (
    <Section
      title="Anomalies"
      description="Unusual charges and possible duplicate charges are compared against what each merchant normally bills you. Detection is arithmetic over your own history — no AI decides what counts as unusual."
    >
      {prefs.isPending ? (
        <Loading />
      ) : (
        <div className="space-y-5">
          <div>
            <label className="label" htmlFor="anomaly-sensitivity">
              Sensitivity
            </label>
            <select
              id="anomaly-sensitivity"
              className="field"
              value={sensitivity}
              onChange={(e) => setSensitivity(e.target.value)}
            >
              <option value="conservative">Conservative</option>
              <option value="balanced">Balanced</option>
              <option value="sensitive">Sensitive</option>
            </select>
            <p className="mt-2 text-sm text-mist-500">
              {SENSITIVITY_HELP[sensitivity] ?? SENSITIVITY_HELP.balanced}
            </p>
          </div>

          <SaveRow save={save} onSave={onSave} />

          <div className="border-t border-white/10 pt-4">
            <h3 className="text-sm font-medium text-mist-100">Merchants marked normal</h3>
            <p className="mt-1 text-sm text-mist-500">
              Marking an insight “This is normal” stops that detector flagging the merchant.
              Restore one to start hearing about it again.
            </p>
            {suppressed.isPending ? (
              <Loading />
            ) : rows.length === 0 ? (
              <p className="mt-3 text-sm text-mist-500">Nothing suppressed.</p>
            ) : (
              <ul className="mt-3 space-y-2">
                {rows.map((m) => (
                  <li
                    key={`${m.merchant_key}:${m.scope}`}
                    className="flex items-center justify-between gap-3 rounded-lg border border-white/5 bg-white/[0.02] px-3 py-2"
                  >
                    <span className="min-w-0 truncate text-sm text-mist-100">
                      {m.merchant || m.merchant_key}
                      <span className="ml-2 text-xs text-mist-500">
                        {m.scope === 'all'
                          ? 'all anomalies'
                          : m.scope === 'outlier'
                            ? 'unusual charges'
                            : 'duplicates'}
                      </span>
                    </span>
                    <button
                      className="shrink-0 text-xs text-mist-500 transition hover:text-mist-100 disabled:opacity-50"
                      disabled={restore.isPending}
                      onClick={() => restore.mutate({ key: m.merchant_key, scope: m.scope })}
                    >
                      Restore
                    </button>
                  </li>
                ))}
              </ul>
            )}
          </div>
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
