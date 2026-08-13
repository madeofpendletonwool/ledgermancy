/**
 * Client-side helpers for the composable transaction search bar.
 *
 * The grammar itself lives in the backend (`internal/search`), and so does the
 * list of operator names — the bar fetches them from
 * `GET /api/transactions/search-operators` rather than keeping its own copy.
 * Everything here is about the *editing* experience: which word the caret is in,
 * what to suggest for it, and how to splice a chosen operator back in.
 *
 * These are pure functions on purpose. The bar is a text input with a popover,
 * and the fiddly part is caret arithmetic, not React — so the arithmetic is
 * testable on its own.
 */

import type { SearchOperator } from './api'

export type { SearchOperator }

/** The whitespace-delimited word the caret sits in, and where it starts/ends. */
export interface ActiveToken {
  start: number
  end: number
  text: string
}

const isSpace = (ch: string) => ch === ' ' || ch === '\t' || ch === '\n'

/**
 * Finds the word the caret is in. A caret directly after a word belongs to that
 * word (that is where you are when you have just typed it), and a caret in
 * whitespace yields an empty token at that position.
 */
export function activeToken(value: string, caret: number): ActiveToken {
  const at = Math.max(0, Math.min(caret, value.length))
  let start = at
  while (start > 0 && !isSpace(value[start - 1])) start--
  let end = at
  while (end < value.length && !isSpace(value[end])) end++
  return { start, end, text: value.slice(start, end) }
}

/**
 * Suggestions for a partially typed token.
 *
 * Nothing is suggested once the token has a colon: at that point the user is
 * typing a *value*, and the values are their own merchant names and dates — not
 * something this list knows anything about. A leading `-` is a negation the user
 * has already committed to, so it is ignored for matching and preserved on
 * insert.
 *
 * Prefix matches rank above substring matches, so typing `cat` offers `category`
 * before `has_no_category`.
 */
export function suggestFor(
  token: string,
  operators: SearchOperator[],
  limit = 8,
): SearchOperator[] {
  const needle = token.replace(/^-/, '').toLowerCase()
  if (needle === '' || token.includes(':')) return []

  const prefix: SearchOperator[] = []
  const substring: SearchOperator[] = []
  for (const op of operators) {
    if (op.name.startsWith(needle)) prefix.push(op)
    else if (op.name.includes(needle)) substring.push(op)
  }
  return [...prefix, ...substring].slice(0, limit)
}

/**
 * Splices a chosen operator over the token the caret is in, and reports where
 * the caret should land.
 *
 * An operator that takes a value ends in `:` with the caret right after it, ready
 * for the value. A flag is complete on its own, so it gets a trailing space and
 * the caret goes past it — the next thing typed starts a new term either way.
 */
export function applySuggestion(
  value: string,
  caret: number,
  op: SearchOperator,
): { value: string; caret: number } {
  const token = activeToken(value, caret)
  const negated = token.text.startsWith('-') ? '-' : ''
  const inserted = `${negated}${op.name}${op.takes_value ? ':' : ' '}`
  return {
    value: value.slice(0, token.start) + inserted + value.slice(token.end),
    caret: token.start + inserted.length,
  }
}

/**
 * A few complete queries to show as examples. Deliberately short and copyable —
 * the point is to teach that terms compose, which one worked example does better
 * than a table of operators.
 */
export const SEARCH_EXAMPLES: { query: string; explains: string }[] = [
  { query: 'starbucks over:10', explains: 'Starbucks charges over $10' },
  { query: 'has_no_category since:-30d', explains: 'Last 30 days, still uncategorised' },
  { query: '-account:Checking is_income', explains: 'Money in, on any account but Checking' },
  {
    query: 'category:groceries since:start-of-this-month',
    explains: 'Groceries so far this month',
  },
  { query: 'merchant_starts:AMZN under:25', explains: 'Small Amazon charges' },
]
