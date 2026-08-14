import { useEffect, useState, type ReactNode } from 'react'
import { Link, useSearchParams } from 'react-router-dom'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, isAdult, isOwner } from '../lib/api'
import type { AnomalyScope, PreferenceWrite } from '../lib/api'
import { useSession } from '../lib/session'
import { Security } from './Security'
import { Household } from './Household'
import { Continuity } from './Continuity'
import { Webhooks } from './Webhooks'
import { SystemStatus } from './SystemStatus'
import { SkeletonRows } from '../components/Skeleton'

type Tab =
  | 'profile'
  | 'security'
  | 'notifications'
  | 'digest'
  | 'anomalies'
  | 'advisor'
  | 'appearance'
  | 'household'
  | 'webhooks'
  | 'continuity'
  | 'system'

const TABS: { id: Tab; label: string; adultOnly?: boolean; ownerOnly?: boolean }[] = [
  { id: 'profile', label: 'Profile' },
  { id: 'security', label: 'Security' },
  { id: 'notifications', label: 'Notifications', adultOnly: true },
  { id: 'digest', label: 'Digest', adultOnly: true },
  { id: 'anomalies', label: 'Anomalies', adultOnly: true },
  // The advisor reads the household's whole financial position, and its knobs
  // change what every member is shown. Adult-only, like the other household
  // settings, and enforced server-side.
  { id: 'advisor', label: 'Advisor', adultOnly: true },
  // Household-wide imagery, so adult-only like the other household settings.
  { id: 'appearance', label: 'Appearance', adultOnly: true },
  { id: 'household', label: 'Household', adultOnly: true },
  // Where this household's events go when they leave the app. Adult-only like
  // the other household settings — a child login has no business wiring the
  // family's finances to an address of its choosing — and enforced server-side.
  { id: 'webhooks', label: 'Webhooks', adultOnly: true },
  // Operator surface: the instance's recovery posture, not the household's
  // data. Owner-only, and enforced server-side by auth.RequireOwner.
  { id: 'continuity', label: 'Continuity', adultOnly: true, ownerOnly: true },
  // The other half of the operator surface: continuity is "could I recover",
  // system is "is it working right now". Owner-only for the same reason.
  { id: 'system', label: 'System', adultOnly: true, ownerOnly: true },
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

      <div className="flex gap-1 overflow-x-auto border-b border-white/10">
        {tabs.map((t) => (
          <button
            key={t.id}
            onClick={() => setTab(t.id)}
            className={`-mb-px shrink-0 whitespace-nowrap border-b-2 px-4 py-2 text-sm transition ${
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
      {activeTab === 'advisor' && <AdvisorSection />}
      {activeTab === 'appearance' && <AppearanceSection />}
      {activeTab === 'household' && <Household />}
      {activeTab === 'webhooks' && <Webhooks />}
      {activeTab === 'continuity' && <Continuity />}
      {activeTab === 'system' && <SystemStatus />}
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
        <SkeletonRows count={3} />
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

/**
 * Digest settings: one cadence, and one switch per surface.
 *
 * The surfaces are deliberately independent. In-app is on by default and needs
 * nothing configured; push needs a notification channel; email needs the
 * operator to have set up SMTP. Turning one off never silences the others, and
 * turning the cadence knob moves all three together — that is the only thing
 * they share.
 */
function DigestSection() {
  const prefs = useQuery({ queryKey: ['preferences'], queryFn: api.preferences })
  const capabilities = useQuery({
    queryKey: ['capabilities'],
    queryFn: api.capabilities,
    staleTime: Infinity,
  })
  const save = useSavePreferences()

  const [inApp, setInApp] = useState(true)
  const [push, setPush] = useState(false)
  const [email, setEmail] = useState(false)
  const [cadence, setCadence] = useState('weekly')

  useEffect(() => {
    const u = prefs.data?.user
    if (!u) return
    // in_app defaults ON, so an unset value must not read as false. The server
    // sends the reserved default, but asBool would still turn an absent key
    // into false — hence the explicit fallback.
    setInApp(u['digest.in_app'] === undefined ? true : asBool(u['digest.in_app']))
    setPush(asBool(u['digest.enabled']))
    setEmail(asBool(u['digest.email']))
    setCadence(asString(u['digest.cadence'], 'weekly'))
  }, [prefs.data])

  const channel = asString(prefs.data?.user?.['notify.channel'], 'none')
  const hasChannel = channel !== '' && channel !== 'none'
  const smtpEnabled = capabilities.data?.smtp_enabled ?? false

  // Queues a digest immediately, independent of the cadence and switches above,
  // so you can see what one looks like without waiting for the schedule.
  const sendNow = useMutation({ mutationFn: () => api.sendDigestNow() })

  const onSave = () =>
    save.mutate([
      { scope: 'user', key: 'digest.in_app', value: inApp },
      { scope: 'user', key: 'digest.enabled', value: push },
      { scope: 'user', key: 'digest.email', value: email },
      { scope: 'user', key: 'digest.cadence', value: cadence },
    ])

  return (
    <Section
      title="Digest"
      description="A periodic recap — the period's figures, your narrative and the top insights. It's kept in the app, and can also be pushed or emailed to you."
    >
      {prefs.isPending ? (
        <SkeletonRows count={4} />
      ) : (
        <div className="space-y-5">
          <div>
            <label className="label" htmlFor="digest-cadence">
              Cadence
            </label>
            <select
              id="digest-cadence"
              className="field"
              value={cadence}
              onChange={(e) => setCadence(e.target.value)}
            >
              <option value="weekly">Weekly</option>
              <option value="monthly">Monthly</option>
            </select>
            <p className="mt-1 text-xs text-mist-500">
              Weekly digests are written on a Monday; monthly ones on the 1st,
              covering the month just finished.
            </p>
          </div>

          <fieldset className="space-y-3">
            <legend className="label">Where it goes</legend>

            <Toggle
              checked={inApp}
              onChange={setInApp}
              label="Keep it in the app"
              hint={
                <>
                  Builds your <Link to="/digest" className="text-rune-300 hover:underline">Digest</Link>{' '}
                  history. Nothing to configure, and past digests stay readable.
                </>
              }
            />

            <Toggle
              checked={push}
              onChange={setPush}
              label="Push it to me"
              hint={
                hasChannel
                  ? `Sent over your ${channel} channel.`
                  : 'Set up a notification channel first — there is nowhere to push to yet.'
              }
            />

            <Toggle
              checked={email}
              onChange={setEmail}
              disabled={!smtpEnabled}
              label="Email it to me"
              hint={
                smtpEnabled
                  ? 'Plain text, to your account address.'
                  : 'No mail server is configured on this deployment.'
              }
            />
          </fieldset>

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
                  Queued — check your Digest page shortly.
                </span>
              ) : (
                <span className="text-sm text-mist-500">
                  Writes a digest for the current period right now, ignoring the
                  schedule.
                </span>
              )}
            </div>
          </div>
        </div>
      )}
    </Section>
  )
}

/** A labelled checkbox with a line of explanation under it. */
function Toggle({
  checked,
  onChange,
  label,
  hint,
  disabled,
}: {
  checked: boolean
  onChange: (v: boolean) => void
  label: string
  hint: ReactNode
  disabled?: boolean
}) {
  return (
    <div>
      <label
        className={`flex items-center gap-2 text-sm ${disabled ? 'text-mist-500' : ''}`}
      >
        <input
          type="checkbox"
          checked={checked && !disabled}
          disabled={disabled}
          onChange={(e) => onChange(e.target.checked)}
        />
        {label}
      </label>
      <p className="mt-0.5 pl-6 text-xs text-mist-500">{hint}</p>
    </div>
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
        <SkeletonRows count={3} />
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
              <SkeletonRows count={3} />
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

/**
 * Appearance: the household's merchant imagery.
 *
 * One switch today, and it is worth reading the two-layer shape before adding a
 * second. Fetching a real logo means contacting a company that is neither Plaid
 * nor your AI provider, so the decision to do that at all is the OPERATOR's
 * (`MERCHANT_LOGOS_ENABLED` in `.env`, off by default, documented there with the
 * destination). This toggle is the household's say over what it already permits:
 * on by default once the operator opted in, and turning it off both stops the
 * imagery and deletes what was already fetched.
 *
 * With the operator switch off, the toggle still saves — it just explains that
 * nothing will happen until the deployment opts in, the same courtesy the
 * Notifications tab extends when no ntfy server is configured.
 */
function AppearanceSection() {
  const prefs = useQuery({ queryKey: ['preferences'], queryFn: api.preferences })
  const capabilities = useQuery({
    queryKey: ['capabilities'],
    queryFn: api.capabilities,
    staleTime: Infinity,
  })
  const qc = useQueryClient()
  const save = useSavePreferences()

  const [logos, setLogos] = useState(true)

  useEffect(() => {
    const h = prefs.data?.household
    if (!h) return
    setLogos(asBool(h['merchant.logos']))
  }, [prefs.data])

  const onSave = () => {
    save.mutate([{ scope: 'household', key: 'merchant.logos', value: logos }], {
      // Avatars read the resolved answer off /capabilities, so it is stale the
      // moment this is saved.
      onSuccess: () => qc.invalidateQueries({ queryKey: ['capabilities'] }),
    })
  }

  const available = capabilities.data?.merchant_logos_available === true

  return (
    <Section
      title="Appearance"
      description="How merchants look around the app. Every merchant has a coloured monogram built from its own name — no network, no third party, always there. Real logos are an extra on top of that, and an opt-in one."
    >
      {prefs.isPending ? (
        <SkeletonRows count={2} />
      ) : (
        <div className="space-y-5">
          {capabilities.data && !available && (
            <p className="rounded-xl border border-white/10 bg-white/5 px-4 py-2.5 text-sm text-mist-300">
              Merchant logos are unavailable — this deployment has not opted in.
              An operator enables them with <code>MERCHANT_LOGOS_ENABLED=true</code>,
              a free Logo.dev token and an AI key. You can still set a preference
              here; it takes effect if that happens.
            </p>
          )}

          <Toggle
            checked={logos}
            onChange={setLogos}
            label="Show merchant logos"
            hint={
              <>
                When on, this app looks up each merchant’s website with your AI
                provider and fetches that company’s logo from Logo.dev — on the
                server, once per merchant, then cached and served from here. Your
                browser never contacts Logo.dev, and no amount, balance or
                transaction is ever sent; only the merchant’s name. Merchants
                without a known logo keep their monogram. Turning this off stops
                the lookups and deletes the logos already stored.
              </>
            }
          />

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

/**
 * Advisor settings: when it speaks, what it counts as a full emergency fund,
 * and which options it has been told to stop suggesting.
 *
 * Household-scoped throughout. These change what everyone sees, and the server
 * refuses a household preference from a child login.
 *
 * The suppression list is the interesting one. Dismissing an advisor insight
 * clears that week's whole list — the feed's grain — whereas muting an option
 * here is durable and per option. The two are deliberately different things, so
 * this is the only place a mute can be undone.
 */
function AdvisorSection() {
  const prefs = useQuery({ queryKey: ['preferences'], queryFn: api.preferences })
  const advice = useQuery({ queryKey: ['advisor'], queryFn: api.advisor })
  const qc = useQueryClient()
  const save = useSavePreferences()

  const [threshold, setThreshold] = useState('100')
  const [efMonths, setEfMonths] = useState('3')

  useEffect(() => {
    const h = prefs.data?.household
    if (!h) return
    setThreshold(asNumberText(h['advisor.slack_threshold'], '100'))
    setEfMonths(asNumberText(h['advisor.emergency_fund_months'], '3'))
  }, [prefs.data])

  const suppressed = advice.data?.suppressed ?? []

  const onSave = () =>
    save.mutate([
      {
        scope: 'household',
        key: 'advisor.slack_threshold',
        value: Number(threshold) || 0,
      },
      {
        scope: 'household',
        key: 'advisor.emergency_fund_months',
        value: Number(efMonths) || 3,
      },
    ])

  // Un-muting rewrites the whole list rather than patching it: the preference IS
  // the list, and a partial write would be a second source of truth for it.
  const unmute = useMutation({
    mutationFn: (key: string) =>
      api.setPreferences([
        {
          scope: 'household',
          key: 'advisor.suppressed_options',
          value: suppressed.filter((k) => k !== key),
        },
      ]),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['preferences'] })
      qc.invalidateQueries({ queryKey: ['advisor'] })
    },
  })

  return (
    <Section
      title="Advisor"
      description="When there is money left over in a typical month, the advisor ranks what it would do with it. Every figure and the order itself are computed from your own data — AI only reads the finished list aloud, and nothing here moves any money."
    >
      {prefs.isPending ? (
        <SkeletonRows count={3} />
      ) : (
        <div className="space-y-5">
          <div>
            <label className="label" htmlFor="advisor-threshold">
              Only speak up above
            </label>
            <input
              id="advisor-threshold"
              className="field"
              type="number"
              min="0"
              step="10"
              value={threshold}
              onChange={(e) => setThreshold(e.target.value)}
            />
            <p className="mt-2 text-sm text-mist-500">
              Dollars of monthly slack before the advisor says anything. An
              advisor that fires on $12 is one you learn to ignore.
            </p>
          </div>

          <div>
            <label className="label" htmlFor="advisor-ef-months">
              Emergency fund target
            </label>
            <input
              id="advisor-ef-months"
              className="field"
              type="number"
              min="1"
              max="24"
              step="1"
              value={efMonths}
              onChange={(e) => setEfMonths(e.target.value)}
            />
            <p className="mt-2 text-sm text-mist-500">
              Months of your typical fixed costs. Until you hold one month, that
              is the only option the advisor offers — no return beats not
              borrowing at card rates for the next emergency.
            </p>
          </div>

          <SaveRow save={save} onSave={onSave} />

          <div className="border-t border-white/10 pt-4">
            <h3 className="text-sm font-medium text-mist-100">Muted options</h3>
            <p className="mt-1 text-sm text-mist-500">
              Options you have told the advisor to stop suggesting. This is
              separate from dismissing an insight, which only clears that week&rsquo;s
              list.
            </p>
            {suppressed.length === 0 ? (
              <p className="mt-3 text-sm text-mist-500">Nothing muted.</p>
            ) : (
              <ul className="mt-3 space-y-2">
                {suppressed.map((k) => (
                  <li
                    key={k}
                    className="flex items-center justify-between gap-3 rounded-lg border border-white/5 bg-white/[0.02] px-3 py-2"
                  >
                    <span className="min-w-0 truncate text-sm text-mist-100">{k}</span>
                    <button
                      className="shrink-0 text-xs text-mist-500 transition hover:text-mist-100 disabled:opacity-50"
                      disabled={unmute.isPending}
                      onClick={() => unmute.mutate(k)}
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

// The preference values arrive as parsed JSON of unknown shape; these coerce a
// value to the type a control expects, falling back when a key is unset.
function asString(v: unknown, fallback: string): string {
  return typeof v === 'string' ? v : fallback
}

function asBool(v: unknown): boolean {
  return v === true
}

// A numeric preference for a text input. Accepts the number the API writes and
// the string an older client may have left, so a value typed before this control
// existed is still editable rather than silently reset to the default.
function asNumberText(v: unknown, fallback: string): string {
  if (typeof v === 'number' && Number.isFinite(v)) return String(v)
  if (typeof v === 'string' && v !== '' && Number.isFinite(Number(v))) return v
  return fallback
}
