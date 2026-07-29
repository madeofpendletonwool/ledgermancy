import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '../lib/api'
import type { Continuity as ContinuityData, ContinuityHealth, ContinuityKind, ContinuityRun } from '../lib/api'

/**
 * The continuity panel: whether this deployment could actually be restored.
 *
 * The copy here is deliberately blunt, and every consequence sentence is
 * written server-side (api/continuity_handlers.go, panelHeadline) so the
 * wording lives next to the logic that decides it is true. Operators
 * under-invest in backups because nothing ever states the stakes plainly — a
 * status word like "stale" is something to acknowledge and scroll past, whereas
 * "you would be restoring from a backup nobody has tested" is something to act
 * on. That is the entire design of this page.
 */
export function Continuity() {
  const status = useQuery({ queryKey: ['continuity'], queryFn: api.continuity })

  return (
    <div className="space-y-8">
      <div>
        <h1 className="text-2xl font-semibold">Continuity</h1>
        <p className="mt-1 text-mist-300">
          Nobody else has a copy of this. There is no support line, no account
          recovery, and no second chance — which is the point of self-hosting
          and also the whole problem.
        </p>
      </div>

      {status.isPending && <p className="text-sm text-mist-500">Loading…</p>}
      {status.isError && (
        <p className="text-sm text-ember-400">
          Could not read backup status: {(status.error as Error).message}
        </p>
      )}

      {status.data && (
        <>
          <StatusSection data={status.data} />
          <KeySection data={status.data} />
          <CoverageSection data={status.data} />
          <SettingsSection data={status.data} />
        </>
      )}
    </div>
  )
}

// ---------------------------------------------------------------------------
// Status
// ---------------------------------------------------------------------------

const KIND_LABELS: Record<ContinuityKind, string> = {
  restore_test: 'Automatic restore check',
  db_dump: 'Database backup',
  documents_archive: 'Document contents',
  export: 'Portable export',
  // Not "off-host copy": BACKUP_DIR may already be a mount from another
  // machine, and the app cannot tell. All it knows is how many destinations
  // are configured, so that is what the label claims.
  mirror_push: 'Second backup location',
  key_ack: 'Encryption key',
}

/**
 * `off` is grey, not red: a thing switched off deliberately is a decision, and
 * painting decisions as failures teaches operators to ignore the colour. `never`
 * is red, because "configured and has never worked" is a failure wearing the
 * costume of a fresh install.
 */
const HEALTH_STYLES: Record<ContinuityHealth, { dot: string; text: string }> = {
  good: { dot: 'bg-verdant-400', text: 'text-mist-300' },
  warn: { dot: 'bg-rune-400', text: 'text-rune-300' },
  bad: { dot: 'bg-ember-400', text: 'text-ember-400' },
  never: { dot: 'bg-ember-400', text: 'text-ember-400' },
  off: { dot: 'bg-mist-500', text: 'text-mist-500' },
}

function StatusSection({ data }: { data: ContinuityData }) {
  const qc = useQueryClient()
  const run = useMutation({
    mutationFn: api.runContinuityJob,
    onSuccess: () => qc.invalidateQueries({ queryKey: ['continuity'] }),
  })

  return (
    <section className="glass p-6">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <h2 className="text-lg font-medium">Recovery posture</h2>
          {data.enabled ? (
            <p className="mt-1 text-sm text-mist-300">
              All of this runs on its own. The restore check works against a
              temporary copy and never touches your live database.
            </p>
          ) : (
            <p className="mt-1 text-sm text-ember-400">
              Backups are switched off. Nothing on this page is running.
            </p>
          )}
        </div>
        {data.enabled && (
          <div className="flex gap-2">
            <button
              className="btn-ghost"
              disabled={run.isPending}
              onClick={() => run.mutate('backup')}
            >
              Back up now
            </button>
            <button
              className="btn-ghost"
              disabled={run.isPending}
              onClick={() => run.mutate('restore_test')}
              title="Restores the latest backup into a temporary database, checks it, and drops it. Your live database is not touched."
            >
              Check a restore now
            </button>
            <a className="btn-ghost" href="/api/admin/continuity/export">
              Download export
            </a>
          </div>
        )}
      </div>

      {run.isError && (
        <p className="mt-3 text-sm text-ember-400">{(run.error as Error).message}</p>
      )}
      {run.isSuccess && (
        <p className="mt-3 text-sm text-mist-300">
          Queued. It will appear below once it finishes.
        </p>
      )}

      <ul className="mt-6 space-y-5">
        {data.runs.map((r) => (
          <RunRow key={r.kind} run={r} />
        ))}
      </ul>
    </section>
  )
}

function RunRow({ run }: { run: ContinuityRun }) {
  const style = HEALTH_STYLES[run.health]

  return (
    <li className="border-t border-white/5 pt-5 first:border-0 first:pt-0">
      <div className="flex items-baseline gap-3">
        <span className={`mt-1.5 h-2 w-2 shrink-0 rounded-full ${style.dot}`} />
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-baseline justify-between gap-x-4">
            <span className="font-medium text-mist-100">{KIND_LABELS[run.kind]}</span>
            {run.age && (
              <span className="text-xs text-mist-500">
                {run.age}
                {run.status === 'failure' && ' · failed'}
              </span>
            )}
          </div>

          <p className={`mt-1 text-sm ${style.text}`}>{run.headline}</p>

          {(run.detail || run.artifact_path) && (
            <details className="mt-2">
              <summary className="cursor-pointer text-xs text-mist-500 hover:text-mist-300">
                Details
              </summary>
              {run.artifact_path && (
                <p className="mt-2 break-all font-mono text-xs text-mist-500">
                  {run.artifact_path}
                  {run.size_bytes ? ` · ${formatBytes(run.size_bytes)}` : ''}
                </p>
              )}
              {run.detail && (
                <pre className="mt-2 whitespace-pre-wrap break-words rounded-lg bg-ink-950/60 px-3 py-2 text-xs text-mist-300">
                  {run.detail}
                </pre>
              )}
            </details>
          )}
        </div>
      </div>
    </li>
  )
}

// ---------------------------------------------------------------------------
// The key
// ---------------------------------------------------------------------------

/**
 * The one thing on this page the app cannot do anything about.
 *
 * ENCRYPTION_KEY lives only in the operator's .env and wherever they copied it.
 * A perfect backup restored without it yields a database that cannot decrypt
 * its own Plaid tokens or open a single document — intact and useless. So the
 * app asks, records the answer and its date, and says plainly that it cannot
 * check.
 */
function KeySection({ data }: { data: ContinuityData }) {
  const qc = useQueryClient()
  const ack = useMutation({
    mutationFn: api.acknowledgeKeyBackup,
    onSuccess: () => qc.invalidateQueries({ queryKey: ['continuity'] }),
  })

  const key = data.runs.find((r) => r.kind === 'key_ack')
  const confirmed = key?.health !== 'never'

  return (
    <section className="glass p-6">
      <h2 className="text-lg font-medium">Your encryption key</h2>
      <p className="mt-3 text-sm text-mist-300">
        <code className="rounded bg-ink-950/60 px-1.5 py-0.5 text-xs">ENCRYPTION_KEY</code>{' '}
        in your <code className="rounded bg-ink-950/60 px-1.5 py-0.5 text-xs">.env</code>{' '}
        encrypts every Plaid access token and every document in the vault. It is
        not in the database and not in any backup this app takes — deliberately,
        because a key stored beside the data it protects is not protecting it.
      </p>
      <p className="mt-3 text-sm text-mist-300">
        Store it in a password manager, and keep one copy somewhere that does not
        depend on this machine being alive. If you lose it, nobody can recover
        it: not you, not us. There is no us.
      </p>

      {!confirmed && (
        <div className="mt-5 rounded-xl border border-ember-400/20 bg-ember-400/5 px-4 py-3">
          <p className="text-sm text-ember-400">
            You have not confirmed your key is stored somewhere safe.
          </p>
          <button
            className="btn-primary mt-3"
            disabled={ack.isPending}
            onClick={() => ack.mutate()}
          >
            I have stored my ENCRYPTION_KEY safely
          </button>
        </div>
      )}

      {confirmed && (
        <div className="mt-5 flex flex-wrap items-center gap-3">
          <p className="text-sm text-mist-300">Confirmed {key?.age}.</p>
          <button
            className="btn-ghost"
            disabled={ack.isPending}
            onClick={() => ack.mutate()}
          >
            I checked again
          </button>
        </div>
      )}
    </section>
  )
}

// ---------------------------------------------------------------------------
// Coverage
// ---------------------------------------------------------------------------

/**
 * What is and is not in a backup, from the coverage registry the backend
 * enforces at build time. Shown because "is my new feature backed up" is a
 * question operators cannot otherwise answer, and because a number that moves
 * when a release adds tables is a quiet reassurance that the guard is alive.
 */
function CoverageSection({ data }: { data: ContinuityData }) {
  const c = data.coverage
  return (
    <section className="glass p-6">
      <h2 className="text-lg font-medium">What is covered</h2>
      <p className="mt-1 text-sm text-mist-300">
        Every table in the schema is classified, and the build fails if one is
        not — so a feature cannot ship with data nobody backs up.
      </p>

      <dl className="mt-5 grid grid-cols-2 gap-4 sm:grid-cols-4">
        <Stat label="Your data" value={c.in_export} hint="backup + portable export" />
        <Stat label="App internals" value={c.dump_only} hint="backup only" />
        <Stat label="Recomputed" value={c.derived} hint="rebuilt by a job" />
        <Stat label="Discarded" value={c.ephemeral} hint="sessions, queue state" />
      </dl>

      {c.blob_stores.length > 0 && (
        <div className="mt-6 border-t border-white/5 pt-5">
          <p className="text-sm font-medium text-mist-100">Stored outside the database</p>
          <ul className="mt-2 space-y-2">
            {c.blob_stores.map((s) => (
              <li key={s.name} className="text-sm text-mist-300">
                <span className="font-medium text-mist-100">{s.name}</span> — {s.why}
              </li>
            ))}
          </ul>
        </div>
      )}
    </section>
  )
}

function Stat({ label, value, hint }: { label: string; value: number; hint: string }) {
  return (
    <div>
      <dt className="text-xs text-mist-500">{label}</dt>
      <dd className="mt-1 text-2xl font-semibold text-rune-300">{value}</dd>
      <p className="text-xs text-mist-500">{hint}</p>
    </div>
  )
}

// ---------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------

function SettingsSection({ data }: { data: ContinuityData }) {
  const s = data.settings
  return (
    <section className="glass p-6">
      <h2 className="text-lg font-medium">Configuration</h2>
      <p className="mt-1 text-sm text-mist-300">
        Set in <code className="rounded bg-ink-950/60 px-1.5 py-0.5 text-xs">.env</code>,
        not here — these describe the host, so changing them is a deploy.
      </p>

      <dl className="mt-5 space-y-3 text-sm">
        <Row label="Backup directory" value={s.dir} mono />
        <Row
          label="Second location"
          value={s.mirror_dir || 'not configured'}
          mono={!!s.mirror_dir}
          warn={!s.mirror_dir}
        />
        <Row label="Backup every" value={s.interval} />
        <Row label="Restore test every" value={s.restore_test_interval} />
        <Row
          label="Document contents"
          value={s.include_documents ? 'archived with each backup' : 'NOT backed up'}
          warn={!s.include_documents}
        />
        <Row
          label="Retention"
          value={`${s.keep_daily} daily · ${s.keep_weekly} weekly · ${s.keep_monthly} monthly`}
        />
      </dl>
    </section>
  )
}

function Row({
  label,
  value,
  mono,
  warn,
}: {
  label: string
  value: string
  mono?: boolean
  warn?: boolean
}) {
  return (
    <div className="flex flex-wrap justify-between gap-x-4 border-t border-white/5 pt-3 first:border-0 first:pt-0">
      <dt className="text-mist-500">{label}</dt>
      <dd
        className={`${mono ? 'font-mono text-xs' : ''} ${
          warn ? 'text-rune-300' : 'text-mist-200'
        }`}
      >
        {value}
      </dd>
    </div>
  )
}

function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`
  const units = ['KB', 'MB', 'GB', 'TB']
  let value = n / 1024
  let i = 0
  while (value >= 1024 && i < units.length - 1) {
    value /= 1024
    i++
  }
  return `${value.toFixed(1)} ${units[i]}`
}
