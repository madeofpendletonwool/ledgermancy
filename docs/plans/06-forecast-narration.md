# 06 — Forecast narration

## Context

Ledgermancy has a working, honest projection engine. `networth.Project`
(`backend/internal/networth/project.go`) rolls the current position forward under
explicit `Assumptions` (monthly surplus, annual return rate, annual debt
paydown, months) and returns `[]ProjectionPoint` — one month each, with
`NetWorth`, `Assets`, `Liabilities`, `Contributed`, and `Growth` kept separate so
saving is never confused with assumed return. The starting position comes from
`networth.Compute` (`backend/internal/networth/networth.go`, `Result` +
`Breakdown`). The endpoint `GET /api/networth/projection` →
`handleProjection` (`backend/internal/api/networth_handlers.go` ~line 322; routed
at `server.go` line 232) defaults the surplus to the trailing-12-month average
leftover and echoes every assumption back, always with `Estimate: true` and a
`Basis` caveat string. The Report page already renders three horizons in its
**Outlook** section (`frontend/src/routes/Report.tsx` ~line 191).

What is missing is **language**. The numbers are correct but terse — a table of
net-worth figures. This doc adds an AI narration layer that phrases the same,
already-computed projection ("at this savings rate you reach $X by March 2031;
trimming dining to your 6-month average would free about $Z/month"). The
arithmetic stays entirely in `networth.Project`; AI only turns finished numbers
into a sentence, and the "not a forecast" caveat travels with the text.

## AI vs deterministic split

| Concern | Owner |
| --- | --- |
| Starting balances / breakdown | **SQL + decimal** (`networth.Compute`) |
| Every projected month, growth vs contributed | **decimal** (`networth.Project`) |
| Default surplus (trailing-12-mo leftover) | **SQL** (`GetSpendingSummary`, in `handleProjection`) |
| "Free $Z/mo by trimming dining" figure | **SQL** — a computed dining-vs-average delta |
| Wording of the scenario, the encouragement | **AI** — phrasing only |

The model receives finished `StringFixed(2)` strings: the milestone net-worth
values, the month labels, the surplus, and any lever delta. It never compounds,
never picks the milestone month by arithmetic, never computes the dining
savings. If a number is not in the input, it may not appear in the output.

## Prerequisites

- **04 — insight engine** (`insights` table + `Producer` interface). The
  optional feed surface is a producer, kind `forecast`.
- The projection engine and endpoint (already shipped). No change to
  `networth.Project`'s signature or math.

## Data model

No new tables. The narration is derived on demand from projection output; if
surfaced as an insight it is an `insights` row (doc 04) with kind `forecast`.

`insights.data` shape for a `forecast` candidate (money as strings):

```json
{
  "milestones": [
    { "months": 12,  "month": "2027-07", "net_worth": "48250.00" },
    { "months": 60,  "month": "2031-07", "net_worth": "132900.00" },
    { "months": 120, "month": "2036-07", "net_worth": "310400.00" }
  ],
  "monthly_surplus": "1450.00",
  "annual_return_rate": "0.06",
  "levers": [
    { "label": "dining", "monthly_savings": "180.00",
      "basis": "current month vs your 6-month average" }
  ],
  "basis": "Straight-line projection from your current balances and trailing-12-month averages. Not a market forecast."
}
```

The `basis` string is copied verbatim from the projection response so the caveat
is stored *with* the data, mirroring how `handleProjection` puts `Estimate`/`Basis`
in the payload and how `Report.tsx` prints the caveat next to the numbers.

## Backend

- **Narration input builder** — a `buildForecastInput` helper alongside
  `buildSummaryInput` (`backend/internal/api/summary_handlers.go`), reusing that
  exact pattern (assemble strings from the reporting layer, hand them to AI):
  1. `networth.Compute` → current position.
  2. `networth.Project` with the same defaulted `Assumptions` `handleProjection`
     builds (do **not** duplicate the surplus math — factor the default-surplus
     computation out of `handleProjection` into a shared helper so endpoint and
     narrator agree exactly).
  3. Pick milestone indices `[11, 59, 119]` — the same horizons `Report.tsx`
     shows — and read `points[i].NetWorth` / `.Month`. No recomputation.
  4. **Lever deltas** in SQL: reuse `GetSpendingByCategory` for the current
     month and a 6-month window (both already exist and are used by
     `buildSummaryInput`); compute the discretionary category's
     (current − average) in decimal and pass the positive deltas as levers. This
     is the only new arithmetic and it stays in Go/decimal.
- **`ai.ForecastNarration(ctx, ForecastInput) (string, error)`** —
  `backend/internal/ai/forecast.go`, modeled on `MonthlySummary`
  (`backend/internal/ai/summary.go`): a strict system prompt, `MaxTokens` ~400,
  `ErrDisabled` when `!Enabled()`. The prompt lists the milestone lines, the
  surplus, and the levers, and asks for 2–3 sentences that **must** end by
  restating the caveat (or the endpoint keeps `Basis` adjacent — see Frontend).
- **Surface A — Report Outlook (primary)**: a new field on `projectionResponse`
  (`networth_handlers.go`), e.g. `Narrative *string`, populated only when
  `s.AI.Enabled()`; nil otherwise. `handleProjection` calls the narrator after
  building `Points`. A slow/failed AI call must **not** fail the endpoint — log
  and return `Narrative: nil` (the table still renders). Consider caching the
  text keyed on the assumption set to avoid a model call on every page load.
- **Surface B — insight feed (optional)**: a `forecast` `Producer`
  (`backend/internal/insights/forecast.go`) that raises one low-priority
  milestone insight per household on the doc-04 sweep, `DedupeKey`
  `"forecast:" + period` (monthly). Same input builder. This is for the proactive
  feed; the Report narrative is the main deliverable.

## Frontend

- **Report Outlook** (`frontend/src/routes/Report.tsx` ~line 191): when
  `projection.data.narrative` is present, render it as a paragraph **above** the
  horizon table, and keep the existing `projection.data.basis` caveat line
  exactly where it is. The narrative supplements, never replaces, the numbers or
  the caveat. When narrative is null (AI off), the section is unchanged from
  today.
- No new route needed for surface A. If surface B is built, `forecast` insights
  render in the doc-04 feed automatically.
- **Capabilities gating**: the narrative only fetches/shows when `ai_enabled`
  (`handleCapabilities`). The projection table and its caveat are always shown.

## AI notes

- System prompt rules (extend `summarySystemPrompt`'s spirit): "Use only the
  figures provided. These are illustrative projections, not predictions — never
  imply certainty about markets or the future. Quote amounts and dates exactly as
  given. 2–3 warm, plain sentences. End by reminding the reader this is an
  estimate that assumes things continue as they are."
- Input is decimal strings + month labels only. The model is told *which* number
  is the milestone and *which* is the lever saving; it does not choose them.
- Because `Project` is deterministic and cheap, regenerate the input fresh each
  time rather than caching numbers; cache only the model *text* if load warrants.

## Verification

- `docker compose up -d --build` (AI enabled, Plaid sandbox). Hit
  `GET /api/networth/projection` and confirm the response now carries a
  `narrative` string whose quoted figures match `points[11|59|119].net_worth`
  byte-for-byte.
- Cross-check in psql / by hand: recompute one milestone with the plain
  compounding formula in `project.go` and confirm the narrated figure equals it.
- Confirm the lever number equals the SQL (current-month dining) − (6-month
  average dining) delta.
- Unset the AI key → endpoint returns `narrative: null`, Report renders the table
  + caveat unchanged, no 5xx.
- Force an AI error (bad base URL) → endpoint still 200 with `narrative: null`
  (log only). Backend `go build/vet/test -p 1`; frontend `tsc/build/lint`.

## Out of scope

- Changing the projection math, adding Monte Carlo, or modeling market variance —
  `Project` stays a transparent straight line by design.
- Letting AI choose or invent assumptions; assumptions remain user-set /
  trailing-average and fully visible.
- Multi-scenario comparison UI (side-by-side "what if I save $X more"). The
  narrator may mention one lever; an interactive scenario builder is a later doc.
- Goal-linked coaching ("to hit your house down-payment goal…") — that is doc 11,
  which depends on this one.
