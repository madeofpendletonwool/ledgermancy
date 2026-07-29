import { useEffect, useState, type ReactNode } from 'react'
import { useSearchParams } from 'react-router-dom'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, isAdult } from '../lib/api'
import type { PreferenceWrite } from '../lib/api'
import { useSession } from '../lib/session'
import { Security } from './Security'
import { Household } from './Household'

type Tab = 'profile' | 'security' | 'notifications' | 'digest' | 'household'

const TABS: { id: Tab; label: string; adultOnly?: boolean }[] = [
  { id: 'profile', label: 'Profile' },
  { id: 'security', label: 'Security' },
  { id: 'notifications', label: 'Notifications', adultOnly: true },
  { id: 'digest', label: 'Digest', adultOnly: true },
  { id: 'household', label: 'Household', adultOnly: true },
]

const isTab = (v: string | null): v is Tab =>
  TABS.some((t) => t.id === v)

export function Settings() {
  // Deep links (e.g. the /household redirect) land on a specific tab via ?tab=.
  const [searchParams] = useSearchParams()
  const { data: user } = useSession()
  const initialTab = searchParams.get('tab')
  const [tab, setTab] = useState<Tab>(isTab(initialTab) ? initialTab : 'profile')

  // A child sees only the tabs that are theirs. The tabs a child cannot open
  // are also refused server-side; hiding them here just avoids offering a
  // door that does not open.
  const tabs = TABS.filter((t) => !t.adultOnly || isAdult(user))
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
      {activeTab === 'household' && <Household />}
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
