import { afterEach, describe, expect, it, vi } from 'vitest'

import {
  WINDOWS,
  defaultRange,
  iso,
  matchedWindow,
  monthsInRange,
  windowRange,
} from './period'

afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllEnvs()
})

/** Freezes the clock at a wall-clock moment in a named zone. */
function at(zone: string, ...parts: [number, number, number, number?, number?]) {
  vi.stubEnv('TZ', zone)
  const [year, month, day, hour = 12, minute = 0] = parts
  vi.useFakeTimers()
  // Constructed after the TZ stub, so the fields are read as local time in
  // `zone` — which is the whole point of the boundary cases below.
  vi.setSystemTime(new Date(year, month - 1, day, hour, minute))
}

describe('iso', () => {
  it('zero-pads month and day', () => {
    expect(iso(new Date(2026, 0, 5))).toBe('2026-01-05')
    expect(iso(new Date(2026, 11, 31))).toBe('2026-12-31')
  })

  it('reads the LOCAL calendar date, not the UTC one', () => {
    // East of UTC, local midnight is the previous day in UTC, so a
    // toISOString().slice(0, 10) implementation returns the wrong day and
    // silently shifts a month boundary. Both zones must agree here.
    vi.stubEnv('TZ', 'Europe/Berlin')
    const berlinMidnight = new Date(2026, 6, 1, 0, 0)
    expect(berlinMidnight.toISOString().slice(0, 10)).toBe('2026-06-30')
    expect(iso(berlinMidnight)).toBe('2026-07-01')

    vi.stubEnv('TZ', 'America/Los_Angeles')
    const laLateNight = new Date(2026, 6, 31, 23, 30)
    expect(laLateNight.toISOString().slice(0, 10)).toBe('2026-08-01')
    expect(iso(laLateNight)).toBe('2026-07-31')
  })
})

describe('windowRange', () => {
  it('spans whole calendar months ending with the current one', () => {
    at('America/Los_Angeles', 2026, 7, 15)

    expect(windowRange(1)).toEqual({ from: '2026-07-01', to: '2026-07-31' })
    expect(windowRange(3)).toEqual({ from: '2026-05-01', to: '2026-07-31' })
    expect(windowRange(12)).toEqual({ from: '2025-08-01', to: '2026-07-31' })
    expect(windowRange(24)).toEqual({ from: '2024-08-01', to: '2026-07-31' })
  })

  it('walks back across a year boundary', () => {
    at('America/Los_Angeles', 2026, 1, 10)

    expect(windowRange(3)).toEqual({ from: '2025-11-01', to: '2026-01-31' })
    expect(windowRange(12)).toEqual({ from: '2025-02-01', to: '2026-01-31' })
  })

  it('ends on the real last day of a short month', () => {
    at('America/Los_Angeles', 2026, 9, 5)
    expect(windowRange(1).to).toBe('2026-09-30')

    at('America/Los_Angeles', 2027, 2, 5)
    expect(windowRange(1).to).toBe('2027-02-28')

    at('America/Los_Angeles', 2028, 2, 5)
    expect(windowRange(1).to).toBe('2028-02-29')
  })

  it('uses the local month at a moment when UTC has already rolled over', () => {
    // 23:30 on the last day of July in Los Angeles is already 1 August in UTC.
    // The window must still be July's, or the last night of the month reports
    // against a month the user has not reached.
    at('America/Los_Angeles', 2026, 7, 31, 23, 30)
    expect(windowRange(1)).toEqual({ from: '2026-07-01', to: '2026-07-31' })
  })

  it('uses the local month at a moment when UTC has not yet rolled over', () => {
    // The mirror image: 00:30 on 1 July in Berlin is still 30 June in UTC.
    at('Europe/Berlin', 2026, 7, 1, 0, 30)
    expect(windowRange(1)).toEqual({ from: '2026-07-01', to: '2026-07-31' })
  })

  it('survives a DST transition inside the window', () => {
    // US DST ends 1 November 2026. A range built by adding milliseconds would
    // drift an hour here and land on the wrong day; calendar arithmetic cannot.
    at('America/Los_Angeles', 2026, 11, 15)
    expect(windowRange(3)).toEqual({ from: '2026-09-01', to: '2026-11-30' })
  })
})

describe('defaultRange', () => {
  it('is the trailing twelve months', () => {
    at('America/Los_Angeles', 2026, 7, 15)
    expect(defaultRange()).toEqual(windowRange(12))
    expect(monthsInRange(defaultRange().from, defaultRange().to)).toBe(12)
  })
})

describe('monthsInRange', () => {
  it('counts months inclusively', () => {
    expect(monthsInRange('2026-07-01', '2026-07-31')).toBe(1)
    expect(monthsInRange('2026-05-01', '2026-07-31')).toBe(3)
    expect(monthsInRange('2026-01-01', '2026-12-31')).toBe(12)
  })

  it('counts across a year boundary', () => {
    expect(monthsInRange('2025-08-01', '2026-07-31')).toBe(12)
    expect(monthsInRange('2024-08-01', '2026-07-31')).toBe(24)
  })

  it('floors at one month', () => {
    // The result divides per-month averages, so zero or a negative would
    // produce Infinity or a sign flip on screen.
    expect(monthsInRange('2026-07-31', '2026-05-01')).toBe(1)
    expect(monthsInRange('', '')).toBe(1)
    expect(monthsInRange('not-a-date', '2026-07-31')).toBe(1)
  })

  it('ignores the day of month', () => {
    expect(monthsInRange('2026-05-31', '2026-07-01')).toBe(3)
  })
})

describe('matchedWindow', () => {
  it('round-trips every preset window', () => {
    at('America/Los_Angeles', 2026, 7, 15)

    for (const window of WINDOWS) {
      const { from, to } = windowRange(window.months)
      expect(matchedWindow(from, to)).toBe(window.months)
    }
  })

  it('falls back to twelve months for a span with no preset', () => {
    expect(matchedWindow('2026-03-01', '2026-07-31')).toBe(12)
    expect(matchedWindow('', '')).toBe(12)
  })
})
