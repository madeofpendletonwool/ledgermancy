import { afterEach, describe, expect, it, vi } from 'vitest'

import {
  formatDate,
  formatMoney,
  formatRelative,
  formatTransactionAmount,
  isAmortizingDebt,
  isLiability,
} from './money'

afterEach(() => {
  vi.unstubAllEnvs()
  vi.useRealTimers()
})

describe('formatMoney', () => {
  it('formats a decimal string as en-US currency', () => {
    expect(formatMoney('-1234.5')).toBe('-$1,234.50')
    expect(formatMoney('1234.5')).toBe('$1,234.50')
    expect(formatMoney('0')).toBe('$0.00')
    expect(formatMoney('12345678.909')).toBe('$12,345,678.91')
  })

  it('always shows exactly two decimal places', () => {
    expect(formatMoney('1')).toBe('$1.00')
    expect(formatMoney('1.005')).toBe('$1.01')
  })

  it('renders a missing amount as an em dash rather than zero', () => {
    // The distinction matters: a balance the server did not send is not a
    // balance of zero, and "$0.00" would assert something untrue.
    expect(formatMoney(null)).toBe('—')
    expect(formatMoney(undefined)).toBe('—')
    expect(formatMoney('')).toBe('—')
  })

  it('renders an unparseable amount as an em dash', () => {
    expect(formatMoney('not a number')).toBe('—')
    expect(formatMoney('Infinity')).toBe('—')
    expect(formatMoney('NaN')).toBe('—')
  })

  it('honours a non-default currency', () => {
    expect(formatMoney('1234.5', 'EUR')).toBe('€1,234.50')
    expect(formatMoney('1234.5', 'GBP')).toBe('£1,234.50')
  })

  it('keeps the sign on an amount that rounds to zero', () => {
    // "-$0.00" looks like a typo but is the honest rendering: the value is
    // negative and it is smaller than the display precision. Pinned so a
    // future "tidy up the minus sign" change is a deliberate one.
    expect(formatMoney('-0.004')).toBe('-$0.00')
  })
})

describe('formatTransactionAmount', () => {
  // Plaid signs a purchase POSITIVE (money left the account). Every one of
  // these asserts the flip, because getting it backwards turns the whole
  // transaction list into income.
  it('renders spending as a negative figure', () => {
    const spend = formatTransactionAmount('42.50')
    expect(spend.text).toBe('-$42.50')
    expect(spend.isSpend).toBe(true)
    expect(spend.isIncome).toBe(false)
  })

  it('renders income as a positive figure', () => {
    const income = formatTransactionAmount('-2500')
    expect(income.text).toBe('$2,500.00')
    expect(income.isSpend).toBe(false)
    expect(income.isIncome).toBe(true)
  })

  it('treats zero as neither spend nor income', () => {
    const zero = formatTransactionAmount('0')
    // Not "-$0.00": negating 0 gives -0, which String() renders as "0".
    expect(zero.text).toBe('$0.00')
    expect(zero.isSpend).toBe(false)
    expect(zero.isIncome).toBe(false)
  })

  it('honours a non-default currency', () => {
    expect(formatTransactionAmount('42.50', 'EUR').text).toBe('-€42.50')
  })
})

describe('isLiability', () => {
  it('is true for money owed', () => {
    expect(isLiability('credit')).toBe(true)
    expect(isLiability('loan')).toBe(true)
  })

  it('is false for money held', () => {
    expect(isLiability('depository')).toBe(false)
    expect(isLiability('investment')).toBe(false)
    expect(isLiability('brokerage')).toBe(false)
    expect(isLiability('other')).toBe(false)
    expect(isLiability('')).toBe(false)
  })

  it('matches the account type exactly', () => {
    // Plaid sends lowercase types. A near-miss must read as "not a liability"
    // rather than silently matching, so the failure is visible.
    expect(isLiability('Credit')).toBe(false)
    expect(isLiability('credit card')).toBe(false)
  })
})

describe('isAmortizingDebt', () => {
  it('is true only for loans', () => {
    // This picks the note rate over the disclosed APR. A card revolves and has
    // one rate; a mortgage has two, and labelling the wrong one moves a payoff
    // date by months.
    expect(isAmortizingDebt('loan')).toBe(true)
    expect(isAmortizingDebt('credit')).toBe(false)
    expect(isAmortizingDebt('depository')).toBe(false)
  })
})

describe('formatDate', () => {
  // The regression this function exists for. Postgres DATE serialises as
  // midnight UTC; `new Date(iso)` formatted anywhere west of UTC renders the
  // PREVIOUS day, which moves a first-of-the-month charge into the prior month
  // and therefore into the wrong monthly total.
  const ZONES = [
    'UTC',
    'America/Los_Angeles', // -07:00 / -08:00
    'America/New_York', // -04:00 / -05:00
    'Pacific/Honolulu', // -10:00, no DST
    'Europe/Berlin', // +01:00 / +02:00
    'Asia/Tokyo', // +09:00
    'Pacific/Kiritimati', // +14:00, the far edge
  ]

  it.each(ZONES)('renders the calendar date as written in %s', (zone) => {
    vi.stubEnv('TZ', zone)

    expect(formatDate('2026-07-01T00:00:00Z')).toBe('Jul 1, 2026')
    expect(formatDate('2026-01-01T00:00:00Z')).toBe('Jan 1, 2026')
    expect(formatDate('2026-12-31T00:00:00Z')).toBe('Dec 31, 2026')
  })

  it('does not shift the first of any month, in any zone', () => {
    for (const zone of ZONES) {
      vi.stubEnv('TZ', zone)
      for (let month = 1; month <= 12; month++) {
        const mm = String(month).padStart(2, '0')
        expect(formatDate(`2026-${mm}-01T00:00:00Z`)).toContain(', 2026')
        expect(formatDate(`2026-${mm}-01T00:00:00Z`)).toMatch(/ 1, 2026$/)
      }
    }
  })

  it('differs from the naive `new Date(iso)` west of UTC', () => {
    // Guards the fix itself, not just its output: if someone replaces the body
    // with `new Date(iso).toLocaleDateString(...)` the tests above would still
    // pass in UTC-only CI. This one states the trap outright.
    vi.stubEnv('TZ', 'America/Los_Angeles')

    const iso = '2026-07-01T00:00:00Z'
    const naive = new Date(iso).toLocaleDateString('en-US', {
      month: 'short',
      day: 'numeric',
      year: 'numeric',
    })

    expect(naive).toBe('Jun 30, 2026')
    expect(formatDate(iso)).toBe('Jul 1, 2026')
  })

  it('accepts a bare YYYY-MM-DD with no time part', () => {
    vi.stubEnv('TZ', 'America/Los_Angeles')
    expect(formatDate('2026-07-13')).toBe('Jul 13, 2026')
  })

  it('handles a leap day', () => {
    vi.stubEnv('TZ', 'Pacific/Kiritimati')
    expect(formatDate('2028-02-29T00:00:00Z')).toBe('Feb 29, 2028')
  })
})

describe('formatRelative', () => {
  // Unlike formatDate this really is a timestamp, so `new Date(iso)` is
  // correct here — the value is an instant, not a calendar day.
  const now = new Date('2026-07-13T12:00:00Z')

  const ago = (ms: number) => new Date(now.getTime() - ms).toISOString()

  it('renders a null timestamp as "never"', () => {
    expect(formatRelative(null)).toBe('never')
  })

  it('renders recent, minute, hour and day scales', () => {
    vi.useFakeTimers()
    vi.setSystemTime(now)

    expect(formatRelative(ago(5_000))).toBe('just now')
    expect(formatRelative(ago(90_000))).toBe('2m ago')
    expect(formatRelative(ago(45 * 60_000))).toBe('45m ago')
    expect(formatRelative(ago(3 * 3_600_000))).toBe('3h ago')
    expect(formatRelative(ago(50 * 3_600_000))).toBe('2d ago')
  })

  it('crosses from minutes to hours at an hour', () => {
    vi.useFakeTimers()
    vi.setSystemTime(now)

    expect(formatRelative(ago(59 * 60_000))).toBe('59m ago')
    expect(formatRelative(ago(60 * 60_000))).toBe('1h ago')
  })
})
