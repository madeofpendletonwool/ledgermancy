import { useState, type FormEvent, type ReactNode } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '../lib/api'
import { useChangePassword } from '../lib/session'

export function Security() {
  return (
    <div className="space-y-8">
      <div>
        <h1 className="text-2xl font-semibold">Security</h1>
        <p className="mt-1 text-mist-300">
          This account can reach every balance and transaction in the household.
          Treat it accordingly.
        </p>
      </div>

      <TwoFactorSection />
      <PasswordSection />
      <SessionsSection />
      <ActivitySection />
    </div>
  )
}

// ---------------------------------------------------------------------------
// Two-factor authentication
// ---------------------------------------------------------------------------

function TwoFactorSection() {
  const qc = useQueryClient()
  const status = useQuery({ queryKey: ['mfa'], queryFn: api.mfaStatus })

  // Enrolment is a three-step conversation, so the component tracks where it is
  // rather than trying to infer it from server state alone.
  const [password, setPassword] = useState('')
  const [code, setCode] = useState('')
  // Held only to be displayed. Never persisted, and cleared when dismissed —
  // the server keeps hashes only, so this is the one moment they exist.
  const [codes, setCodes] = useState<string[] | null>(null)

  const setup = useMutation({ mutationFn: api.mfaSetup })

  const activate = useMutation({
    mutationFn: api.mfaActivate,
    onSuccess: (res) => {
      setCodes(res.recovery_codes)
      setCode('')
      setPassword('')
      setup.reset()
      qc.invalidateQueries({ queryKey: ['mfa'] })
      // Enabling MFA revokes every other session, so the list is now stale.
      qc.invalidateQueries({ queryKey: ['sessions'] })
    },
  })

  const disable = useMutation({
    mutationFn: ({ password, code }: { password: string; code: string }) =>
      api.mfaDisable(password, code),
    onSuccess: () => {
      setPassword('')
      setCode('')
      qc.invalidateQueries({ queryKey: ['mfa'] })
    },
  })

  const regenerate = useMutation({
    mutationFn: api.regenerateRecoveryCodes,
    onSuccess: (res) => {
      setCodes(res.recovery_codes)
      setPassword('')
      qc.invalidateQueries({ queryKey: ['mfa'] })
    },
  })

  const enabled = status.data?.enabled ?? false

  if (codes) {
    return (
      <Section title="Save your recovery codes">
        <RecoveryCodeList codes={codes} onDismiss={() => setCodes(null)} />
      </Section>
    )
  }

  return (
    <Section
      title="Two-factor authentication"
      description={
        enabled
          ? 'Your authenticator app is required to sign in.'
          : 'Add a code from your phone to every sign-in. Strongly recommended.'
      }
    >
      {status.isPending && <p className="text-sm text-mist-500">Loading…</p>}

      {enabled && status.data && (
        <>
          <StatusPill tone="good">
            Enabled
            {status.data.confirmed_at &&
              ` since ${new Date(status.data.confirmed_at).toLocaleDateString()}`}
          </StatusPill>

          <p className="mt-3 text-sm text-mist-300">
            {status.data.recovery_codes_remaining} unused recovery{' '}
            {status.data.recovery_codes_remaining === 1 ? 'code' : 'codes'} remaining.
          </p>
          {status.data.recovery_codes_remaining <= 2 && (
            <p className="mt-1 text-sm text-ember-400">
              You are nearly out. Generate a new set while you still can — without
              them and without your phone, only a database edit can get you back in.
            </p>
          )}

          <form
            className="mt-5 space-y-3"
            onSubmit={(e: FormEvent) => {
              e.preventDefault()
              regenerate.mutate(password)
            }}
          >
            <PasswordField value={password} onChange={setPassword} label="Confirm your password" />
            <div className="flex flex-col gap-2 sm:flex-row">
              <button
                type="submit"
                className="btn-ghost"
                disabled={regenerate.isPending || !password}
              >
                {regenerate.isPending ? 'Generating…' : 'Generate new recovery codes'}
              </button>
            </div>
            {regenerate.isError && <ErrorNote>{regenerate.error.message}</ErrorNote>}
          </form>

          <details className="mt-6 border-t border-white/5 pt-5">
            <summary className="cursor-pointer text-sm text-mist-500 hover:text-mist-300">
              Turn off two-factor authentication
            </summary>
            <form
              className="mt-4 space-y-3"
              onSubmit={(e: FormEvent) => {
                e.preventDefault()
                disable.mutate({ password, code })
              }}
            >
              <p className="text-sm text-mist-300">
                Both your password and a current code are required — if either
                alone were enough, the second factor would not be protecting
                anything.
              </p>
              <PasswordField value={password} onChange={setPassword} label="Password" />
              <CodeField value={code} onChange={setCode} />
              <button
                type="submit"
                className="btn-ghost text-ember-400"
                disabled={disable.isPending || !password || !code}
              >
                {disable.isPending ? 'Disabling…' : 'Disable two-factor'}
              </button>
              {disable.isError && <ErrorNote>{disable.error.message}</ErrorNote>}
            </form>
          </details>
        </>
      )}

      {!enabled && status.data && !setup.data && (
        <form
          className="space-y-3"
          onSubmit={(e: FormEvent) => {
            e.preventDefault()
            setup.mutate(password)
          }}
        >
          <StatusPill tone="warn">Not enabled</StatusPill>
          <p className="text-sm text-mist-300">
            Your password is required again here. Without that, anyone who got
            hold of a signed-in browser could attach their own authenticator.
          </p>
          <PasswordField value={password} onChange={setPassword} label="Confirm your password" />
          <button type="submit" className="btn-primary" disabled={setup.isPending || !password}>
            {setup.isPending ? 'Preparing…' : 'Set up two-factor'}
          </button>
          {setup.isError && <ErrorNote>{setup.error.message}</ErrorNote>}
        </form>
      )}

      {!enabled && setup.data && (
        <form
          className="space-y-4"
          onSubmit={(e: FormEvent) => {
            e.preventDefault()
            activate.mutate(code)
          }}
        >
          <ol className="space-y-4 text-sm text-mist-300">
            <li>
              <span className="font-medium text-mist-100">1. Scan this</span> with
              Google Authenticator, 1Password, Aegis, or any TOTP app.
              <img
                src={setup.data.qr_png}
                alt="Two-factor setup QR code"
                width={240}
                height={240}
                className="mt-3 rounded-xl border border-white/10 bg-white p-2"
              />
            </li>
            <li>
              <span className="font-medium text-mist-100">Can&rsquo;t scan?</span>{' '}
              Enter this key by hand:
              <code className="mt-2 block break-all rounded-lg bg-ink-950/60 px-3 py-2 text-xs text-mist-300">
                {setup.data.secret}
              </code>
            </li>
            <li>
              <span className="font-medium text-mist-100">2. Confirm</span> with the
              code your app now shows.
            </li>
          </ol>

          <CodeField value={code} onChange={setCode} />

          <div className="flex flex-col gap-2 sm:flex-row">
            <button
              type="submit"
              className="btn-primary"
              disabled={activate.isPending || code.length !== 6}
            >
              {activate.isPending ? 'Confirming…' : 'Confirm and enable'}
            </button>
            <button
              type="button"
              className="btn-ghost"
              onClick={() => {
                setup.reset()
                setCode('')
              }}
            >
              Cancel
            </button>
          </div>
          {activate.isError && <ErrorNote>{activate.error.message}</ErrorNote>}
        </form>
      )}
    </Section>
  )
}

function RecoveryCodeList({ codes, onDismiss }: { codes: string[]; onDismiss: () => void }) {
  return (
    <>
      <p className="text-sm text-mist-300">
        These are shown <span className="font-medium text-mist-100">once</span>.
        Only hashes are stored, so they cannot be retrieved later. Each works a
        single time, and they are the only way back in if you lose your phone.
      </p>

      <ul className="mt-4 grid grid-cols-2 gap-2 sm:grid-cols-3">
        {codes.map((code) => (
          <li
            key={code}
            className="rounded-lg bg-ink-950/60 px-3 py-2 text-center font-mono text-sm text-mist-200"
          >
            {code}
          </li>
        ))}
      </ul>

      <div className="mt-5 flex flex-col gap-2 sm:flex-row">
        <button
          type="button"
          className="btn-ghost"
          onClick={() => navigator.clipboard?.writeText(codes.join('\n'))}
        >
          Copy all
        </button>
        <button type="button" className="btn-primary" onClick={onDismiss}>
          I have saved these
        </button>
      </div>
    </>
  )
}

// ---------------------------------------------------------------------------
// Password
// ---------------------------------------------------------------------------

function PasswordSection() {
  const qc = useQueryClient()
  const status = useQuery({ queryKey: ['mfa'], queryFn: api.mfaStatus })
  const change = useChangePassword()

  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [confirm, setConfirm] = useState('')
  const [code, setCode] = useState('')
  const [done, setDone] = useState(false)

  // Checked here purely to save a round trip; the server is the authority on
  // every other rule, and its message is what gets shown when it disagrees.
  const mismatch = confirm !== '' && next !== confirm

  function onSubmit(e: FormEvent) {
    e.preventDefault()
    if (mismatch) return

    change.mutate(
      { current_password: current, new_password: next, code: code || undefined },
      {
        onSuccess: () => {
          setCurrent('')
          setNext('')
          setConfirm('')
          setCode('')
          setDone(true)
          qc.invalidateQueries({ queryKey: ['sessions'] })
        },
      },
    )
  }

  return (
    <Section
      title="Password"
      description="At least 12 characters. Length matters far more than punctuation."
    >
      <form onSubmit={onSubmit} className="space-y-3">
        <PasswordField
          value={current}
          onChange={setCurrent}
          label="Current password"
          autoComplete="current-password"
        />
        <PasswordField
          value={next}
          onChange={setNext}
          label="New password"
          autoComplete="new-password"
        />
        <PasswordField
          value={confirm}
          onChange={setConfirm}
          label="Confirm new password"
          autoComplete="new-password"
        />

        {status.data?.enabled && <CodeField value={code} onChange={setCode} />}

        {mismatch && <ErrorNote>Those two passwords do not match.</ErrorNote>}
        {change.isError && <ErrorNote>{change.error.message}</ErrorNote>}
        {done && (
          <p className="rounded-xl border border-rune-400/25 bg-rune-400/5 px-4 py-2.5 text-sm text-rune-300">
            Password changed. Every other signed-in device has been signed out.
          </p>
        )}

        <p className="text-sm text-mist-500">
          Changing this signs out every other device. That is the point — if the
          old password may be known to someone else, leaving their session alive
          would achieve nothing.
        </p>

        <button
          type="submit"
          className="btn-primary"
          disabled={change.isPending || !current || !next || mismatch}
        >
          {change.isPending ? 'Changing…' : 'Change password'}
        </button>
      </form>
    </Section>
  )
}

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

function SessionsSection() {
  const qc = useQueryClient()
  const sessions = useQuery({ queryKey: ['sessions'], queryFn: api.sessions })

  const revoke = useMutation({
    mutationFn: api.revokeSession,
    onSuccess: () => qc.invalidateQueries({ queryKey: ['sessions'] }),
  })

  const revokeOthers = useMutation({
    mutationFn: api.revokeOtherSessions,
    onSuccess: () => qc.invalidateQueries({ queryKey: ['sessions'] }),
  })

  const others = (sessions.data ?? []).filter((s) => !s.is_current).length

  return (
    <Section
      title="Signed-in devices"
      description="Anything here can read your finances. If you do not recognise one, revoke it and change your password."
    >
      {sessions.isPending && <p className="text-sm text-mist-500">Loading…</p>}

      <ul className="divide-y divide-white/5">
        {sessions.data?.map((session) => (
          <li key={session.id} className="flex flex-wrap items-center gap-x-4 gap-y-1 py-3">
            <div className="min-w-0 flex-1">
              <p className="truncate text-sm font-medium">
                {describeDevice(session.user_agent)}
                {session.is_current && (
                  <span className="ml-2 rounded-full bg-rune-400/15 px-2 py-0.5 text-xs text-rune-300">
                    this device
                  </span>
                )}
              </p>
              <p className="truncate text-xs text-mist-500">
                {session.client_ip ?? 'unknown address'} · last used{' '}
                {new Date(session.last_used_at).toLocaleString()}
              </p>
            </div>
            <button
              className="shrink-0 text-xs text-ember-400 hover:underline disabled:opacity-50"
              disabled={revoke.isPending}
              onClick={() => revoke.mutate(session.id)}
            >
              {session.is_current ? 'Sign out' : 'Revoke'}
            </button>
          </li>
        ))}
      </ul>

      {revoke.isError && <ErrorNote>{revoke.error.message}</ErrorNote>}

      {others > 0 && (
        <button
          type="button"
          className="btn-ghost mt-4"
          disabled={revokeOthers.isPending}
          onClick={() => revokeOthers.mutate()}
        >
          {revokeOthers.isPending
            ? 'Signing out…'
            : `Sign out ${others} other ${others === 1 ? 'device' : 'devices'}`}
        </button>
      )}
    </Section>
  )
}

/**
 * A rough, readable device label from a User-Agent.
 *
 * Deliberately coarse: the point is to help someone recognise their own
 * devices, not to fingerprint them. The raw string is attacker-controlled, so
 * it is only ever rendered as text — React escapes it.
 */
function describeDevice(userAgent: string | null): string {
  if (!userAgent) return 'Unknown device'

  const os = /iPhone|iPad/.test(userAgent)
    ? 'iOS'
    : /Android/.test(userAgent)
      ? 'Android'
      : /Macintosh/.test(userAgent)
        ? 'macOS'
        : /Windows/.test(userAgent)
          ? 'Windows'
          : /Linux/.test(userAgent)
            ? 'Linux'
            : 'Unknown OS'

  // Order matters: Edge and Chrome both claim Safari, and Edge claims Chrome.
  const browser = /Edg\//.test(userAgent)
    ? 'Edge'
    : /Firefox\//.test(userAgent)
      ? 'Firefox'
      : /Chrome\//.test(userAgent)
        ? 'Chrome'
        : /Safari\//.test(userAgent)
          ? 'Safari'
          : 'Unknown browser'

  return `${browser} on ${os}`
}

// ---------------------------------------------------------------------------
// Recent activity
// ---------------------------------------------------------------------------

const EVENT_LABELS: Record<string, string> = {
  login_succeeded: 'Signed in',
  login_failed: 'Failed sign-in attempt',
  login_locked: 'Sign-in blocked (too many failures)',
  logout: 'Signed out',
  registered: 'Account created',
  mfa_challenged: 'Two-factor code requested',
  mfa_succeeded: 'Two-factor code accepted',
  mfa_failed: 'Two-factor code rejected',
  mfa_enabled: 'Two-factor enabled',
  mfa_disabled: 'Two-factor disabled',
  recovery_code_used: 'Recovery code used',
  recovery_codes_regenerated: 'Recovery codes regenerated',
  password_changed: 'Password changed',
  session_revoked: 'Device signed out',
  invite_created: 'Invite created',
  invite_accepted: 'Invite accepted',
}

/** Events worth making visually obvious when scanning the list. */
const NOTABLE = new Set([
  'login_failed',
  'login_locked',
  'mfa_failed',
  'mfa_disabled',
  'recovery_code_used',
  'password_changed',
])

function ActivitySection() {
  const events = useQuery({ queryKey: ['auth-events'], queryFn: api.authEvents })

  return (
    <Section
      title="Recent activity"
      description="The last 50 security events on your account."
    >
      {events.isPending && <p className="text-sm text-mist-500">Loading…</p>}
      {events.data?.length === 0 && (
        <p className="text-sm text-mist-500">Nothing recorded yet.</p>
      )}

      <ul className="divide-y divide-white/5">
        {events.data?.map((event, i) => (
          <li key={i} className="flex flex-wrap items-baseline gap-x-3 gap-y-1 py-2.5 text-sm">
            <span className={NOTABLE.has(event.event_type) ? 'text-ember-400' : ''}>
              {EVENT_LABELS[event.event_type] ?? event.event_type}
            </span>
            <span className="text-xs text-mist-500">{event.client_ip ?? 'unknown address'}</span>
            <span className="ml-auto text-xs text-mist-500">
              {new Date(event.created_at).toLocaleString()}
            </span>
          </li>
        ))}
      </ul>
    </Section>
  )
}

// ---------------------------------------------------------------------------
// Shared pieces
// ---------------------------------------------------------------------------

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

function PasswordField({
  value,
  onChange,
  label,
  autoComplete = 'current-password',
}: {
  value: string
  onChange: (v: string) => void
  label: string
  autoComplete?: string
}) {
  return (
    <div>
      <label className="label" htmlFor={label}>
        {label}
      </label>
      <input
        id={label}
        type="password"
        className="field"
        autoComplete={autoComplete}
        value={value}
        onChange={(e) => onChange(e.target.value)}
      />
    </div>
  )
}

function CodeField({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  return (
    <div>
      <label className="label" htmlFor="mfa-code">
        Authentication code
      </label>
      <input
        id="mfa-code"
        className="field text-center tracking-[0.3em]"
        autoComplete="one-time-code"
        inputMode="numeric"
        pattern="[0-9]*"
        maxLength={6}
        placeholder="000000"
        value={value}
        onChange={(e) => onChange(e.target.value)}
      />
    </div>
  )
}

function StatusPill({ tone, children }: { tone: 'good' | 'warn'; children: ReactNode }) {
  const styles =
    tone === 'good'
      ? 'border-rune-400/25 bg-rune-400/10 text-rune-300'
      : 'border-ember-400/25 bg-ember-400/10 text-ember-400'
  return (
    <span className={`inline-block rounded-full border px-3 py-1 text-xs ${styles}`}>
      {children}
    </span>
  )
}

function ErrorNote({ children }: { children: ReactNode }) {
  return (
    <p
      role="alert"
      className="rounded-xl border border-ember-400/30 bg-ember-400/10 px-4 py-2.5 text-sm text-ember-400"
    >
      {children}
    </p>
  )
}
