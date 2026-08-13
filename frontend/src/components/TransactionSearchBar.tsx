import { useEffect, useRef, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { api } from '../lib/api'
import {
  activeToken,
  applySuggestion,
  suggestFor,
  SEARCH_EXAMPLES,
  type SearchOperator,
} from '../lib/searchGrammar'

/**
 * The composable search box on the Transactions page.
 *
 * It is one text input over the `q` grammar (see backend `internal/search`): bare
 * words are free text, `key:value` terms narrow, and a leading `-` negates. The
 * filter chips beside it still work and still compose with whatever is typed
 * here — nobody is required to learn the grammar to use the page.
 *
 * Two affordances make the grammar discoverable, which is the whole reason a
 * power-user feature like this earns its place:
 *
 *   - typing a word offers the operators it could become, with their help text
 *   - a cheat-sheet toggle lists worked examples that can be clicked to run
 *
 * The operator list is fetched, not hardcoded, so it cannot drift from the parser
 * that has to accept it.
 */
export function TransactionSearchBar({
  value,
  onChange,
}: {
  value: string
  onChange: (next: string) => void
}) {
  const inputRef = useRef<HTMLInputElement>(null)
  // The token the caret is in, tracked as a range so a suggestion can be spliced
  // over exactly that word. Null means the popover is closed.
  const [caret, setCaret] = useState<number | null>(null)
  const [highlighted, setHighlighted] = useState(0)
  const [showHelp, setShowHelp] = useState(false)

  // Static for a given backend build, so it is fetched once and kept. A failure
  // here costs autocomplete, not search: the input still submits whatever is
  // typed, and the parser is the thing that decides what it means.
  const operators = useQuery({
    queryKey: ['search-operators'],
    queryFn: api.searchOperators,
    staleTime: Infinity,
    retry: false,
  })

  const token = caret === null ? null : activeToken(value, caret)
  const suggestions = token ? suggestFor(token.text, operators.data ?? []) : []
  const open = suggestions.length > 0

  // Keep the highlight in range as the list shrinks under the user's typing,
  // otherwise Enter accepts nothing and reads as a dead key.
  useEffect(() => {
    setHighlighted((h) => (h < suggestions.length ? h : 0))
  }, [suggestions.length])

  const accept = (op: SearchOperator) => {
    const next = applySuggestion(value, caret ?? value.length, op)
    onChange(next.value)
    // The caret has to be placed after React has written the new value, or the
    // browser puts it at the end of the input instead of after the `:`.
    requestAnimationFrame(() => {
      const el = inputRef.current
      if (!el) return
      el.focus()
      el.setSelectionRange(next.caret, next.caret)
      setCaret(next.caret)
    })
  }

  const onKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (!open) {
      // Escape with the popover shut is "clear the search", which is what the
      // native type=search × does and what muscle memory expects.
      if (e.key === 'Escape' && value !== '') {
        e.preventDefault()
        onChange('')
      }
      return
    }
    switch (e.key) {
      case 'ArrowDown':
        e.preventDefault()
        setHighlighted((h) => (h + 1) % suggestions.length)
        break
      case 'ArrowUp':
        e.preventDefault()
        setHighlighted((h) => (h - 1 + suggestions.length) % suggestions.length)
        break
      case 'Tab':
      case 'Enter':
        e.preventDefault()
        accept(suggestions[highlighted])
        break
      case 'Escape':
        e.preventDefault()
        setCaret(null)
        break
    }
  }

  return (
    <div className="relative min-w-64 flex-1">
      <div className="flex items-baseline justify-between">
        <label className="label" htmlFor="search">
          Search
        </label>
        <button
          type="button"
          className="pb-1 text-[11px] text-mist-500 transition-colors hover:text-mist-300"
          aria-expanded={showHelp}
          onClick={() => setShowHelp((v) => !v)}
        >
          {showHelp ? 'hide help' : 'what can I type?'}
        </button>
      </div>
      <input
        id="search"
        ref={inputRef}
        type="search"
        className="field w-full"
        placeholder="starbucks over:10 since:-30d"
        autoComplete="off"
        spellCheck={false}
        value={value}
        onChange={(e) => {
          onChange(e.target.value)
          setCaret(e.target.selectionStart)
        }}
        onKeyDown={onKeyDown}
        onClick={(e) => setCaret(e.currentTarget.selectionStart)}
        // A blur that lands on a suggestion must not close the list before the
        // click registers, so the mousedown handler below does the accepting and
        // this only handles leaving the control for real.
        onBlur={() => setCaret(null)}
      />

      {open && (
        <ul className="absolute z-30 mt-1 max-h-72 w-full overflow-auto rounded-2xl border border-white/10 bg-ink-950/90 p-1.5 shadow-xl shadow-black/40 backdrop-blur-xl">
          {suggestions.map((op, i) => (
            <li key={op.name}>
              <button
                type="button"
                className={`w-full rounded-lg px-2 py-1.5 text-left text-sm ${
                  i === highlighted ? 'bg-white/10' : 'hover:bg-white/5'
                }`}
                // mousedown, not click: the input's blur fires first on click and
                // would unmount this list before the handler ran.
                onMouseDown={(e) => {
                  e.preventDefault()
                  accept(op)
                }}
                onMouseEnter={() => setHighlighted(i)}
              >
                <span className="font-mono text-rune-200">
                  {op.name}
                  {op.takes_value ? ':' : ''}
                </span>
                <span className="ml-2 text-xs text-mist-400">{op.help}</span>
              </button>
            </li>
          ))}
        </ul>
      )}

      {showHelp && (
        <div className="absolute right-0 z-30 mt-1 w-80 rounded-2xl border border-white/10 bg-ink-950/90 p-3 text-sm shadow-xl shadow-black/40 backdrop-blur-xl">
          <p className="text-mist-300">
            Terms are ANDed. Put a <span className="font-mono text-rune-200">-</span> in front of
            one to exclude it, and quote values with spaces.
          </p>
          <ul className="mt-2 space-y-1.5">
            {SEARCH_EXAMPLES.map((ex) => (
              <li key={ex.query}>
                <button
                  type="button"
                  className="w-full rounded-lg px-2 py-1 text-left hover:bg-white/5"
                  onClick={() => {
                    onChange(ex.query)
                    setShowHelp(false)
                  }}
                >
                  <span className="font-mono text-xs text-rune-200">{ex.query}</span>
                  <span className="block text-xs text-mist-400">{ex.explains}</span>
                </button>
              </li>
            ))}
          </ul>
          <p className="mt-2 text-xs text-mist-500">
            Dates take <span className="font-mono">today</span>,{' '}
            <span className="font-mono">start-of-this-month</span>,{' '}
            <span className="font-mono">-30d</span> or <span className="font-mono">2026-01-01</span>
            . Amounts ignore sign — pair with{' '}
            <span className="font-mono">is_expense</span> or{' '}
            <span className="font-mono">is_income</span>.
          </p>
        </div>
      )}
    </div>
  )
}
