import { useState, type FormEvent, type ReactNode } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ApiError, api } from '../lib/api'
import type { CreatedWebhook, Webhook, WebhookMessage } from '../lib/api'
import { SkeletonRows } from '../components/Skeleton'

/**
 * Outgoing webhooks: wire this household's events to something the app does not
 * ship.
 *
 * Two design points that the markup below only implies:
 *
 *  - The feature is a deployment switch, not a household preference. When
 *    WEBHOOKS_ENABLED is unset every route answers 503, so the section reads the
 *    503 and says so plainly rather than rendering a form whose submit button
 *    would fail. Hiding the section entirely was the alternative, and it is
 *    worse: somebody who came here from the docs deserves to be told which
 *    variable to set, not to conclude the docs are wrong.
 *  - The secret is shown exactly once. It is held in component state until
 *    dismissed and written nowhere else, because the server keeps it sealed and
 *    there would be nothing to retrieve it from — the same contract as a
 *    personal API token.
 */
export function Webhooks() {
  const qc = useQueryClient()
  const hooks = useQuery({ queryKey: ['webhooks'], queryFn: api.webhooks, retry: false })
  const triggers = useQuery({
    queryKey: ['webhook-triggers'],
    queryFn: api.webhookTriggers,
    retry: false,
  })

  // The secret from the most recent create or rotate, and which webhook it
  // belongs to so the panel can name it.
  const [revealed, setRevealed] = useState<{ name: string; secret: string } | null>(null)
  const [editing, setEditing] = useState<Webhook | null>(null)
  const [inspecting, setInspecting] = useState<Webhook | null>(null)

  const create = useMutation({
    mutationFn: api.createWebhook,
    onSuccess: (hook: CreatedWebhook) => {
      setRevealed({ name: hook.name, secret: hook.secret })
      qc.invalidateQueries({ queryKey: ['webhooks'] })
    },
  })

  const update = useMutation({
    mutationFn: (input: { id: string; body: Parameters<typeof api.updateWebhook>[1] }) =>
      api.updateWebhook(input.id, input.body),
    onSuccess: () => {
      setEditing(null)
      qc.invalidateQueries({ queryKey: ['webhooks'] })
    },
  })

  const remove = useMutation({
    mutationFn: api.deleteWebhook,
    onSuccess: () => {
      setInspecting(null)
      qc.invalidateQueries({ queryKey: ['webhooks'] })
    },
  })

  const rotate = useMutation({
    mutationFn: (hook: Webhook) =>
      api.rotateWebhookSecret(hook.id).then((r) => ({ name: hook.name, secret: r.secret })),
    onSuccess: setRevealed,
  })

  const test = useMutation({
    mutationFn: api.testWebhook,
    onSuccess: (_result, id) => qc.invalidateQueries({ queryKey: ['webhook-messages', id] }),
  })

  // A 503 is the instance saying the feature is off, which is a different thing
  // from a failure and gets different copy.
  if (hooks.error instanceof ApiError && hooks.error.status === 503) {
    return (
      <Section title="Webhooks">
        <p className="text-sm text-mist-300">{hooks.error.message}</p>
        <p className="mt-3 text-sm text-mist-500">
          Webhooks send this household's events to a URL you choose. They are off
          by default because Ledgermancy contacts nothing but Plaid and your AI
          provider unless you switch something on, and this is the switch that
          lets it contact anything at all.
        </p>
      </Section>
    )
  }

  if (revealed) {
    return (
      <Section title={`Copy the signing secret for ${revealed.name}`}>
        <p className="text-sm text-mist-300">
          This is shown <span className="font-medium text-mist-100">once</span>.
          Your receiver uses it to check that a delivery really came from here —
          without it, anything that can reach your URL can pretend to be
          Ledgermancy. If you lose it, rotate it and update the receiver.
        </p>

        <code className="mt-4 block break-all rounded-lg bg-ink-950/60 px-3 py-2.5 font-mono text-sm text-mist-200">
          {revealed.secret}
        </code>

        <div className="mt-5 flex flex-col gap-2 sm:flex-row">
          <button
            type="button"
            className="btn-ghost"
            onClick={() => navigator.clipboard?.writeText(revealed.secret)}
          >
            Copy secret
          </button>
          <button type="button" className="btn-primary" onClick={() => setRevealed(null)}>
            I have saved it
          </button>
        </div>
      </Section>
    )
  }

  return (
    <div className="space-y-6">
      <Section
        title="Webhooks"
        description="Send this household's events to something else — Home Assistant, a Discord channel, a script of your own. Every delivery is signed, retried if it fails, and recorded so you can see exactly what was sent."
      >
        {hooks.isPending && <SkeletonRows count={2} />}
        {hooks.isError && !(hooks.error instanceof ApiError && hooks.error.status === 503) && (
          <ErrorNote>{(hooks.error as Error).message}</ErrorNote>
        )}
        {hooks.data?.length === 0 && (
          <p className="text-sm text-mist-500">No webhooks yet.</p>
        )}

        <ul className="divide-y divide-white/5">
          {hooks.data?.map((hook) => (
            <li key={hook.id} className="py-3">
              <div className="flex flex-wrap items-start gap-x-4 gap-y-2">
                <div className="min-w-0 flex-1">
                  <p className="truncate text-sm font-medium">
                    {hook.name}
                    {!hook.active && (
                      <span className="ml-2 rounded-full bg-mist-500/15 px-2 py-0.5 text-xs text-mist-400">
                        paused
                      </span>
                    )}
                  </p>
                  <p className="truncate font-mono text-xs text-mist-500">{hook.url}</p>
                  <p className="mt-1 text-xs text-mist-500">
                    {hook.triggers.map(triggerLabel).join(' · ')}
                  </p>
                </div>
                <div className="flex shrink-0 flex-wrap gap-3 text-xs">
                  <button
                    className="text-mist-300 hover:underline disabled:opacity-50"
                    disabled={test.isPending}
                    onClick={() => test.mutate(hook.id)}
                  >
                    Send test
                  </button>
                  <button
                    className="text-mist-300 hover:underline"
                    onClick={() =>
                      setInspecting((current) => (current?.id === hook.id ? null : hook))
                    }
                  >
                    {inspecting?.id === hook.id ? 'Hide deliveries' : 'Deliveries'}
                  </button>
                  <button
                    className="text-mist-300 hover:underline"
                    onClick={() => setEditing((current) => (current?.id === hook.id ? null : hook))}
                  >
                    Edit
                  </button>
                  <button
                    className="text-mist-300 hover:underline disabled:opacity-50"
                    disabled={rotate.isPending}
                    onClick={() => rotate.mutate(hook)}
                  >
                    Rotate secret
                  </button>
                  <button
                    className="text-ember-400 hover:underline disabled:opacity-50"
                    disabled={remove.isPending}
                    onClick={() => remove.mutate(hook.id)}
                  >
                    Delete
                  </button>
                </div>
              </div>

              {editing?.id === hook.id && (
                <WebhookForm
                  key={hook.id}
                  triggers={triggers.data ?? []}
                  initial={hook}
                  submitLabel={update.isPending ? 'Saving…' : 'Save changes'}
                  error={update.error as Error | null}
                  onSubmit={(body) => update.mutate({ id: hook.id, body })}
                  onCancel={() => setEditing(null)}
                />
              )}

              {inspecting?.id === hook.id && <DeliveryInspector webhook={hook} />}
            </li>
          ))}
        </ul>

        {test.isError && <ErrorNote>{(test.error as Error).message}</ErrorNote>}
        {rotate.isError && <ErrorNote>{(rotate.error as Error).message}</ErrorNote>}
        {remove.isError && <ErrorNote>{(remove.error as Error).message}</ErrorNote>}
      </Section>

      {hooks.isSuccess && (
        <Section
          title="Add a webhook"
          description="Ledgermancy POSTs a JSON body to your URL and signs it with a secret you will be shown once. A private address on your own network is fine — that is what most of these are for."
        >
          <WebhookForm
            triggers={triggers.data ?? []}
            submitLabel={create.isPending ? 'Creating…' : 'Create webhook'}
            error={create.error as Error | null}
            onSubmit={(body) => create.mutate(body)}
          />
        </Section>
      )}
    </div>
  )
}

// ---------------------------------------------------------------------------
// The create/edit form
// ---------------------------------------------------------------------------

/**
 * Human labels for the trigger vocabulary.
 *
 * The trigger STRINGS come from the server so the UI can never offer one the
 * backend does not understand. The labels are here because they are wording, not
 * behaviour — and an unknown trigger falls back to its raw name rather than
 * rendering blank, so a trigger added on the backend is usable before this map
 * catches up.
 */
const TRIGGER_LABELS: Record<string, string> = {
  'insight.created': 'A new insight is raised',
  'alert.fired': 'An alert fires',
  'goal.contribution.recorded': 'A goal contribution is logged',
}

function triggerLabel(trigger: string): string {
  return TRIGGER_LABELS[trigger] ?? trigger
}

function WebhookForm({
  triggers,
  initial,
  submitLabel,
  error,
  onSubmit,
  onCancel,
}: {
  triggers: string[]
  initial?: Webhook
  submitLabel: string
  error: Error | null
  onSubmit: (body: {
    name: string
    url: string
    triggers: string[]
    active: boolean
  }) => void
  onCancel?: () => void
}) {
  const [name, setName] = useState(initial?.name ?? '')
  const [url, setUrl] = useState(initial?.url ?? '')
  const [active, setActive] = useState(initial?.active ?? true)
  const [selected, setSelected] = useState<string[]>(initial?.triggers ?? [])

  const toggle = (trigger: string) =>
    setSelected((current) =>
      current.includes(trigger)
        ? current.filter((t) => t !== trigger)
        : [...current, trigger],
    )

  return (
    <form
      className="mt-4 space-y-3 border-t border-white/5 pt-4"
      onSubmit={(e: FormEvent) => {
        e.preventDefault()
        onSubmit({ name: name.trim(), url: url.trim(), triggers: selected, active })
      }}
    >
      <div>
        <label className="label" htmlFor={`webhook-name-${initial?.id ?? 'new'}`}>
          What is it for?
        </label>
        <input
          id={`webhook-name-${initial?.id ?? 'new'}`}
          className="field"
          maxLength={100}
          placeholder="Home Assistant"
          value={name}
          onChange={(e) => setName(e.target.value)}
        />
      </div>

      <div>
        <label className="label" htmlFor={`webhook-url-${initial?.id ?? 'new'}`}>
          Where should we send it?
        </label>
        <input
          id={`webhook-url-${initial?.id ?? 'new'}`}
          className="field"
          placeholder="http://homeassistant.local:8123/api/webhook/ledgermancy"
          value={url}
          onChange={(e) => setUrl(e.target.value)}
        />
      </div>

      <fieldset>
        <legend className="label">Which events?</legend>
        <div className="space-y-1.5">
          {triggers.map((trigger) => (
            <label key={trigger} className="flex items-start gap-2 text-sm text-mist-300">
              <input
                type="checkbox"
                className="mt-1"
                checked={selected.includes(trigger)}
                onChange={() => toggle(trigger)}
              />
              <span>{triggerLabel(trigger)}</span>
            </label>
          ))}
        </div>
      </fieldset>

      <label className="flex items-start gap-2 text-sm text-mist-300">
        <input
          type="checkbox"
          className="mt-1"
          checked={active}
          onChange={(e) => setActive(e.target.checked)}
        />
        <span>
          Active. Pausing stops deliveries without losing this webhook or its
          history — the right thing to do while you fix a receiver.
        </span>
      </label>

      <div className="flex flex-wrap gap-2">
        <button
          type="submit"
          className="btn-primary"
          disabled={!name.trim() || !url.trim() || selected.length === 0}
        >
          {submitLabel}
        </button>
        {onCancel && (
          <button type="button" className="btn-ghost" onClick={onCancel}>
            Cancel
          </button>
        )}
      </div>
      {error && <ErrorNote>{error.message}</ErrorNote>}
    </form>
  )
}

// ---------------------------------------------------------------------------
// The delivery inspector
// ---------------------------------------------------------------------------

const STATUS_STYLES: Record<WebhookMessage['status'], string> = {
  // Amber rather than red: pending means "not yet", which on a healthy instance
  // is a state a message occupies for a second or two.
  pending: 'bg-rune-400/15 text-rune-300',
  sent: 'bg-verdant-400/15 text-verdant-400',
  failed: 'bg-ember-400/15 text-ember-400',
}

/**
 * The answer to "why didn't my webhook fire?".
 *
 * Every message, its status, and — on demand — every HTTP request made for it
 * with what came back. This exists because the alternative support experience is
 * asking somebody to read a server log they may not have access to, for a
 * request that may never have been made.
 *
 * It polls while anything is still pending, then stops: deliveries resolve in
 * seconds, and a panel that kept polling a settled list forever would be a
 * background request every few seconds for as long as the tab is open.
 */
function DeliveryInspector({ webhook }: { webhook: Webhook }) {
  const [openMessage, setOpenMessage] = useState<string | null>(null)

  const messages = useQuery({
    queryKey: ['webhook-messages', webhook.id],
    queryFn: () => api.webhookMessages(webhook.id),
    refetchInterval: (query) =>
      query.state.data?.some((m) => m.status === 'pending') ? 2000 : false,
  })

  return (
    <div className="mt-4 rounded-xl border border-white/5 bg-ink-950/40 p-4">
      {messages.isPending && <SkeletonRows count={2} />}
      {messages.isError && <ErrorNote>{(messages.error as Error).message}</ErrorNote>}
      {messages.data?.length === 0 && (
        <p className="text-sm text-mist-500">
          Nothing has been sent to this webhook yet. Use “Send test” to check
          your receiver.
        </p>
      )}

      <ul className="divide-y divide-white/5">
        {messages.data?.map((message) => (
          <li key={message.id} className="py-2.5">
            <button
              className="flex w-full flex-wrap items-center gap-x-3 gap-y-1 text-left"
              onClick={() =>
                setOpenMessage((current) => (current === message.id ? null : message.id))
              }
            >
              <span
                className={`rounded-full px-2 py-0.5 text-xs ${STATUS_STYLES[message.status]}`}
              >
                {message.status}
              </span>
              <span className="min-w-0 flex-1 truncate text-sm">
                {triggerLabel(message.trigger)}
              </span>
              <span className="text-xs text-mist-500">
                {new Date(message.created_at).toLocaleString()} ·{' '}
                {message.attempts === 1 ? '1 attempt' : `${message.attempts} attempts`}
              </span>
            </button>

            {message.last_error && (
              <p className="mt-1 text-xs text-ember-400">{message.last_error}</p>
            )}

            {openMessage === message.id && (
              <AttemptList webhookId={webhook.id} message={message} />
            )}
          </li>
        ))}
      </ul>
    </div>
  )
}

function AttemptList({
  webhookId,
  message,
}: {
  webhookId: string
  message: WebhookMessage
}) {
  const attempts = useQuery({
    queryKey: ['webhook-attempts', webhookId, message.id],
    queryFn: () => api.webhookAttempts(webhookId, message.id),
  })

  return (
    <div className="mt-3 space-y-3">
      <div>
        <p className="label">What we sent</p>
        <pre className="mt-1 max-h-48 overflow-auto rounded-lg bg-ink-950/60 p-3 font-mono text-xs text-mist-300">
          {JSON.stringify(message.payload, null, 2)}
        </pre>
      </div>

      {attempts.isPending && <SkeletonRows count={1} />}
      {attempts.data?.length === 0 && (
        <p className="text-xs text-mist-500">No request has been made yet.</p>
      )}

      {attempts.data?.map((attempt) => (
        <div key={attempt.id} className="rounded-lg border border-white/5 p-3">
          <p className="text-xs text-mist-400">
            Attempt {attempt.attempt} · {new Date(attempt.created_at).toLocaleString()} ·{' '}
            {attempt.duration_ms} ms
          </p>
          {attempt.response_status !== null ? (
            <p className="mt-1 text-xs text-mist-300">
              Responded {attempt.response_status}
              {attempt.response_body ? ':' : ''}
            </p>
          ) : (
            <p className="mt-1 text-xs text-ember-400">
              No response — {attempt.error}
            </p>
          )}
          {attempt.response_body && (
            <pre className="mt-1 max-h-32 overflow-auto rounded bg-ink-950/60 p-2 font-mono text-xs text-mist-400">
              {attempt.response_body}
            </pre>
          )}
        </div>
      ))}
    </div>
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

function ErrorNote({ children }: { children: ReactNode }) {
  return (
    <p
      role="alert"
      className="mt-3 rounded-xl border border-ember-400/30 bg-ember-400/10 px-4 py-2.5 text-sm text-ember-400"
    >
      {children}
    </p>
  )
}
