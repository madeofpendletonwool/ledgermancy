import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, type FinancialPlan, type PlanSectionKind, type Person } from '../lib/api'
import { SkeletonRows } from '../components/Skeleton'

/**
 * The Plan — the household's authored intent (MAD-258).
 *
 * Every other page shows where the household IS, computed. This one holds what
 * the household MEANS, written down: the strategy prose, per-person notes, an
 * append-only decisions log, and a review stamp. Three rules from the design,
 * visible in the UI: sections link to live values rather than restating them;
 * confirmed decisions are never edited, only superseded by a newer one; and
 * the review stamp is how a plan says "still current" — the plan_stale
 * reminder nudges when it goes quiet.
 *
 * Everything here is deterministic storage; AI's only role anywhere in the
 * feature is that the advisor chat can DRAFT a decision, which lands here as a
 * proposal for confirmation — never a confirmed write.
 */

const SECTION_META: Record<
  Exclude<PlanSectionKind, 'person'>,
  { label: string; blurb: string }
> = {
  strategy: {
    label: 'Strategy & priorities',
    blurb: 'What you are doing and why, in your words. The advisor reads this before it answers anything.',
  },
  income: {
    label: 'Income & employment',
    blurb: 'Jobs, side income, expected changes — the context the bank feed cannot carry.',
  },
  estate: {
    label: 'Estate & insurance',
    blurb: 'Wills, policies, beneficiaries. Link the paperwork itself in the Documents vault; keep the decisions here.',
  },
  notes: {
    label: 'Notes',
    blurb: 'Everything no section anticipated.',
  },
}

const HOUSEHOLD_KINDS = Object.keys(SECTION_META) as Exclude<PlanSectionKind, 'person'>[]

function todayISO(): string {
  return new Date().toISOString().slice(0, 10)
}

function fmtDate(iso: string): string {
  const d = new Date(iso.length === 10 ? iso + 'T00:00:00Z' : iso)
  return d.toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' })
}

function fmtReviewed(iso: string): string {
  const days = Math.floor((Date.now() - new Date(iso).getTime()) / 86_400_000)
  if (days < 1) return 'today'
  if (days < 60) return `${days} days ago`
  const months = Math.round(days / 30)
  return `${months} month${months === 1 ? '' : 's'} ago`
}

export function Plan() {
  const qc = useQueryClient()
  const plan = useQuery({ queryKey: ['plan'], queryFn: api.plan })
  const people = useQuery({ queryKey: ['people'], queryFn: api.people })

  const invalidate = () => qc.invalidateQueries({ queryKey: ['plan'] })

  const review = useMutation({ mutationFn: api.reviewPlan, onSuccess: invalidate })

  if (plan.isPending) return <SkeletonRows count={4} />
  if (plan.isError)
    return (
      <div className="glass p-8 text-center text-mist-300">
        The plan could not be loaded. Try again in a moment.
      </div>
    )

  const data: FinancialPlan = plan.data
  const sectionFor = (kind: PlanSectionKind, personId?: string) =>
    data.sections.find(
      (s) => s.kind === kind && (s.person_id ?? '') === (personId ?? ''),
    )
  const personSections = (personId: string) =>
    data.sections.filter((s) => s.kind === 'person' && s.person_id === personId)

  const active = data.decisions.filter((d) => !d.superseded && d.status === 'confirmed')
  const proposals = data.decisions.filter((d) => d.status === 'proposed')
  const superseded = data.decisions.filter((d) => d.superseded)

  return (
    <div className="space-y-8">
      <div className="flex flex-wrap items-baseline justify-between gap-3">
        <div>
          <h1 className="text-2xl font-semibold">Plan</h1>
          <p className="mt-1 text-mist-300">
            What your household is doing and why — the intent every other page's numbers are measured against.
          </p>
        </div>
        <div className="text-right">
          <button
            className="rounded-lg bg-arcane-500/20 px-3 py-1.5 text-sm text-mist-100 transition hover:bg-arcane-500/30"
            disabled={review.isPending}
            onClick={() => review.mutate()}
          >
            Mark reviewed
          </button>
          <p className="mt-1 text-xs text-mist-500">
            {data.reviewed_at
              ? `Reviewed ${fmtReviewed(data.reviewed_at)}`
              : 'Never reviewed — stamp it once you have read the whole plan'}
          </p>
        </div>
      </div>

      {/* The outline: household-wide sections as editable prose, one row per
          slot. A save is an upsert — there is exactly one strategy, one income
          story, one estate note. */}
      <div className="grid gap-4 lg:grid-cols-2">
        {HOUSEHOLD_KINDS.map((kind) => (
          <SectionCard
            key={kind}
            label={SECTION_META[kind].label}
            blurb={SECTION_META[kind].blurb}
            initial={sectionFor(kind)?.body ?? ''}
            savedAt={sectionFor(kind) ? fmtReviewed(sectionFor(kind)!.updated_at) : undefined}
            onSave={(body) => api.savePlanSection({ kind, body }).then(invalidate)}
          />
        ))}
      </div>

      {/* Per-person notes: the kid's 529 reasoning attaches to the kid, not to
          a strategy paragraph nobody will find it in. */}
      <section className="space-y-3">
        <h2 className="text-lg font-medium">People</h2>
        {people.isPending ? (
          <SkeletonRows count={1} />
        ) : (people.data ?? []).length === 0 ? (
          <p className="text-sm text-mist-500">
            Add people on the Settings → Household page to keep per-person notes here.
          </p>
        ) : (
          <div className="grid gap-4 lg:grid-cols-2">
            {(people.data ?? []).map((p: Person) => (
              <PersonCard
                key={p.id}
                person={p}
                initial={personSections(p.id)[0]?.body ?? ''}
                savedAt={
                  personSections(p.id)[0]
                    ? fmtReviewed(personSections(p.id)[0].updated_at)
                    : undefined
                }
                onSave={(body) =>
                  body.trim() === ''
                    ? sectionFor('person', p.id)
                      ? api.deletePlanSection(sectionFor('person', p.id)!.id).then(invalidate)
                      : undefined
                    : api
                        .savePlanSection({ kind: 'person', person_id: p.id, body })
                        .then(invalidate)
                }
              />
            ))}
          </div>
        )}
      </section>

      {/* The decisions log: append-only. The form can name a decision it
          replaces; the old one stays, greyed, under its replacement. */}
      <section className="space-y-3">
        <div className="flex items-baseline justify-between gap-3">
          <h2 className="text-lg font-medium">Decisions</h2>
          <p className="text-xs text-mist-500">
            Confirmed decisions are never edited — add one that supersedes the old.
          </p>
        </div>

        {proposals.length > 0 && (
          <div className="space-y-2">
            {proposals.map((d) => (
              <ProposalRow key={d.id} decision={d} onDone={invalidate} />
            ))}
          </div>
        )}

        <div className="space-y-2">
          {active.map((d) => {
            const replaced = superseded.filter((s) => d.id === (s.supersedes ?? ''))
            return (
              <div key={d.id} className="glass p-4">
                <div className="flex flex-wrap items-baseline justify-between gap-2">
                  <span className="font-medium">{d.topic}</span>
                  <span className="text-xs text-mist-500">
                    decided {fmtDate(d.decided_at)}
                    {d.source === 'advisor' && ' · drafted by advisor'}
                  </span>
                </div>
                <p className="mt-1 whitespace-pre-wrap text-sm text-mist-300">{d.body}</p>
                {replaced.length > 0 && (
                  <div className="mt-2 border-l-2 border-white/10 pl-3">
                    {replaced.map((old) => (
                      <p key={old.id} className="text-xs text-mist-500 line-through">
                        {old.topic} ({fmtDate(old.decided_at)})
                      </p>
                    ))}
                  </div>
                )}
              </div>
            )
          })}
          {active.length === 0 && proposals.length === 0 && (
            <p className="glass p-6 text-center text-sm text-mist-500">
              No decisions yet. Write down the ones you have already made — the log is
              more useful for what it replaces than for what it holds today.
            </p>
          )}
        </div>

        <AddDecisionForm activeDecisions={active} onSaved={invalidate} />
      </section>
    </div>
  )
}

function SectionCard({
  label,
  blurb,
  initial,
  savedAt,
  onSave,
}: {
  label: string
  blurb: string
  initial: string
  savedAt?: string
  onSave: (body: string) => unknown
}) {
  const [draft, setDraft] = useState(initial)
  const [dirty, setDirty] = useState(false)
  const save = useMutation({
    mutationFn: (body: string) => onSave(body) as Promise<unknown>,
  })

  return (
    <section className="glass flex flex-col p-4">
      <h3 className="font-medium">{label}</h3>
      <p className="mt-0.5 text-xs text-mist-500">{blurb}</p>
      <textarea
        className="mt-3 min-h-32 flex-1 rounded-lg border border-white/10 bg-black/20 p-3 text-sm leading-relaxed text-mist-100 outline-none focus:border-arcane-500/50"
        value={draft}
        placeholder="Write it as you would say it."
        onChange={(e) => {
          setDraft(e.target.value)
          setDirty(true)
        }}
      />
      <div className="mt-2 flex items-center justify-between">
        <span className="text-xs text-mist-500">
          {savedAt && !dirty ? `updated ${savedAt}` : ''}
          {save.isError ? ' could not save' : ''}
        </span>
        <button
          className="rounded-lg bg-arcane-500/20 px-3 py-1 text-sm text-mist-100 transition hover:bg-arcane-500/30 disabled:opacity-40"
          disabled={!dirty || save.isPending}
          onClick={() => save.mutate(draft, { onSuccess: () => setDirty(false) })}
        >
          Save
        </button>
      </div>
    </section>
  )
}

function PersonCard({
  person,
  initial,
  savedAt,
  onSave,
}: {
  person: Person
  initial: string
  savedAt?: string
  onSave: (body: string) => unknown
}) {
  return (
    <SectionCard
      label={person.display_name}
      blurb={`Notes about ${person.display_name}'s money — the 529 reasoning, custodial situation, allowance thinking.`}
      initial={initial}
      savedAt={savedAt}
      onSave={onSave}
    />
  )
}

function ProposalRow({
  decision,
  onDone,
}: {
  decision: FinancialPlan['decisions'][number]
  onDone: () => void
}) {
  const [topic, setTopic] = useState(decision.topic)
  const [body, setBody] = useState(decision.body)
  const confirm = useMutation({
    mutationFn: () => api.updatePlanDecision(decision.id, { confirm: true }),
    onSuccess: onDone,
  })
  const edit = useMutation({
    mutationFn: () => api.updatePlanDecision(decision.id, { topic, body }),
    onSuccess: onDone,
  })
  const discard = useMutation({
    mutationFn: () => api.discardPlanDecision(decision.id),
    onSuccess: onDone,
  })
  const busy = confirm.isPending || edit.isPending || discard.isPending

  return (
    <div className="glass border-arcane-500/30 p-4">
      <div className="flex flex-wrap items-center gap-2">
        <span className="rounded bg-arcane-500/20 px-1.5 py-0.5 text-[10px] uppercase tracking-wide text-arcane-400">
          proposed{decision.source === 'advisor' ? ' by advisor' : ''}
        </span>
        <input
          className="flex-1 rounded border border-white/10 bg-black/20 px-2 py-1 text-sm font-medium text-mist-100 outline-none focus:border-arcane-500/50"
          value={topic}
          onChange={(e) => setTopic(e.target.value)}
        />
      </div>
      <textarea
        className="mt-2 min-h-20 w-full rounded border border-white/10 bg-black/20 p-2 text-sm text-mist-100 outline-none focus:border-arcane-500/50"
        value={body}
        onChange={(e) => setBody(e.target.value)}
      />
      <div className="mt-2 flex justify-end gap-2 text-sm">
        <button
          className="px-2 py-1 text-mist-500 transition hover:text-mist-300 disabled:opacity-40"
          disabled={busy}
          onClick={() => discard.mutate()}
        >
          Discard
        </button>
        <button
          className="px-2 py-1 text-mist-300 transition hover:text-mist-100 disabled:opacity-40"
          disabled={busy}
          onClick={() => edit.mutate()}
        >
          Save draft
        </button>
        <button
          className="rounded-lg bg-arcane-500/30 px-3 py-1 text-mist-100 transition hover:bg-arcane-500/40 disabled:opacity-40"
          disabled={busy}
          onClick={() => confirm.mutate()}
        >
          Confirm
        </button>
      </div>
    </div>
  )
}

function AddDecisionForm({
  activeDecisions,
  onSaved,
}: {
  activeDecisions: FinancialPlan['decisions']
  onSaved: () => void
}) {
  const [open, setOpen] = useState(false)
  const [topic, setTopic] = useState('')
  const [body, setBody] = useState('')
  const [decidedAt, setDecidedAt] = useState(todayISO())
  const [supersedes, setSupersedes] = useState('')

  const create = useMutation({
    mutationFn: () =>
      api.createPlanDecision({
        topic,
        body,
        decided_at: decidedAt,
        supersedes: supersedes || undefined,
      }),
    onSuccess: () => {
      setTopic('')
      setBody('')
      setDecidedAt(todayISO())
      setSupersedes('')
      setOpen(false)
      onSaved()
    },
  })

  if (!open) {
    return (
      <button
        className="w-full rounded-lg border border-dashed border-white/15 px-4 py-3 text-sm text-mist-400 transition hover:border-arcane-500/40 hover:text-mist-200"
        onClick={() => setOpen(true)}
      >
        + Add a decision
      </button>
    )
  }

  return (
    <div className="glass space-y-3 p-4">
      <input
        className="w-full rounded border border-white/10 bg-black/20 px-3 py-2 text-sm font-medium text-mist-100 outline-none focus:border-arcane-500/50"
        placeholder="What was decided — e.g. Hold the emergency fund at 3 months"
        value={topic}
        onChange={(e) => setTopic(e.target.value)}
      />
      <textarea
        className="min-h-20 w-full rounded border border-white/10 bg-black/20 p-3 text-sm text-mist-100 outline-none focus:border-arcane-500/50"
        placeholder="The why. This is the part you will want to have written down in two years."
        value={body}
        onChange={(e) => setBody(e.target.value)}
      />
      <div className="flex flex-wrap items-center gap-3 text-sm">
        <label className="flex items-center gap-2 text-mist-300">
          Decided
          <input
            type="date"
            className="rounded border border-white/10 bg-black/20 px-2 py-1 text-mist-100"
            value={decidedAt}
            max={todayISO()}
            onChange={(e) => setDecidedAt(e.target.value)}
          />
        </label>
        <label className="flex items-center gap-2 text-mist-300">
          Replaces
          <select
            className="rounded border border-white/10 bg-black/20 px-2 py-1 text-mist-100"
            value={supersedes}
            onChange={(e) => setSupersedes(e.target.value)}
          >
            <option value="">— nothing</option>
            {activeDecisions.map((d) => (
              <option key={d.id} value={d.id}>
                {d.topic}
              </option>
            ))}
          </select>
        </label>
        <div className="ml-auto flex gap-2">
          <button
            className="px-3 py-1.5 text-mist-400 transition hover:text-mist-200"
            onClick={() => setOpen(false)}
          >
            Cancel
          </button>
          <button
            className="rounded-lg bg-arcane-500/20 px-3 py-1.5 text-mist-100 transition hover:bg-arcane-500/30 disabled:opacity-40"
            disabled={topic.trim() === '' || create.isPending}
            onClick={() => create.mutate()}
          >
            Add decision
          </button>
        </div>
      </div>
      {create.isError && (
        <p className="text-sm text-red-400">{String((create.error as Error).message)}</p>
      )}
    </div>
  )
}
