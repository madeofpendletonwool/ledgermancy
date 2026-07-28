# 27 — Inflation-adjusted (real) views

*(TODO.md "Next major initiatives" #12.)*

## Context

Every long-term trend in the app compares **nominal dollars across years**. "Net
worth up 8% this year" in a 6% inflation year is 2% real growth, and the app has
no way to say so.

This is the same class of arithmetic dishonesty the app already rejects
everywhere else — transfers counted as spending, monthly averages dividing by
months touched rather than elapsed. The README's "the numbers are honest" section
enumerates those fixes. Nominal-only long-horizon comparison belongs on that list
and is not on it.

Most users have no idea what inflation actually was in a given year or how it
compounds. Small amount of work, large honesty payoff, and a genuine educational
win.

**This doc is deliberately small and self-contained.** TODO #12 notes it
"arguably should land alongside" docs 14/15 rather than as a standalone project —
that is right, and it is a good candidate to bundle with either.

## AI vs deterministic split

No AI. Deflating a series by a price index is arithmetic.

## Prerequisites

None hard.

- **[15-fire-projections.md](15-fire-projections.md)** — 15 already stores
  `inflation_rate` in `projection_assumptions` and defaults to *real* returns.
  This doc supplies the actual CPI series behind that, making it switchable
  properly rather than an assumed constant. Coordinate: do not add a second
  inflation input.
- **[14-investments-page.md](14-investments-page.md)** — real returns on the
  performance view.

## Data model

**Reserved migration: `00030_cpi_series.sql`.**

```sql
-- CPI-U, monthly. Same shape as asset_prices (doc 14) and any future FX table
-- (doc 29) — keep the three consistent; they are the same kind of thing.
CREATE TABLE cpi_series (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    period     DATE NOT NULL UNIQUE,   -- first of the month
    index_value NUMERIC(12,4) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

Not household-scoped — CPI is a public series, identical for everyone.

## Backend

### Ingestion

A monthly job in `backend/internal/jobs/` pulling CPI-U from the BLS public API
(series `CUUR0000SA0`; the v1 endpoint needs no registration key for a limited
number of series and years, which is sufficient here).

Three requirements:

- **Ship a seed dataset** covering at least the last 15 years, committed as a
  fixture and loaded by the migration. CPI is small, slow-moving, and public. A
  self-hosted install with no outbound access must still get real deflation
  rather than a broken feature — and the job then only fetches the tail.
- **BLS publishes with a lag** (mid-following-month) and **revises**. Upsert on
  `period` rather than insert-only, and handle a missing most-recent month by
  falling back to the latest available, clearly labelled.
- Same outbound-call treatment as docs 14/25/26: config flag, and the README's
  "phones home to nothing but Plaid" line must be accurate. With the seed
  dataset the flag can default off and the feature still works, which is the
  better default.

### Deflation helper

One function in `backend/internal/reporting/`:

```go
// Real converts a nominal amount at `from` into `base`-dated dollars.
//   real = nominal × (index[base] / index[from])
func Real(nominal decimal.Decimal, from, base time.Time, series map[time.Time]decimal.Decimal) (decimal.Decimal, error)
```

Exact decimal. Return an error for a missing index rather than silently passing
the nominal figure through — a "real" number that is quietly nominal is the exact
dishonesty this doc exists to remove.

**Base period is today by default** ("in today's dollars"), and must be stated
wherever a real figure renders.

### Surfaces

Add a `real=true` parameter (or equivalent) to:

- Net-worth history.
- Long-horizon spending trends and annual totals.
- Investment performance (doc 14).
- FIRE projections (doc 15) — an explicit nominal/real toggle rather than
  presenting one as the truth.

**Do not change any default.** Nominal stays the default everywhere; real is
opt-in. Silently switching the meaning of existing figures would break every
comparison a user has in their head.

Short-horizon views (this month, this quarter) should not offer the toggle — a
one-month deflation is noise dressed as precision.

## Frontend

- A **"real (inflation-adjusted)" toggle** on qualifying charts. Persist the
  choice per user via the existing preferences store (doc 02).
- When real is active, label the axis and every headline figure: "in July 2026
  dollars." Not a footnote — the label is what makes the number honest.
- A Dashboard inflation strip: "inflation YTD is X%", contextualised against the
  household's own income and net-worth growth. The comparison is what makes it
  concrete rather than abstract trivia.
- Where the series is stale, say so rather than showing a gap.

## Verification

- `go test -p 1 ./...`.
- `Real()` against hand-computed fixtures: a known CPI pair, a same-period
  conversion (must be identity), and a missing index (must error, not
  pass through).
- Seed dataset loads and covers the documented span.
- Upsert handles a BLS revision to an existing month.
- **Default-unchanged assertion:** every existing endpoint returns byte-identical
  output with the feature deployed and `real` unset.
- A household with history predating the seed series degrades with a clear
  message rather than a wrong number.
- Frontend `tsc -b`, `vite build`, `oxlint`.

## Out of scope

- Regional or category-specific CPI (CPI-U national only).
- Personal inflation rate computed from the user's own basket. Interesting,
  and a much larger piece of work.
- Currency conversion (doc 29).
- Retroactively rewriting stored historical figures. Deflation is a view, applied
  at read time, always.
