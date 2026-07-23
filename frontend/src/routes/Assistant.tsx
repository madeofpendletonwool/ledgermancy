import { useEffect, useRef, useState, type FormEvent } from 'react'
import { useQuery } from '@tanstack/react-query'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { api, ApiError, type ChatTurn } from '../lib/api'

// High-value prompts that show what the assistant is actually good at: dynamic,
// one-off questions the dashboard can't answer at a glance. Deliberately not
// "what's my net worth" — the app already shows that.
const SUGGESTIONS = [
  'How many times did I eat out in July? Give me a breakdown',
  'Is my dining spending up vs last month?',
  'List my subscriptions and what they cost',
  'What are my biggest merchants this month?',
]

export function Assistant() {
  const capabilities = useQuery({
    queryKey: ['capabilities'],
    queryFn: api.capabilities,
    staleTime: Infinity,
  })

  const [turns, setTurns] = useState<ChatTurn[]>([])
  const [input, setInput] = useState('')
  const [streaming, setStreaming] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const scrollRef = useRef<HTMLDivElement>(null)

  // Keep the newest message in view as it grows.
  useEffect(() => {
    scrollRef.current?.scrollTo({ top: scrollRef.current.scrollHeight })
  }, [turns, streaming])

  async function ask(text: string) {
    const trimmed = text.trim()
    if (!trimmed || streaming) return

    const history: ChatTurn[] = [...turns, { role: 'user', content: trimmed }]
    // Append the user turn plus an empty assistant turn we stream into.
    setTurns([...history, { role: 'assistant', content: '' }])
    setInput('')
    setError(null)
    setStreaming(true)

    try {
      await api.chat(history, (delta) => {
        setTurns((prev) => {
          const copy = prev.slice()
          const last = copy[copy.length - 1]
          copy[copy.length - 1] = { ...last, content: last.content + delta }
          return copy
        })
      })
    } catch (e) {
      const message =
        e instanceof ApiError ? e.message : 'Something went wrong. Try again.'
      setError(message)
      // Drop the placeholder if nothing streamed into it.
      setTurns((prev) => {
        const last = prev[prev.length - 1]
        if (last?.role === 'assistant' && last.content === '') {
          return prev.slice(0, -1)
        }
        return prev
      })
    } finally {
      setStreaming(false)
    }
  }

  function onSubmit(e: FormEvent) {
    e.preventDefault()
    ask(input)
  }

  if (capabilities.data && !capabilities.data.ai_enabled) {
    return (
      <div className="space-y-4">
        <h1 className="text-2xl font-semibold">Assistant</h1>
        <p className="text-mist-300">
          The assistant needs an AI provider configured. Set{' '}
          <code>AI_API_KEY</code> to enable it.
        </p>
      </div>
    )
  }

  const empty = turns.length === 0

  return (
    <div className="flex h-[calc(100vh-9rem)] flex-col gap-4">
      <div>
        <h1 className="text-2xl font-semibold">Assistant</h1>
        <p className="mt-1 text-mist-300">
          Ask about your spending, subscriptions, or trends. Every figure comes
          straight from your own data.
        </p>
      </div>

      <div ref={scrollRef} className="glass flex-1 overflow-y-auto p-4 sm:p-5">
        {empty ? (
          <EmptyState onPick={ask} />
        ) : (
          <div className="space-y-5">
            {turns.map((t, i) => (
              <Message
                key={i}
                role={t.role}
                content={t.content}
                streaming={
                  streaming &&
                  i === turns.length - 1 &&
                  t.role === 'assistant'
                }
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

      <form onSubmit={onSubmit} className="flex gap-3">
        <input
          className="field flex-1"
          placeholder="Ask a question…"
          value={input}
          onChange={(e) => setInput(e.target.value)}
          disabled={streaming}
        />
        <button
          type="submit"
          className="btn-primary"
          disabled={streaming || !input.trim()}
        >
          Send
        </button>
      </form>
    </div>
  )
}

function EmptyState({ onPick }: { onPick: (q: string) => void }) {
  return (
    <div className="flex h-full flex-col items-center justify-center gap-5 text-center">
      <div className="flex h-11 w-11 items-center justify-center rounded-full bg-arcane-500/15 text-lg text-arcane-400">
        ✦
      </div>
      <div>
        <p className="font-medium text-mist-100">Ask about your money</p>
        <p className="mt-1 text-sm text-mist-500">
          Try one of these to get started
        </p>
      </div>
      <div className="flex max-w-xl flex-wrap justify-center gap-2">
        {SUGGESTIONS.map((q) => (
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

function Message({
  role,
  content,
  streaming,
}: {
  role: 'user' | 'assistant'
  content: string
  streaming: boolean
}) {
  if (role === 'user') {
    return (
      <div className="flex justify-end">
        <div className="max-w-[85%] rounded-2xl rounded-br-sm bg-arcane-500/20 px-4 py-2.5 text-sm leading-relaxed whitespace-pre-wrap text-mist-100">
          {content}
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
          {content !== '' && <CopyButton text={content} />}
        </div>
        <div className="rounded-2xl rounded-tl-sm border border-white/10 bg-white/5 px-4 py-3">
          {content === '' && streaming ? (
            <span className="inline-flex items-center gap-2 text-sm text-mist-500">
              Thinking
              <span className="inline-block h-3 w-1.5 animate-pulse rounded-full bg-mist-500" />
            </span>
          ) : (
            <div className="chat-md">
              <ReactMarkdown
                remarkPlugins={[remarkGfm]}
                components={{
                  // Let wide tables scroll inside the bubble instead of
                  // stretching it past the panel edge.
                  table: ({ children }) => (
                    <div className="table-scroll">
                      <table>{children}</table>
                    </div>
                  ),
                }}
              >
                {content}
              </ReactMarkdown>
              {streaming && (
                <span className="ml-0.5 inline-block h-4 w-1.5 translate-y-0.5 animate-pulse rounded-full bg-arcane-400/70 align-middle" />
              )}
            </div>
          )}
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
