import { useQuery } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import { api } from '../lib/api'
import type { JobFailure, StatusHealth, SystemStatus as SystemStatusData } from '../lib/api'
import { SkeletonCard } from '../components/Skeleton'

/**
 * What this instance is doing right now.
 *
 * The sibling of the Continuity tab: that one answers "could I recover from
 * this", this one answers "is it working at all". Same five-value health
 * vocabulary and the same rule about where the words come from — every headline
 * is written server-side (api/status_handlers.go) next to the logic that
 * decides it is true, and rendered here verbatim.
 *
 * Polled rather than streamed. One household, one browser tab, and a five
 * second refresh is indistinguishable from live for a queue whose jobs run on
 * minute-to-hour cadences — a websocket would be machinery to maintain in
 * exchange for nothing a reader could perceive.
 */

const POLL_MS = 5000

const HEALTH_STYLES: Record<StatusHealth, { dot: string; text: string }> = {
  good: { dot: 'bg-verdant-400', text: 'text-mist-300' },
  warn: { dot: 'bg-rune-400', text: 'text-rune-300' },
  bad: { dot: 'bg-ember-400', text: 'text-ember-400' },
  never: { dot: 'bg-ember-400', text: 'text-ember-400' },
  off: { dot: 'bg-mist-500', text: 'text-mist-500' },
}

/** Queue states, in lifecycle order, with what each one means to a reader. */
const STATE_LABELS: { key: string; label: string; hint: string }[] = [
  { key: 'available', label: 'Queued', hint: 'waiting for a worker' },
  { key: 'running', label: 'Running', hint: 'being worked now' },
  { key: 'scheduled', label: 'Scheduled', hint: 'due later' },
  { key: 'retryable', label: 'Retrying', hint: 'failed, will try again' },
  { key: 'pending', label: 'Pending', hint: 'parked' },
  { key: 'discarded', label: 'Given up', hint: 'out of retries' },
]

export function SystemStatus() {
  const status = useQuery({
    queryKey: ['system-status'],
    queryFn: api.systemStatus,
    refetchInterval: POLL_MS,
  })

  return (
    <div className="space-y-8">
      <div>
        <h1 className="text-2xl font-semibold">System</h1>
        <p className="mt-1 text-mist-300">
          The background work this instance runs on its own — what is queued,
          what is failing, and when each connection last brought anything in.
        </p>
      </div>

      {status.isPending && (
        <div className="space-y-4">
          <SkeletonCard />
          <SkeletonCard />
        </div>
      )}
      {status.isError && (
        <p className="text-sm text-ember-400">
          Could not read system status: {(status.error as Error).message}
        </p>
      )}

      {status.data && (
        <>
          <JobsSection data={status.data} />
          <SyncSection data={status.data} />
          <BackupSection data={status.data} />
        </>
      )}
    </div>
  )
}

// ---------------------------------------------------------------------------
// Jobs
// ---------------------------------------------------------------------------

function JobsSection({ data }: { data: SystemStatusData }) {
  const jobs = data.jobs

  return (
    <section className="glass p-6">
      <SectionHeading
        title="Background jobs"
        health={jobs.health}
        headline={jobs.headline}
      />

      {/*
        The worker banner is separate from the headline because it is the one
        state where every number below is misleading: a queue of zero is
        excellent when a worker is draining it and meaningless when nothing is
        reading it at all.
      */}
      {!jobs.worker_alive && (
        <div className="mt-5 rounded-xl border border-ember-400/20 bg-ember-400/5 px-4 py-3">
          <p className="text-sm text-ember-400">
            No worker process has checked in. Start the{' '}
            <code className="rounded bg-ink-950/60 px-1.5 py-0.5 text-xs">worker</code>{' '}
            container — the counts below will not move until you do.
          </p>
        </div>
      )}

      <dl className="mt-6 grid grid-cols-2 gap-4 sm:grid-cols-3 lg:grid-cols-6">
        {STATE_LABELS.map((s) => (
          <StateTile
            key={s.key}
            label={s.label}
            value={jobs.counts[s.key] ?? 0}
            hint={s.hint}
            alarming={s.key === 'discarded' && (jobs.counts[s.key] ?? 0) > 0}
          />
        ))}
      </dl>

      {jobs.waiting_age && (
        <p className="mt-4 text-xs text-mist-500">
          Oldest queued job has been waiting {jobs.waiting_age}.
        </p>
      )}

      {jobs.running.length > 0 && (
        <div className="mt-6 border-t border-white/5 pt-5">
          <p className="text-sm font-medium text-mist-100">Running now</p>
          <ul className="mt-3 space-y-2">
            {jobs.running.map((job, i) => (
              <li
                key={`${job.kind}-${i}`}
                className="flex flex-wrap items-baseline justify-between gap-x-4 text-sm"
              >
                <span className="flex items-center gap-2 text-mist-200">
                  <span className="h-1.5 w-1.5 animate-pulse rounded-full bg-verdant-400" />
                  <code className="font-mono text-xs">{job.kind}</code>
                </span>
                <span className="text-xs text-mist-500">
                  {job.attempt > 1 && `attempt ${job.attempt} of ${job.max_attempts} · `}
                  started {job.age}
                </span>
              </li>
            ))}
          </ul>
        </div>
      )}

      {jobs.failures.length > 0 && (
        <div className="mt-6 border-t border-white/5 pt-5">
          <p className="text-sm font-medium text-mist-100">Failing</p>
          <ul className="mt-3 space-y-4">
            {jobs.failures.map((f) => (
              <FailureRow key={`${f.kind}-${f.state}`} failure={f} />
            ))}
          </ul>
        </div>
      )}
    </section>
  )
}

function FailureRow({ failure }: { failure: JobFailure }) {
  // `discarded` is red and `retryable` amber, because the difference between
  // them is whether a human has to do something.
  const gaveUp = failure.state === 'discarded'

  return (
    <li>
      <div className="flex flex-wrap items-baseline justify-between gap-x-4">
        <span className="flex items-center gap-2">
          <span
            className={`h-2 w-2 shrink-0 rounded-full ${
              gaveUp ? 'bg-ember-400' : 'bg-rune-400'
            }`}
          />
          <code className="font-mono text-xs text-mist-200">{failure.kind}</code>
          <span className={`text-xs ${gaveUp ? 'text-ember-400' : 'text-rune-300'}`}>
            {failure.count} {gaveUp ? 'gave up' : 'retrying'}
          </span>
        </span>
        {failure.age && <span className="text-xs text-mist-500">{failure.age}</span>}
      </div>
      {failure.last_error && (
        <pre className="mt-2 whitespace-pre-wrap break-words rounded-lg bg-ink-950/60 px-3 py-2 text-xs text-mist-300">
          {failure.last_error}
        </pre>
      )}
    </li>
  )
}

function StateTile({
  label,
  value,
  hint,
  alarming,
}: {
  label: string
  value: number
  hint: string
  alarming?: boolean
}) {
  return (
    <div>
      <dt className="text-xs text-mist-500">{label}</dt>
      <dd
        className={`mt-1 text-2xl font-semibold ${
          alarming ? 'text-ember-400' : value > 0 ? 'text-rune-300' : 'text-mist-500'
        }`}
      >
        {value}
      </dd>
      <p className="text-xs text-mist-500">{hint}</p>
    </div>
  )
}

// ---------------------------------------------------------------------------
// Sync
// ---------------------------------------------------------------------------

function SyncSection({ data }: { data: SystemStatusData }) {
  const sync = data.sync

  return (
    <section className="glass p-6">
      <SectionHeading
        title="Institution syncs"
        health={sync.health}
        headline={sync.headline}
      />

      {sync.items.length > 0 && (
        <ul className="mt-6 space-y-4">
          {sync.items.map((item) => {
            const style = HEALTH_STYLES[item.health]
            return (
              <li
                key={item.id}
                className="border-t border-white/5 pt-4 first:border-0 first:pt-0"
              >
                <div className="flex items-baseline gap-3">
                  <span
                    className={`mt-1.5 h-2 w-2 shrink-0 rounded-full ${style.dot}`}
                  />
                  <div className="min-w-0 flex-1">
                    <div className="flex flex-wrap items-baseline justify-between gap-x-4">
                      <span className="font-medium text-mist-100">
                        {item.institution}
                      </span>
                      <span className="text-xs text-mist-500">
                        {item.age ? `synced ${item.age}` : 'never synced'}
                      </span>
                    </div>
                    {/*
                      The error code is Plaid's, verbatim. It is the string the
                      operator will search for, so translating it into friendlier
                      words here would cost them the one useful handle they have.
                    */}
                    {item.status !== 'active' && (
                      <p className={`mt-1 text-sm ${style.text}`}>
                        {item.status === 'login_required'
                          ? 'Needs reconnecting — the institution has invalidated the login.'
                          : `Connection is ${item.status}.`}
                        {item.error_code && (
                          <code className="ml-1 rounded bg-ink-950/60 px-1.5 py-0.5 font-mono text-xs">
                            {item.error_code}
                          </code>
                        )}
                      </p>
                    )}
                    {item.status === 'active' && !item.backfill_complete && (
                      <p className="mt-1 text-sm text-mist-500">
                        Still backfilling history — older transactions are on
                        their way.
                      </p>
                    )}
                  </div>
                </div>
              </li>
            )
          })}
        </ul>
      )}
    </section>
  )
}

// ---------------------------------------------------------------------------
// Backup
// ---------------------------------------------------------------------------

/**
 * One line, then a door. The Continuity tab is the page that explains backups
 * properly; repeating it here would give the operator two versions of the same
 * state to reconcile, and the shorter one would win for the wrong reason.
 */
function BackupSection({ data }: { data: SystemStatusData }) {
  const backup = data.backup

  return (
    <section className="glass p-6">
      <SectionHeading
        title="Backups"
        health={backup.health}
        headline={backup.headline}
      />
      <p className="mt-4 text-sm text-mist-500">
        <Link className="text-arcane-300 hover:text-arcane-200" to="/settings?tab=continuity">
          Continuity
        </Link>{' '}
        has the full picture: restore checks, retention, and where the copies
        live.
      </p>
    </section>
  )
}

// ---------------------------------------------------------------------------
// Shared
// ---------------------------------------------------------------------------

function SectionHeading({
  title,
  health,
  headline,
}: {
  title: string
  health: StatusHealth
  headline: string
}) {
  const style = HEALTH_STYLES[health]
  return (
    <div className="flex items-baseline gap-3">
      <span className={`mt-1.5 h-2 w-2 shrink-0 rounded-full ${style.dot}`} />
      <div className="min-w-0 flex-1">
        <h2 className="text-lg font-medium">{title}</h2>
        <p className={`mt-1 text-sm ${style.text}`}>{headline}</p>
      </div>
    </div>
  )
}
