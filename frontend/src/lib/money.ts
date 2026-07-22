/**
 * Money formatting.
 *
 * Amounts cross the wire as decimal *strings* on purpose: the backend stores
 * them as exact NUMERIC and JSON numbers would drag them back through a
 * float64. Converting to a JS number for display is safe — formatting one
 * value to two decimal places cannot go wrong — but **never sum these in
 * JavaScript**. `0.1 + 0.2` is a real problem here, and every total the app
 * shows must be computed by the server, where the arithmetic is exact.
 */

/** Formats a decimal string as currency, e.g. "-1234.5" -> "-$1,234.50". */
export function formatMoney(
  value: string | null | undefined,
  currency = 'USD',
): string {
  if (value === null || value === undefined || value === '') return '—'

  const n = Number(value)
  if (!Number.isFinite(n)) return '—'

  return new Intl.NumberFormat('en-US', {
    style: 'currency',
    currency,
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  }).format(n)
}

/**
 * Plaid signs transactions as positive = money leaving the account. That reads
 * backwards in a ledger, so spending is shown as a negative figure and income
 * as a positive one.
 */
export function formatTransactionAmount(amount: string, currency = 'USD') {
  const n = Number(amount)
  const isSpend = n > 0
  return {
    text: formatMoney(String(-n), currency),
    isSpend,
    isIncome: n < 0,
  }
}

/** True when an account type represents money owed rather than money held. */
export function isLiability(accountType: string): boolean {
  return accountType === 'credit' || accountType === 'loan'
}

/**
 * Formats a transaction date.
 *
 * These are calendar dates (Postgres `DATE`), serialised as midnight UTC —
 * "2026-07-13T00:00:00Z". Handing that to `new Date()` and formatting it in any
 * timezone west of UTC renders the *previous* day. That is not cosmetic: a
 * transaction on the 1st would display in the prior month and land in the wrong
 * monthly total. So the calendar parts are read directly and rebuilt as a local
 * date, with no timezone conversion.
 */
export function formatDate(iso: string): string {
  const [year, month, day] = iso.slice(0, 10).split('-').map(Number)
  return new Date(year, month - 1, day).toLocaleDateString('en-US', {
    month: 'short',
    day: 'numeric',
    year: 'numeric',
  })
}

export function formatRelative(iso: string | null): string {
  if (!iso) return 'never'

  const then = new Date(iso).getTime()
  const minutes = Math.round((Date.now() - then) / 60_000)

  if (minutes < 1) return 'just now'
  if (minutes < 60) return `${minutes}m ago`

  const hours = Math.round(minutes / 60)
  if (hours < 24) return `${hours}h ago`

  return `${Math.round(hours / 24)}d ago`
}
