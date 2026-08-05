# 27 — Inflation-adjusted (real) views

*(TODO.md "Next major initiatives" #12.)*

> **Shipped.** What follows is the plan as written. Five things came out
> differently, and the reasons are worth more than the plan text below —
> see **[Shipped notes](#shipped-notes)** at the end before touching this area.
>
> The short version: the migration is `00052`, not `00051` and not the `00057`
> the reservation table named; **the CPI series has a permanent hole in the
> middle of it** (October 2025), not just at the tail; investment returns are
> deflated but investment *dollar* figures are not; the FIRE page gained a
> measured-inflation figure to adopt rather than a nominal/real toggle; and the
> real toggle lives on a user preference so two people sharing a ledger can read
> the same chart differently.

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

**Reserved migration: `00040_cpi_series.sql`.**

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

## Shipped notes

Five departures from the plan above, in descending order of how far they reach.

### 1. The series has a hole in the middle, and it is permanent

The plan anticipated one failure mode — "handle a missing most-recent month" —
and that is not the one the real series has. **BLS never published October 2025**
and has said it will not estimate it after the fact: the 2025 lapse in
appropriations stopped collection. The API returns that month with a value of
`-` and a footnote saying so.

So `cpi_series` covers January 2010 to the present with exactly one interior
gap, and **anything reading this series must handle a hole, not just a short
tail.** Concretely:

- `reporting.Real` returns `ErrNoIndex` for a figure dated in that month. It
  does not interpolate. September 2025 and November 2025 differ by 0.2%, so
  interpolating would have been *within noise* — which is exactly the reasoning
  this app rejects everywhere else, and rejecting it here cost nothing.
- The `real=1` endpoints return an **absent** field for such a point, never the
  nominal figure under a real name. The clients drop the point and say how many
  they dropped and why.
- `/api/inflation` publishes `gaps` so a client can name the months rather than
  drawing a line through them.

`CPISeries.Gaps()` exists for this. Do not "fix" it by filling the hole.

### 2. Migration `00052`, and the reservation table was wrong in an instructive way

Doc 27 was reserved `00057`, which sits **above** wave 6/7's `00052`–`00056`.
Doc 27 is wave 5, and wave 5 ships first — so taking `00057` would have pushed
the schema past all five of those reservations and voided every one of them
under goose's strict ordering. It took `00052`; the wave 6/7 rows each moved up
one, and `docs/plans/README.md` has been updated.

The general lesson, now recorded there: **a reservation above an unshipped one
is not a reservation.** Allocate in ship order, or do not allocate.

### 3. Investment RETURNS are deflated; investment DOLLARS are not

The plan said "investment performance" without saying which figures. Returns are
deflated — TWR, its annualised form, and the money-weighted IRR. `start_value`,
`net_flows` and `gain` are **not**, and this is deliberate rather than
unfinished: deflating a period's cash flows correctly needs each flow converted
on its own date, and converting them from the span's endpoints would produce a
figure that looks precise and is not. A ratio of two index values converts a
return exactly; it does not convert a stream of dated flows. The response's
`real.note` says this, and the page renders it.

One consequence worth knowing: **MWR is deflated by the ANNUALISED price change,
not the total**, because MWR arrives already annualised. Deflating an annualised
return by a five-year total would understate it by the entire compounding of the
span — 16% instead of 3%. `RealReturns.AnnualInflation` is that figure, and it is
null under a year, which is why real MWR is absent on short spans.

### 4. FIRE got a measured rate to adopt, not a nominal toggle

The plan asked for "an explicit nominal/real toggle" on doc 15's projections.
That was not built, and the reason is that doc 15 does not have the problem the
bullet describes. Its every figure is already real, and it says so at the top of
the page in `realBasis` — it is not "presenting one as the truth", it is stating
which one it is. Adding a nominal view would have meant inflating a
thirty-year projection forward by an assumed rate, whose entire visible effect is
to make the numbers larger.

What the plan's *other* sentence about doc 15 asked for was built, and it is the
valuable half: "making it switchable properly rather than an assumed constant."
`GET /api/projections/assumptions` now returns `measured_inflation` — what CPI-U
actually did over the trailing decade, compounded — beside the household's
assumed rate, with a button to adopt it. It is **shown, never applied**:
`projection_assumptions.inflation_rate` remains the only inflation input in the
app, per this doc's own "do not add a second inflation input".

If a nominal projection is wanted later, it is a display transform over
`RetirementProjection` and needs no new stored input.

### 5. The toggle is a user preference, and short windows do not get one

`views.real`, user-scoped, defaulting false. User-scoped because it is a reading
preference rather than a fact about the household's money — two people sharing a
ledger can disagree about it harmlessly, which is not true of anomaly
sensitivity.

The plan's "short-horizon views should not offer the toggle" is enforced by
publishing `min_span_months` (12) from `/api/inflation` and having the clients
hide the control below it, rather than by rejecting the parameter server-side. A
`real=1` on a one-month window is answered honestly; it is simply not *offered*,
because deflating one month by one month's price change moves the figure by a
couple of tenths of a percent and invites a conclusion the data cannot support.

### Also worth knowing

- **The series is NSA** (`CUUR0000SA0`), not seasonally adjusted. The SA series
  is revised annually for five years running as BLS re-estimates seasonal
  factors, which would mean a deflated figure a user saw last month quietly
  changing. NSA is the published index of record and does not move once released.
- **The fetch job is off by default and the feature still works**, because the
  series ships seeded from January 2010 — sixteen years, well past the fifteen
  this doc asked for, and past anything a Plaid-backed history can reach. This
  is the reason `CPI_FETCH_ENABLED` can default off without the README's
  "phones home to nothing but Plaid" line becoming a half-truth.
- **The base period is the newest published month, not today.** BLS publishes
  mid-following-month, so the current month never has an index. Every real figure
  is labelled "in June 2026 dollars" rather than "in today's dollars", which is
  both accurate and stops the label from silently overstating how fresh the
  series is.
- `cpi_series` is classified `DumpOnly` in `continuity/coverage.go` — public
  reference data, identical in every install, so it rides the dump but stays out
  of the portable export. Same call as `savings_bond_rates`.
