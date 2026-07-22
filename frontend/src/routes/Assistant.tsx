import { useEffect, useRef, useState, type FormEvent } from 'react'
import { useMutation, useQuery } from '@tanstack/react-query'
import { api, type ChatTurn } from '../lib/api'

const SUGGESTIONS = [
  'How much did I spend this month?',
  'What are my biggest categories?',
  "What's my net worth?",
  'Which subscriptions am I paying for?',
]

export function Assistant() {
  const capabilities = useQuery({
    queryKey: ['capabilities'],
    queryFn: api.capabilities,
    staleTime: Infinity,
  })

  const [turns, setTurns] = useState<ChatTurn[]>([])
  const [input, setInput] = useState('')
  const scrollRef = useRef<HTMLDivElement>(null)

  const send = useMutation({
    mutationFn: (history: ChatTurn[]) => api.chat(history),
    onSuccess: (res) =>
      setTurns((t) => [...t, { role: 'assistant', content: res.reply }]),
  })

  // Keep the newest message in view as the conversation grows.
  useEffect(() => {
    scrollRef.current?.scrollTo({ top: scrollRef.current.scrollHeight })
  }, [turns, send.isPending])

  function ask(text: string) {
    const trimmed = text.trim()
    if (!trimmed || send.isPending) return
    const next: ChatTurn[] = [...turns, { role: 'user', content: trimmed }]
    setTurns(next)
    setInput('')
    send.mutate(next)
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
          The assistant needs an AI provider configured. Set <code>AI_API_KEY</code>{' '}
          to enable it.
        </p>
      </div>
    )
  }

  return (
    <div className="flex h-[calc(100vh-9rem)] flex-col">
      <div>
        <h1 className="text-2xl font-semibold">Assistant</h1>
        <p className="mt-1 text-mist-300">
          Ask about your spending, budgets, or net worth. Answers come from your
          own data.
        </p>
      </div>

      <div
        ref={scrollRef}
        className="mt-6 flex-1 space-y-4 overflow-y-auto rounded-2xl border border-white/10 bg-ink-850/40 p-4"
      >
        {turns.length === 0 && (
          <div className="flex h-full flex-col items-center justify-center gap-4 text-center">
            <p className="text-sm text-mist-500">Try asking…</p>
            <div className="flex flex-wrap justify-center gap-2">
              {SUGGESTIONS.map((q) => (
                <button
                  key={q}
                  className="btn-ghost px-3 py-1.5 text-sm"
                  onClick={() => ask(q)}
                >
                  {q}
                </button>
              ))}
            </div>
          </div>
        )}

        {turns.map((t, i) => (
          <Bubble key={i} role={t.role} content={t.content} />
        ))}

        {send.isPending && (
          <Bubble role="assistant" content="Thinking…" muted />
        )}
        {send.isError && (
          <p role="alert" className="text-sm text-ember-400">
            {send.error.message}
          </p>
        )}
      </div>

      <form onSubmit={onSubmit} className="mt-4 flex gap-3">
        <input
          className="field flex-1"
          placeholder="Ask a question…"
          value={input}
          onChange={(e) => setInput(e.target.value)}
          disabled={send.isPending}
        />
        <button type="submit" className="btn-primary" disabled={send.isPending || !input.trim()}>
          Send
        </button>
      </form>
    </div>
  )
}

function Bubble({
  role,
  content,
  muted,
}: {
  role: 'user' | 'assistant'
  content: string
  muted?: boolean
}) {
  const isUser = role === 'user'
  return (
    <div className={`flex ${isUser ? 'justify-end' : 'justify-start'}`}>
      <div
        className={`max-w-[85%] whitespace-pre-wrap rounded-2xl px-4 py-2.5 text-sm leading-relaxed ${
          isUser
            ? 'bg-arcane-500/20 text-mist-100'
            : `bg-white/5 text-mist-200 ${muted ? 'text-mist-500' : ''}`
        }`}
      >
        {content}
      </div>
    </div>
  )
}
