import { useEffect, useMemo, useRef, useState, type FormEvent } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import {
  api,
  ApiError,
  type ActionItem,
  type ActionItemStatus,
  type Advice,
  type AdviceOption,
  type AdvisorThread,
  type Briefing,
  type ChatToolResult,
  type ChatTurn,
  type FilingStatus,
  type Goal,
} from '../lib/api'
import { formatMoney } from '../lib/money'
import { AdvisorPanel } from '../components/AdvisorPanel'
import { BucketAllocator } from '../components/BucketAllocator'
import { ChartAttachment } from '../components/ChartAttachment'

/**
 * The Advisor: the Assistant route grown into an advisor's meeting.
 *
 * The structural difference from a chatbot is the point of the page. A reactive
 * assistant answers past-tense questions one at a time; a real advisor meeting
 * OPENS WITH A BRIEFING, works across horizons, and keeps history. So the chat
 * is one region here rather than the whole surface, and it sits underneath a
 * position the household did not have to ask for.
 *
 * THE PAGE WORKS WITH NO AI KEY. The briefing, horizon, options and action items
 * are deterministic server-side computations; only the conversation needs a
 * model. With AI off the chat tab is simply absent and nothing else changes —
 * the same rule every other AI-touched surface in this app follows.
 */

type Tab = 'chat' | 'horizon' | 'options' | 'allocate' | 'actions' | 'assumptions'

/**
 * How many turns of a saved thread travel with a new question.
 *
 * A few under the server's 40-message cap rather than exactly on it. The margin
 * is not decorative: the client is now the thing that decides how much history
 * to send, and a limit sitting flush against the server's leaves no room for
 * either side to change without producing a 400 on somebody's longest
 * conversation.
 */
const MAX_SENT_TURNS = 36

export function Advisor() {
  const capabilities = useQuery({
    queryKey: ['capabilities'],
    queryFn: api.capabilities,
    staleTime: Infinity,
  })
  const briefing = useQuery({ queryKey: ['advisor-briefing'], queryFn: api.advisorBriefing })

  const aiEnabled = capabilities.data?.ai_enabled ?? false
  const [tab, setTab] = useState<Tab>('chat')

  // With AI off the conversation does not exist, so land on the horizon instead
  // of an empty tab. Done as an effect rather than in the initial state because
  // capabilities arrive asynchronously.
  useEffect(() => {
    if (capabilities.data && !aiEnabled && tab === 'chat') setTab('horizon')
  }, [capabilities.data, aiEnabled, tab])

  const tabs: { id: Tab; label: string }[] = [
    ...(aiEnabled ? [{ id: 'chat' as Tab, label: 'Conversation' }] : []),
    { id: 'horizon', label: 'Horizon' },
    { id: 'options', label: 'Options' },
    // The allocator sits after Options for a reason: Options ranks SINGLE
    // picks ("the highest-value thing is to pay the card"), and this is the
    // multi-bucket answer to the same money. Reading them in that order is
    // reading the advice getting more specific.
    { id: 'allocate', label: 'Allocate' },
    { id: 'actions', label: 'Action items' },
    { id: 'assumptions', label: 'Assumptions' },
  ]

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold">Advisor</h1>
        <p className="mt-1 text-mist-300">
          Your position, what it implies, and what you decided to do about it.
          Every figure is computed from your own data — nothing here moves money.
        </p>
      </div>

      <BriefingStrip
        data={briefing.data}
        loading={briefing.isPending}
        error={briefing.isError}
      />

      <nav className="flex flex-wrap gap-1 border-b border-white/10" role="tablist">
        {tabs.map((t) => (
          <button
            key={t.id}
            role="tab"
            aria-selected={tab === t.id}
            onClick={() => setTab(t.id)}
            className={
              tab === t.id
                ? 'border-b-2 border-arcane-400 px-4 py-2 text-sm font-medium text-mist-100'
                : 'border-b-2 border-transparent px-4 py-2 text-sm text-mist-400 hover:text-mist-200'
            }
          >
            {t.label}
          </button>
        ))}
      </nav>

      {tab === 'chat' && aiEnabled && <Conversation />}
      {tab === 'horizon' && <HorizonView briefing={briefing.data} />}
      {tab === 'options' && <OptionsTab />}
      {tab === 'allocate' && <BucketAllocator />}
      {tab === 'actions' && <ActionItemsTray />}
      {tab === 'assumptions' && <AssumptionsPanel />}
    </div>
  )
}

// --------------------------------------------------------------------------
// Briefing strip
// --------------------------------------------------------------------------

/**
 * The four headline figures plus what needs attention.
 *
 * Every nullable field renders as words rather than as a number. "Not reached
 * inside the horizon" and "we could not project two of your debts" are answers;
 * substituting a zero or a dash for either is how a strip starts implying
 * things the app has not computed.
 */
function BriefingStrip({
  data,
  loading,
  error,
}: {
  data?: Briefing
  loading: boolean
  error: boolean
}) {
  if (loading) {
    return <div className="glass h-28 animate-pulse p-6" aria-hidden />
  }
  if (error || !data) {
    return (
      <section className="glass p-6">
        <p className="text-sm text-mist-400">
          Your briefing could not be assembled just now.
        </p>
      </section>
    )
  }

  return (
    <section className="glass p-6">
      <div className="grid gap-5 sm:grid-cols-2 lg:grid-cols-4">
        <Tile label="Net worth" value={formatMoney(data.net_worth)}>
          {formatMoney(data.assets)} in assets less {formatMoney(data.debts)} owed
        </Tile>

        <Tile label="Slack this month" value={formatMoney(data.monthly_slack)}>
          {data.slack_basis === 'after_bills'
            ? 'A typical month, after the bills already scheduled'
            : 'A typical month, from your trailing median'}
          {data.income_months > 0 && data.income_months < 3
            ? ` — only ${data.income_months} month${data.income_months === 1 ? '' : 's'} of history so far`
            : ''}
        </Tile>

        <Tile label="Debt-free" value={debtFreeHeadline(data)}>
          {debtFreeDetail(data)}
        </Tile>

        <Tile label="Financial independence" value={fiHeadline(data)}>
          {data.retirement_projected
            ? 'From your own assumptions — an estimate, not a forecast'
            : 'Confirm a tax treatment on an investment account to project this'}
        </Tile>
      </div>

      <div className="mt-5 border-t border-white/5 pt-4">
        <p className="text-xs text-mist-500">
          Emergency fund: {formatMoney(data.runway.liquid)} liquid
          {data.runway.months
            ? ` — ${data.runway.months} months of your ${formatMoney(data.runway.monthly_fixed)} typical fixed costs, against a ${data.runway.target_months}-month target${
                data.runway.target_amount ? ` (${formatMoney(data.runway.target_amount)})` : ''
              }`
            : ' — no typical fixed costs on record yet, so there is no runway to measure'}
        </p>
      </div>

      {data.attention.length > 0 && (
        <div className="mt-4 border-t border-white/5 pt-4">
          <h2 className="mb-2 text-sm font-medium">Worth your attention</h2>
          <ul className="space-y-2">
            {data.attention.map((a) => (
              <li key={a.id} className="text-sm">
                <span className="font-medium text-mist-200">{a.title}</span>{' '}
                <span className="text-mist-400">{a.body}</span>
              </li>
            ))}
          </ul>
        </div>
      )}
    </section>
  )
}

function Tile({
  label,
  value,
  children,
}: {
  label: string
  value: string
  children: React.ReactNode
}) {
  return (
    <div>
      <div className="text-xs uppercase tracking-wide text-mist-500">{label}</div>
      <div className="mt-1 text-xl font-semibold tabular">{value}</div>
      <p className="mt-1 text-xs leading-relaxed text-mist-500">{children}</p>
    </div>
  )
}

function debtFreeHeadline(b: Briefing): string {
  if (b.debt_free.never) return 'Never'
  if (b.debt_free.date) return monthYear(b.debt_free.date)
  if (b.debt_free.projected === 0 && b.debt_free.excluded === 0) return 'Already'
  return 'Unknown'
}

/**
 * The clause that keeps the headline honest.
 *
 * "The date the LAST debt clears" is stated explicitly because the intuitive
 * reading is the first one, and the first one is a much more flattering number.
 */
function debtFreeDetail(b: Briefing): string {
  const parts: string[] = []
  if (b.debt_free.never) {
    parts.push(
      b.debt_free.never_account
        ? `${b.debt_free.never_account} never clears at its current payment`
        : 'one debt never clears at its current payment',
    )
  } else if (b.debt_free.date) {
    parts.push('when the last of your debts clears, not the first')
  } else if (b.debt_free.projected === 0 && b.debt_free.excluded === 0) {
    parts.push('nothing outstanding')
  }
  if (b.debt_free.excluded > 0) {
    parts.push(
      `${b.debt_free.excluded} debt${b.debt_free.excluded === 1 ? '' : 's'} could not be projected (${b.debt_free.excluded_names.join(', ')})`,
    )
  }
  if (Number(b.debt_free.total_balance) > 0) {
    parts.push(`${formatMoney(b.debt_free.total_balance)} owed`)
  }
  return parts.join(' — ')
}

function fiHeadline(b: Briefing): string {
  if (!b.retirement_projected) return 'Not projected'
  if (b.already_fi) return 'Already there'
  if (b.fi_age == null) return 'Not reached'
  return `Age ${b.fi_age}`
}

function monthYear(date: string): string {
  const d = new Date(`${date}T00:00:00`)
  if (Number.isNaN(d.getTime())) return date
  return d.toLocaleDateString(undefined, { month: 'short', year: 'numeric' })
}

// --------------------------------------------------------------------------
// Conversation
// --------------------------------------------------------------------------

/** Suggestions the household's own position makes relevant. */
function suggestionsFor(b?: Briefing): string[] {
  const out: string[] = []
  if (b && Number(b.debt_free.total_balance) > 0) {
    out.push('How much faster would my debts clear with $200 a month extra?')
  }
  if (b && !b.retirement_projected) {
    out.push('What would it take to retire at 60?')
  } else {
    out.push('Am I on track to retire when I want to?')
  }
  if (b && b.runway.months && Number(b.runway.months) < b.runway.target_months) {
    out.push('How far off is my emergency fund?')
  }
  out.push('What have we spent vs saved over the last 6 months, and what is our average leftover?')
  return out.slice(0, 4)
}

function Conversation() {
  const queryClient = useQueryClient()
  const briefing = useQuery({ queryKey: ['advisor-briefing'], queryFn: api.advisorBriefing })
  const threads = useQuery({ queryKey: ['advisor-threads'], queryFn: api.advisorThreads })

  const [threadID, setThreadID] = useState<string | null>(null)
  const [turns, setTurns] = useState<ChatTurn[]>([])
  const [input, setInput] = useState('')
  const [streaming, setStreaming] = useState(false)
  const [toolSet, setToolSet] = useState<string | null>(null)
  const [addedTools, setAddedTools] = useState<string[]>([])
  const [error, setError] = useState<string | null>(null)
  const scrollRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    scrollRef.current?.scrollTo({ top: scrollRef.current.scrollHeight })
  }, [turns, streaming])

  // Loading a thread REPLACES the transcript rather than appending to it: two
  // conversations spliced together would produce a history the model reads as
  // one, with figures from both.
  async function openThread(id: string) {
    if (streaming) return
    setError(null)
    try {
      const detail = await api.advisorThread(id)
      setThreadID(id)
      setTurns(
        detail.messages.map((m) => ({
          role: m.role,
          content: m.content,
          tools: m.tool_trace,
          stale: m.stale,
        })),
      )
    } catch (e) {
      setError(e instanceof ApiError ? e.message : 'That conversation could not be opened.')
    }
  }

  function newThread() {
    if (streaming) return
    setThreadID(null)
    setTurns([])
    setToolSet(null)
    setAddedTools([])
    setError(null)
  }

  async function ask(text: string) {
    const trimmed = text.trim()
    if (!trimmed || streaming) return

    // A thread is created lazily, on the first question, so an abandoned "new
    // thread" click never leaves an empty row in the sidebar.
    let target = threadID
    if (target === null) {
      try {
        const created = await api.createAdvisorThread(titleFrom(trimmed))
        target = created.id
        setThreadID(created.id)
        queryClient.invalidateQueries({ queryKey: ['advisor-threads'] })
      } catch {
        // A thread that could not be created must not block the answer: fall
        // back to the stateless behaviour the chat has always had.
        target = null
      }
    }

    const history: ChatTurn[] = [...turns, { role: 'user', content: trimmed }]
    setTurns([...history, { role: 'assistant', content: '', tools: [] }])
    setInput('')
    setError(null)
    // Cleared per TURN, not per thread: the line below the composer describes
    // the answer just given, and carrying a previous turn's escalation into it
    // would credit this answer with tools it never used.
    setAddedTools([])
    setStreaming(true)

    const appendToLast = (update: (t: ChatTurn) => ChatTurn) =>
      setTurns((prev) => {
        const copy = prev.slice()
        copy[copy.length - 1] = update(copy[copy.length - 1])
        return copy
      })

    // The server caps a transcript at 40 messages and rejects anything longer.
    // That cap was harmless while conversations lived for one session; a saved
    // thread grows past it, and the failure would be a 400 on the FIRST message
    // of a long-running relationship — exactly the conversation worth keeping.
    // Sending the tail keeps the recent context and drops the oldest, which is
    // also what the reload system line already assumes about staleness.
    const sent = history.slice(-MAX_SENT_TURNS)

    try {
      await api.chat(sent, (delta) => appendToLast((t) => ({ ...t, content: t.content + delta })), {
        threadID: target ?? undefined,
        onToolSet: setToolSet,
        onToolsAdded: (names) =>
          setAddedTools((prev) => [...prev, ...names.filter((n) => !prev.includes(n))]),
        onTool: (frame) =>
          appendToLast((t) => ({ ...t, tools: [...(t.tools ?? []), frame] })),
      })
      if (target) queryClient.invalidateQueries({ queryKey: ['advisor-threads'] })
    } catch (e) {
      setError(e instanceof ApiError ? e.message : 'Something went wrong. Try again.')
      setTurns((prev) => {
        const last = prev[prev.length - 1]
        if (last?.role === 'assistant' && last.content === '') return prev.slice(0, -1)
        return prev
      })
    } finally {
      setStreaming(false)
    }
  }

  const suggestions = useMemo(() => suggestionsFor(briefing.data), [briefing.data])

  return (
    <div className="grid gap-5 lg:grid-cols-[16rem_1fr]">
      <ThreadsSidebar
        threads={threads.data ?? []}
        activeID={threadID}
        onOpen={openThread}
        onNew={newThread}
        disabled={streaming}
      />

      <div className="flex h-[calc(100vh-24rem)] min-h-[26rem] flex-col gap-3">
        <div ref={scrollRef} className="glass flex-1 overflow-y-auto p-4 sm:p-5">
          {turns.length === 0 ? (
            <EmptyState suggestions={suggestions} onPick={ask} />
          ) : (
            <div className="space-y-5">
              {turns.map((t, i) => (
                <Message
                  key={i}
                  turn={t}
                  streaming={streaming && i === turns.length - 1 && t.role === 'assistant'}
                />
              ))}
            </div>
          )}
          {error && (
            <p role="alert" className="mt-4 text-sm text-ember-400">
              {error}
            </p>
          )}
        </div>

        <form
          onSubmit={(e: FormEvent) => {
            e.preventDefault()
            ask(input)
          }}
          className="flex gap-3"
        >
          <input
            className="field flex-1"
            placeholder="Ask about your money…"
            value={input}
            onChange={(e) => setInput(e.target.value)}
            disabled={streaming}
          />
          <button type="submit" className="btn-primary" disabled={streaming || !input.trim()}>
            Send
          </button>
        </form>

        {toolSet && (
          /*
           * Which tool set the server chose, shown rather than hidden. The set is
           * picked by a deterministic classifier over the question, and naming it
           * is what turns "that answer was worse than I expected" into "it was
           * sent the spending tools for a debt question" — a wrong pick becomes
           * visible instead of mysterious.
           */
          <p className="text-[11px] text-mist-600">
            Answered with the <span className="text-mist-400">{toolSet}</span> tools
            {addedTools.length > 0 && (
              /*
               * And what it had to fetch on top. The set can be picked wrong;
               * the assistant now recovers by loading what it needs mid-turn
               * instead of reporting the capability missing. Naming the additions
               * is what turns that recovery into a measurable signal — a set
               * escalated out of on most turns is a membership bug.
               */
              <>
                , plus <span className="text-mist-400">{addedTools.join(', ')}</span>
              </>
            )}
            .
          </p>
        )}
      </div>
    </div>
  )
}

/** A thread title from the first question, so the sidebar reads as a list of
 *  questions rather than of timestamps. */
function titleFrom(question: string): string {
  const clean = question.replace(/\s+/g, ' ').trim()
  return clean.length > 60 ? `${clean.slice(0, 57)}…` : clean
}

function ThreadsSidebar({
  threads,
  activeID,
  onOpen,
  onNew,
  disabled,
}: {
  threads: AdvisorThread[]
  activeID: string | null
  onOpen: (id: string) => void
  onNew: () => void
  disabled: boolean
}) {
  const queryClient = useQueryClient()
  const remove = useMutation({
    mutationFn: api.deleteAdvisorThread,
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['advisor-threads'] }),
  })

  return (
    <aside className="glass flex max-h-[calc(100vh-24rem)] min-h-[12rem] flex-col p-3">
      <button type="button" className="btn-ghost mb-2 text-sm" onClick={onNew} disabled={disabled}>
        New conversation
      </button>
      <div className="min-h-0 flex-1 overflow-y-auto">
        {threads.length === 0 ? (
          <p className="px-2 py-3 text-xs text-mist-500">
            Saved conversations appear here. An advisor relationship is
            cumulative — the next question does not start from nothing.
          </p>
        ) : (
          <ul className="space-y-0.5">
            {threads.map((t) => (
              <li key={t.id} className="group flex items-center gap-1">
                <button
                  type="button"
                  onClick={() => onOpen(t.id)}
                  disabled={disabled}
                  className={
                    'min-w-0 flex-1 truncate rounded-lg px-2 py-1.5 text-left text-xs ' +
                    (t.id === activeID
                      ? 'bg-arcane-500/15 text-mist-100'
                      : 'text-mist-400 hover:bg-white/5 hover:text-mist-200')
                  }
                  title={t.title}
                >
                  {t.title}
                  {!t.is_shared && <span className="ml-1 text-mist-600">(private)</span>}
                </button>
                <button
                  type="button"
                  onClick={() => remove.mutate(t.id)}
                  className="shrink-0 px-1 text-xs text-mist-600 opacity-0 transition group-hover:opacity-100 hover:text-ember-400"
                  aria-label={`Delete ${t.title}`}
                >
                  ×
                </button>
              </li>
            ))}
          </ul>
        )}
      </div>
    </aside>
  )
}

function EmptyState({
  suggestions,
  onPick,
}: {
  suggestions: string[]
  onPick: (q: string) => void
}) {
  return (
    <div className="flex h-full flex-col items-center justify-center gap-5 text-center">
      <div className="flex h-11 w-11 items-center justify-center rounded-full bg-arcane-500/15 text-lg text-arcane-400">
        ✦
      </div>
      <div>
        <p className="font-medium text-mist-100">Ask about your money</p>
        <p className="mt-1 text-sm text-mist-500">
          These are picked from what your own position makes relevant
        </p>
      </div>
      <div className="flex max-w-xl flex-wrap justify-center gap-2">
        {suggestions.map((q) => (
          <button
            key={q}
            className="btn-ghost px-3 py-1.5 text-left text-sm"
            onClick={() => onPick(q)}
          >
            {q}
          </button>
        ))}
      </div>
    </div>
  )
}

function Message({ turn, streaming }: { turn: ChatTurn; streaming: boolean }) {
  if (turn.role === 'user') {
    return (
      <div className="flex justify-end">
        <div className="max-w-[85%] rounded-2xl rounded-br-sm bg-arcane-500/20 px-4 py-2.5 text-sm leading-relaxed whitespace-pre-wrap text-mist-100">
          {turn.content}
        </div>
      </div>
    )
  }

  return (
    <div className="flex gap-3">
      <div className="mt-0.5 flex h-7 w-7 shrink-0 items-center justify-center rounded-full bg-arcane-500/15 text-sm text-arcane-400">
        ✦
      </div>
      <div className="min-w-0 flex-1">
        <div className="mb-1 flex items-center gap-2">
          <span className="text-xs font-medium text-mist-500">Ledgermancy</span>
          {turn.stale && (
            /*
             * FIGURES IN HISTORY ARE CONTEXT, NEVER CURRENT. A reloaded turn's
             * money was true when it was written and may not be now; the model
             * is told the same thing on reload, and the reader deserves to be
             * told it too rather than reading a six-week-old figure as today's.
             */
            <span className="rounded bg-white/5 px-1.5 py-0.5 text-[10px] text-mist-500">
              figures as of this conversation
            </span>
          )}
          {turn.content !== '' && <CopyButton text={turn.content} />}
        </div>
        <div className="rounded-2xl rounded-tl-sm border border-white/10 bg-white/5 px-4 py-3">
          {turn.content === '' && streaming && (turn.tools?.length ?? 0) === 0 ? (
            <span className="inline-flex items-center gap-2 text-sm text-mist-500">
              Thinking
              <span className="inline-block h-3 w-1.5 animate-pulse rounded-full bg-mist-500" />
            </span>
          ) : (
            <div className="chat-md">
              <ReactMarkdown
                remarkPlugins={[remarkGfm]}
                components={{
                  table: ({ children }) => (
                    <div className="table-scroll">
                      <table>{children}</table>
                    </div>
                  ),
                }}
              >
                {turn.content}
              </ReactMarkdown>
              {streaming && (
                <span className="ml-0.5 inline-block h-4 w-1.5 translate-y-0.5 animate-pulse rounded-full bg-arcane-400/70 align-middle" />
              )}
            </div>
          )}

          {/* Charts mount as their tool results land, which is BEFORE the final
              prose — tool calls complete earlier in the server's loop. The chart
              appearing while the answer composes reads as native rather than as
              an afterthought bolted on at the end. */}
          {(turn.tools ?? []).map((frame: ChatToolResult, i: number) => (
            <ChartAttachment key={`${frame.tool}-${i}`} frame={frame} />
          ))}
        </div>
      </div>
    </div>
  )
}

function CopyButton({ text }: { text: string }) {
  const [copied, setCopied] = useState(false)
  return (
    <button
      type="button"
      className="text-xs text-mist-500 transition hover:text-mist-300"
      onClick={async () => {
        try {
          await navigator.clipboard.writeText(text)
          setCopied(true)
          setTimeout(() => setCopied(false), 1500)
        } catch {
          /* clipboard unavailable; ignore */
        }
      }}
    >
      {copied ? 'Copied' : 'Copy'}
    </button>
  )
}

// --------------------------------------------------------------------------
// Horizon
// --------------------------------------------------------------------------

type Horizon = 'short' | 'medium' | 'long'

const HORIZON_LABELS: Record<Horizon, string> = {
  short: 'Next 30 days',
  medium: 'Goals in the next few years',
  long: 'Retirement',
}

/**
 * Target versus projected, across three time horizons.
 *
 * The split is by WHEN a thing resolves, not by what kind of thing it is: a bill
 * due Friday and a goal three years out need different reactions, and mixing
 * them into one list is how the urgent gets lost among the important.
 */
function HorizonView({ briefing }: { briefing?: Briefing }) {
  const [horizon, setHorizon] = useState<Horizon>('short')
  const upcoming = useQuery({
    queryKey: ['advisor-upcoming'],
    queryFn: () => api.upcomingObligations(30),
    enabled: horizon === 'short',
  })
  const goals = useQuery({
    queryKey: ['goals'],
    queryFn: api.goals,
    enabled: horizon === 'medium',
  })

  return (
    <section className="glass p-6">
      <div className="mb-4 flex flex-wrap gap-2">
        {(['short', 'medium', 'long'] as Horizon[]).map((h) => (
          <button
            key={h}
            type="button"
            onClick={() => setHorizon(h)}
            className={
              horizon === h
                ? 'rounded-full bg-arcane-500/20 px-3 py-1 text-xs text-mist-100'
                : 'rounded-full px-3 py-1 text-xs text-mist-400 hover:bg-white/5'
            }
          >
            {HORIZON_LABELS[h]}
          </button>
        ))}
      </div>

      {horizon === 'short' && (
        <div>
          {upcoming.isPending && <p className="text-sm text-mist-500">Loading…</p>}
          {upcoming.data && upcoming.data.items.length === 0 && (
            <p className="text-sm text-mist-400">
              No scheduled bills in the next 30 days.
            </p>
          )}
          {upcoming.data && upcoming.data.items.length > 0 && (
            <>
              <p className="mb-3 text-sm text-mist-400">
                {formatMoney(upcoming.data.total)} due between{' '}
                {upcoming.data.from} and {upcoming.data.to}.
              </p>
              <ul className="space-y-1.5">
                {upcoming.data.items.map((o, i) => (
                  <li
                    key={`${o.obligation_id}-${o.due_date}-${i}`}
                    className="flex items-baseline justify-between gap-4 text-sm"
                  >
                    <span className="text-mist-300">{o.label}</span>
                    <span className="shrink-0 text-mist-500">
                      {o.due_date} · <span className="tabular">{formatMoney(o.amount)}</span>
                    </span>
                  </li>
                ))}
              </ul>
            </>
          )}
        </div>
      )}

      {horizon === 'medium' && (
        <div>
          {goals.isPending && <p className="text-sm text-mist-500">Loading…</p>}
          {goals.data && goals.data.length === 0 && (
            <p className="text-sm text-mist-400">No goals set yet.</p>
          )}
          <ul className="space-y-3">
            {(goals.data ?? []).map((g) => (
              <GoalRow key={g.id} goal={g} />
            ))}
          </ul>
        </div>
      )}

      {horizon === 'long' && (
        <div className="space-y-2 text-sm">
          {!briefing || !briefing.retirement_projected ? (
            <p className="text-mist-400">
              Nothing to project yet. Confirm a tax treatment on an investment
              account and set your assumptions, and this fills in.
            </p>
          ) : (
            <>
              <Row label="Financial independence" value={fiHeadline(briefing)} />
              <Row
                label="Debt-free"
                value={debtFreeHeadline(briefing)}
                note="the date your LAST debt clears"
              />
              <Row
                label="Emergency fund"
                value={
                  briefing.runway.months
                    ? `${briefing.runway.months} of ${briefing.runway.target_months} months`
                    : 'no runway to measure yet'
                }
                note={
                  briefing.runway.target_amount
                    ? `${formatMoney(briefing.runway.target_amount)} target, at ${formatMoney(briefing.runway.monthly_fixed)} typical fixed costs a month`
                    : undefined
                }
              />
              <p className="pt-2 text-xs text-mist-500">
                An estimate at a flat real return, not a market forecast. Tax on
                withdrawals and required minimum distributions are not modelled.
              </p>
            </>
          )}
        </div>
      )}
    </section>
  )
}

function GoalRow({ goal }: { goal: Goal }) {
  const target = Number(goal.target_amount)
  const current = Number(goal.current_amount)
  const pct = target > 0 ? Math.min(100, Math.max(0, (current / target) * 100)) : 0

  return (
    <li className="rounded-xl border border-white/10 bg-white/[0.03] p-4">
      <div className="flex items-baseline justify-between gap-3">
        <span className="font-medium">{goal.name}</span>
        <span className="shrink-0 text-sm tabular text-mist-400">
          {formatMoney(goal.current_amount)} / {formatMoney(goal.target_amount)}
        </span>
      </div>
      <div className="mt-2 h-1.5 overflow-hidden rounded-full bg-white/5">
        <div
          className={goal.on_track ? 'h-full bg-rune-400' : 'h-full bg-amber-400'}
          style={{ width: `${pct}%` }}
        />
      </div>
      <p className="mt-2 text-xs text-mist-500">
        {goal.achieved
          ? 'Achieved.'
          : goal.open_ended
            ? 'Open-ended — no date to be behind on.'
            : goal.on_track
              ? `On track: ${formatMoney(goal.required_monthly)}/mo over ${goal.months_left} months.`
              : `Behind by ${formatMoney(goal.shortfall)}/mo — needs ${formatMoney(goal.required_monthly)}/mo over ${goal.months_left} months.`}
      </p>
    </li>
  )
}

function Row({ label, value, note }: { label: string; value: string; note?: string }) {
  return (
    <div className="flex items-baseline justify-between gap-4 border-b border-white/5 py-2 last:border-0">
      <span className="text-mist-400">
        {label}
        {note && <span className="ml-2 text-xs text-mist-600">{note}</span>}
      </span>
      <span className="font-medium tabular">{value}</span>
    </div>
  )
}

// --------------------------------------------------------------------------
// Options
// --------------------------------------------------------------------------

/**
 * The ranked options, with an accept that WRITES A NOTE and nothing else.
 *
 * The panel is shared with the Dashboard so the two surfaces cannot show
 * different orderings of the same list. The only difference here is the accept
 * — and the empty state. AdvisorPanel degrades to SILENCE when there is nothing
 * to say, which is the right rule for a Dashboard glance and the wrong one for
 * a tab a household navigated to on purpose. A dedicated tab that renders
 * nothing answers "what is the point of this?" with nothing, so the silence is
 * turned into a stated reason here while the panel itself is left untouched.
 */
function OptionsTab() {
  const queryClient = useQueryClient()
  const [saved, setSaved] = useState<string | null>(null)
  const advice = useQuery({ queryKey: ['advisor'], queryFn: api.advisor })

  const track = useMutation({
    mutationFn: (o: AdviceOption) =>
      api.createActionItem({
        title: o.label,
        detail: o.detail,
        source: 'option',
      }),
    onSuccess: (item) => {
      setSaved(item.title)
      queryClient.invalidateQueries({ queryKey: ['advisor-action-items'] })
      setTimeout(() => setSaved(null), 3000)
    },
  })

  const hasList =
    !!advice.data && advice.data.significant && advice.data.options.length > 0

  return (
    <div className="space-y-3">
      {hasList ? (
        <AdvisorPanel onAccept={(o) => track.mutate(o)} />
      ) : (
        <OptionsEmpty advice={advice} />
      )}
      {saved && (
        <p role="status" className="text-sm text-rune-300">
          Tracked “{saved}” in your action items. Nothing was moved — this is a
          note, not an instruction.
        </p>
      )}
    </div>
  )
}

/**
 * The Options tab's reason for being empty, stated rather than left blank.
 *
 * The silence rule lives in AdvisorPanel and is not repeated here: this
 * component owns only the cases where there is no list to render, and its job
 * is to name the specific one. The copy stays CONDITIONAL on the same slack
 * figure the panel uses — never "you have $X available", always the framing
 * the advisor surface keeps everywhere else.
 */
function OptionsEmpty({
  advice,
}: {
  advice: ReturnType<typeof useQuery<Advice>>
}) {
  const slack = advice.data ? Number(advice.data.slack) : NaN
  const threshold = advice.data ? Number(advice.data.threshold) : NaN

  return (
    <section className="glass p-6">
      <h2 className="text-lg font-medium">Ranked options for surplus cash</h2>
      <p className="mt-1 text-sm text-mist-500">
        When a typical month leaves money over, the advisor ranks what it would
        do with it — fund the emergency fund, capture an employer match, pay a
        debt, accelerate a goal. Every figure and the order itself are computed
        from your own data, and nothing here moves any money.
      </p>

      <div className="mt-5 border-t border-white/5 pt-4">
        {advice.isPending ? (
          <p className="text-sm text-mist-500">Reading your position…</p>
        ) : advice.isError ? (
          <p className="text-sm text-mist-400">
            Your options could not be computed just now.
          </p>
        ) : Number.isFinite(slack) && slack <= 0 ? (
          <p className="text-sm text-mist-400">
            A typical month leaves nothing over to rank right now
            {slack < 0 ? ` — about ${formatMoney(advice.data!.slack)} short.` : '.'}
          </p>
        ) : Number.isFinite(slack) &&
          Number.isFinite(threshold) &&
          slack < threshold ? (
          <p className="text-sm text-mist-400">
            A typical month leaves about {formatMoney(advice.data!.slack)} over,
            below the {formatMoney(advice.data!.threshold)} floor you set for
            when the advisor speaks up. It will appear here once a typical month
            clears that. Lower the floor under Settings → Advisor if you would
            like it sooner.
          </p>
        ) : (
          <p className="text-sm text-mist-400">
            A typical month leaves about {formatMoney(advice.data!.slack)} over,
            but there is nothing to rank it against yet. Add a debt with an APR,
            a goal, or a retirement account and the advisor will order what each
            would do with it.
          </p>
        )}
      </div>
    </section>
  )
}

// --------------------------------------------------------------------------
// Action items
// --------------------------------------------------------------------------

function ActionItemsTray() {
  const queryClient = useQueryClient()
  const items = useQuery({ queryKey: ['advisor-action-items'], queryFn: () => api.actionItems() })
  const [title, setTitle] = useState('')

  const invalidate = () =>
    queryClient.invalidateQueries({ queryKey: ['advisor-action-items'] })

  const add = useMutation({
    mutationFn: (t: string) => api.createActionItem({ title: t, source: 'manual' }),
    onSuccess: () => {
      setTitle('')
      invalidate()
    },
  })
  const setStatus = useMutation({
    mutationFn: ({ id, status }: { id: string; status: ActionItemStatus }) =>
      api.updateActionItem(id, status),
    onSuccess: invalidate,
  })

  const open = (items.data ?? []).filter((i) => i.status === 'open')
  const closed = (items.data ?? []).filter((i) => i.status !== 'open')

  return (
    <section className="glass p-6">
      <h2 className="text-lg font-medium">Action items</h2>
      <p className="mt-1 mb-4 text-sm text-mist-500">
        What you decided to do. The advisor tracks these and executes none of
        them — there are no transfers or payments here, by design.
      </p>

      <form
        onSubmit={(e) => {
          e.preventDefault()
          if (title.trim()) add.mutate(title.trim())
        }}
        className="mb-5 flex gap-2"
      >
        <input
          className="field flex-1"
          placeholder="Add something you decided to do…"
          value={title}
          onChange={(e) => setTitle(e.target.value)}
        />
        <button type="submit" className="btn-ghost" disabled={!title.trim() || add.isPending}>
          Add
        </button>
      </form>

      {items.isPending && <p className="text-sm text-mist-500">Loading…</p>}
      {items.data && items.data.length === 0 && (
        <p className="text-sm text-mist-400">Nothing tracked yet.</p>
      )}

      {open.length > 0 && (
        <ul className="space-y-2">
          {open.map((i) => (
            <ActionRow key={i.id} item={i} onStatus={(s) => setStatus.mutate({ id: i.id, status: s })} />
          ))}
        </ul>
      )}

      {closed.length > 0 && (
        <div className="mt-5">
          <h3 className="mb-2 text-sm font-medium text-mist-400">Closed</h3>
          <ul className="space-y-2">
            {closed.map((i) => (
              <ActionRow
                key={i.id}
                item={i}
                onStatus={(s) => setStatus.mutate({ id: i.id, status: s })}
              />
            ))}
          </ul>
        </div>
      )}
    </section>
  )
}

function ActionRow({
  item,
  onStatus,
}: {
  item: ActionItem
  onStatus: (status: ActionItemStatus) => void
}) {
  const done = item.status === 'done'
  const dismissed = item.status === 'dismissed'
  return (
    <li className="flex items-start gap-3 rounded-xl border border-white/10 bg-white/[0.03] p-3">
      <div className="min-w-0 flex-1">
        <p
          className={
            'text-sm ' + (done || dismissed ? 'text-mist-500 line-through' : 'text-mist-200')
          }
        >
          {item.title}
        </p>
        {item.detail && <p className="mt-0.5 text-xs text-mist-500">{item.detail}</p>}
        <p className="mt-1 text-[11px] text-mist-600">
          {sourceLabel(item.source)}
          {item.due_date && ` · due ${item.due_date}`}
        </p>
      </div>
      <div className="flex shrink-0 gap-2 text-xs">
        {item.status !== 'open' ? (
          <button type="button" className="text-mist-500 hover:text-mist-300" onClick={() => onStatus('open')}>
            Reopen
          </button>
        ) : (
          <>
            <button type="button" className="text-rune-300 hover:underline" onClick={() => onStatus('done')}>
              Done
            </button>
            <button
              type="button"
              className="text-mist-500 hover:text-mist-300"
              onClick={() => onStatus('dismissed')}
            >
              Dismiss
            </button>
          </>
        )}
      </div>
    </li>
  )
}

function sourceLabel(source: ActionItem['source']): string {
  switch (source) {
    case 'option':
      return 'from a ranked option'
    case 'allocation':
      return 'from an allocation plan'
    case 'thread':
      return 'from a conversation'
    default:
      return 'added by hand'
  }
}

// --------------------------------------------------------------------------
// Assumptions
// --------------------------------------------------------------------------

const FILING_STATUSES: { value: FilingStatus; label: string }[] = [
  { value: 'single', label: 'Single' },
  { value: 'married_joint', label: 'Married, filing jointly' },
  { value: 'married_separate', label: 'Married, filing separately' },
  { value: 'hoh', label: 'Head of household' },
]

/**
 * The inputs behind every projection on this page, editable in place.
 *
 * Doc 15's rule, applied one surface over: a curve must never be renderable
 * without the assumptions that produced it. The advisor states an FI age and a
 * debt-free date; the household has to be able to see — and change — what they
 * rest on without leaving the page.
 */
function AssumptionsPanel() {
  const queryClient = useQueryClient()
  const assumptions = useQuery({
    queryKey: ['retirement-assumptions'],
    queryFn: api.retirementAssumptions,
  })
  const profile = useQuery({ queryKey: ['household-profile'], queryFn: api.householdProfile })

  const [filing, setFiling] = useState<string>('')
  const [floor, setFloor] = useState('')
  const [magi, setMagi] = useState('')
  const [magiYear, setMagiYear] = useState<string>(String(new Date().getFullYear()))
  const [saved, setSaved] = useState(false)

  useEffect(() => {
    if (!profile.data) return
    setFiling(profile.data.filing_status ?? '')
    setFloor(profile.data.risk_drawdown_floor ?? '')
    setMagi(profile.data.magi ?? '')
    if (profile.data.magi_tax_year != null) setMagiYear(String(profile.data.magi_tax_year))
  }, [profile.data])

  const save = useMutation({
    mutationFn: () =>
      api.updateHouseholdProfile({
        filing_status: filing === '' ? null : (filing as FilingStatus),
        risk_drawdown_floor: floor.trim() === '' ? null : floor.trim(),
        magi: magi.trim() === '' ? null : magi.trim(),
        magi_tax_year: magi.trim() === '' ? null : Number(magiYear),
      }),
    onSuccess: () => {
      setSaved(true)
      queryClient.invalidateQueries({ queryKey: ['household-profile'] })
      setTimeout(() => setSaved(false), 2500)
    },
  })

  const a = assumptions.data

  return (
    <section className="glass space-y-6 p-6">
      <div>
        <h2 className="text-lg font-medium">Assumptions</h2>
        <p className="mt-1 text-sm text-mist-500">
          {a?.basis ??
            'Every projection on this page rests on these. They are yours to set, and they travel with every figure derived from them.'}
        </p>
      </div>

      {a && (
        <div className="space-y-1 text-sm">
          <Row label="Real return rate" value={`${pct(a.real_return_rate)}%`} />
          <Row label="Withdrawal rate" value={`${pct(a.withdrawal_rate)}%`} />
          <Row label="Inflation rate" value={`${pct(a.inflation_rate)}%`} />
          <Row
            label="Current age"
            value={a.current_age == null ? 'not set' : String(a.current_age)}
          />
          <Row
            label="Target retirement age"
            value={a.target_retirement_age == null ? 'not decided' : String(a.target_retirement_age)}
          />
          <Row
            label="Retirement needs to support"
            value={formatMoney(a.target_annual_spending ?? a.defaulted_spending)}
            note={a.spending_is_defaulted ? 'your own trailing spend' : undefined}
          />
          <p className="pt-2 text-xs text-mist-500">
            Edit these on the Retirement page — they are shared with every
            projection, so there is one place to change them rather than two.
          </p>
        </div>
      )}

      <form
        onSubmit={(e) => {
          e.preventDefault()
          save.mutate()
        }}
        className="space-y-4 border-t border-white/5 pt-5"
      >
        <div>
          <h3 className="text-sm font-medium">Household profile</h3>
          <p className="mt-1 text-xs text-mist-500">
            Two facts the contribution and guardrail rules key on. Leaving either
            blank is a real answer — the app treats “not told” as unknown rather
            than guessing.
          </p>
        </div>

        <label className="block">
          <span className="mb-1 block text-sm text-mist-300">Filing status</span>
          <select
            className="field w-full"
            value={filing}
            onChange={(e) => setFiling(e.target.value)}
          >
            <option value="">Not set</option>
            {FILING_STATUSES.map((f) => (
              <option key={f.value} value={f.value}>
                {f.label}
              </option>
            ))}
          </select>
          <span className="mt-1 block text-xs text-mist-500">
            Used to check whether you are eligible for a Roth or IRA
            contribution, not just how much the cap allows.
          </span>
        </label>

        <div className="grid gap-4 sm:grid-cols-[1fr_8rem]">
          <label className="block">
            <span className="mb-1 block text-sm text-mist-300">
              Modified AGI (optional)
            </span>
            <input
              className="field w-full"
              inputMode="decimal"
              placeholder="e.g. 185000.00"
              value={magi}
              onChange={(e) => setMagi(e.target.value)}
            />
            <span className="mt-1 block text-xs text-mist-500">
              Ledgermancy cannot work this out — a MAGI is not your gross income and
              not your AGI. Without it, Roth eligibility is reported as{' '}
              <em>unknown</em> rather than assumed to be fine.
            </span>
          </label>
          <label className="block">
            <span className="mb-1 block text-sm text-mist-300">Tax year</span>
            <input
              className="field w-full"
              inputMode="numeric"
              value={magiYear}
              onChange={(e) => setMagiYear(e.target.value)}
            />
            <span className="mt-1 block text-xs text-mist-500">
              A figure from another year is treated as absent.
            </span>
          </label>
        </div>

        <label className="block">
          <span className="mb-1 block text-sm text-mist-300">
            Drawdown floor (percent)
          </span>
          <input
            className="field w-full"
            inputMode="decimal"
            placeholder="e.g. 20.00"
            value={floor}
            onChange={(e) => setFloor(e.target.value)}
          />
          <span className="mt-1 block text-xs text-mist-500">
            The largest peak-to-trough fall you are willing to plan around.
          </span>
        </label>

        <div className="flex items-center gap-3">
          <button type="submit" className="btn-primary" disabled={save.isPending}>
            Save profile
          </button>
          {saved && <span className="text-sm text-rune-300">Saved.</span>}
          {save.isError && (
            <span className="text-sm text-ember-400">
              {save.error instanceof ApiError ? save.error.message : 'Could not save.'}
            </span>
          )}
        </div>
      </form>
    </section>
  )
}

/** A stored fraction ("0.05") rendered as a percentage figure ("5"). */
function pct(fraction: string): string {
  const n = Number(fraction)
  if (!Number.isFinite(n)) return fraction
  return String(Math.round(n * 10000) / 100)
}
