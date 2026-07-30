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
 * True when a debt amortizes on a schedule rather than revolving.
 *
 * This decides what the app CALLS a rate, and the distinction is not cosmetic.
 * On a card, "APR" and "interest rate" are one number: there are no closing
 * costs to fold in. On a mortgage or auto loan they are two — the note rate,
 * which the servicer multiplies by the balance every month, and the disclosed
 * APR, which spreads points, origination fees and PMI across the original
 * principal so that offers can be compared. Every payoff projection in this app
 * wants the first (ComputePayoff: interest = balance × rate / 100 / 12), and a
 * field labelled "APR" on a mortgage asks the household for the second. The
 * gap is a couple of tenths of a point: too small to look wrong, big enough to
 * move a payoff date by months.
 *
 * Keyed on account TYPE and not on subtype/kind, which is Plaid's free-form
 * label — enumerating which of its values revolve would be a guess where the
 * type is already an answer.
 */
export function isAmortizingDebt(accountType: string): boolean {
  return accountType === 'loan'
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
