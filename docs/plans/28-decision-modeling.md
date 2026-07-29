# 28 — Decision modeling: the what-if scenario engine

*(TODO.md "Next major initiatives" #16. The capstone — read the TODO entry in
full before starting; it is the most detailed in the backlog.)*

## Context

Every adult faces a handful of financial decisions that genuinely change their
trajectory: which house to buy, pay off debt or invest, which job offer to take,
when they can retire, whether they can afford a kid or a sabbatical.

These are six-figure decisions served badly by everything that exists.
Spreadsheets are too tedious to maintain. Online calculators use rules of thumb
and know nothing about you. The math is too interleaved — opportunity cost, tax
treatment, sequence-of-returns risk, cash flow — to do by hand.

Once docs 13, 14, 15, 23, 26, and 27 have landed, this app holds every input
needed to compute these answers **for the actual user** rather than a generic
persona. That is the payoff this feature captures, and it is the reason it comes
last.

## Shape: an engine, not a pile of calculators

A **scenario** is a clone of the household's current state — income (23),
obligations (13), debts, assets (26), investments (14), goals, contributions —
with any input overridden. Doc 15's projection machinery runs against the
modified state over a chosen horizon. Output is **side-by-side baseline vs.
scenario**: net worth, FI age, monthly cash flow, tax picture, goal completion
dates.

Scenarios are saved, named, and comparable against each other. Each carries its
explicit assumptions so the user can interrogate the result rather than trust a
black box.

If you find yourself writing a second projection loop, stop — you are
re-implementing doc 15 and it will diverge.

## AI vs deterministic split

**Deterministic:** the entire engine. Every projection, breakeven, survival rate,
and comparison, in exact decimal.

**AI:** two narrow jobs.

1. **Discovery** — noticing which scenario is worth running for this household
   ("your HYSA at 4.5% exceeds your card balance at 22% — want me to model
   paying it off?"). This is where the model genuinely earns its keep, because
   most people do not know which question to ask.
2. **Presentation** — prose over computed results, `chat_handlers.go` pattern.

The model never computes a scenario outcome. Someone will make an irreversible
six-figure decision on this output.

## Prerequisites

**Hard: [15-fire-projections.md](15-fire-projections.md).** TODO #16 is explicit —
"this initiative must land after #5 ships. Building it before means
re-implementing projection logic that will be thrown away." Do not start the
engine before 15's `ProjectByAccount` exists.

**Strong (each unlocks scenario families):**

- [13-bill-calendar.md](13-bill-calendar.md) — cash-flow impact of purchases.
- [23-paystub-income.md](23-paystub-income.md) — tax math and contribution
  headroom; without it, job-offer and move scenarios are guesses.
- [26-real-asset-revaluation.md](26-real-asset-revaluation.md) — accurate asset
  inputs.
- [27-inflation-adjusted-views.md](27-inflation-adjusted-views.md) — honest
  long-horizon projections.
- [24-proactive-advisor.md](24-proactive-advisor.md) — becomes the discovery
  layer.

**Scope realistically.** This is the largest item in the backlog. Ship the engine
plus **one or two** scenario families first (major purchase and retirement stress
tests are the highest value), then add families incrementally. Attempting all
five at once is how this stalls.

## Data model

**Reserved migration: `00041_scenarios.sql`.**

```sql
CREATE TABLE scenarios (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id UUID NOT NULL REFERENCES households (id) ON DELETE CASCADE,
    created_by   UUID REFERENCES users (id) ON DELETE SET NULL,
    name         TEXT NOT NULL,
    family       TEXT NOT NULL CHECK (family IN (
                     'major_purchase','retirement_stress','life_event',
                     'job_offer','goal_solve','debt_vs_invest','windfall',
                     'insurance_needs')),
    -- The overrides applied to the baseline. Schema-versioned, because a saved
    -- scenario must still open after the engine's inputs evolve.
    inputs        JSONB NOT NULL,
    input_version INT   NOT NULL DEFAULT 1,
    -- Assumptions AT SAVE TIME, snapshotted. A scenario compared against
    -- different assumptions than it was built with is meaningless.
    assumptions   JSONB NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX scenarios_household_idx ON scenarios (household_id);
```

**Results are not stored.** They are recomputed from `inputs` + `assumptions`
against the current baseline, so a scenario stays meaningful as the household's
real position changes — that is the point of comparing against a *live* baseline.
Cache in memory if it is slow; do not persist.

**`input_version` is load-bearing.** These are saved for years across engine
changes. Version from day one and migrate forward explicitly.

## Backend

New package `backend/internal/scenarios/`.

### Core

- **Baseline assembly** — gather current state into one struct. One function,
  reused by every family.
- **Override application** — pure, no side effects. A scenario must never mutate
  real data. Enforce this with types if possible; it is the most dangerous bug
  available here.
- **Projection** — delegate to doc 15. Do not fork it.
- **Comparison** — baseline vs. scenario across net worth, FI age, cash flow,
  goal dates, and where 23 has landed, tax.

### Scenario families, in priority order

1. **Major purchase with opportunity cost.** The universal decision. Direct cost
   (payment, total interest, property tax + insurance + maintenance, or the
   depreciation curve from 26), cash-flow impact against 13's calendar, and — the
   part nobody does — **the invest-the-difference comparison**: rent + invest the
   down payment and monthly delta at an assumed real return vs. own. Both paths
   on one net-worth chart, with FI-age delta and breakeven year.
2. **Retirement stress tests.** Sequence-of-returns risk (retire into a year-one
   drop), return-rate sensitivity, Social Security cuts, inflation surprises.
   Reuse doc 15's Monte Carlo — same deterministic seeding requirement.
3. **Goal acceleration (solve-for-X).** Inverts the question: "retire 5 years
   earlier — solve for savings rate." Bisection over the engine, bounded, with an
   explicit "not reachable" result.
4. **Job offer comparison.** Total comp over a horizon: salary, match with
   vesting, bonus, RSU/option grants with vesting and an assumed stock path,
   benefits value, HSA. Needs 23's tax math to be honest.
5. **Life-event impact.** Kid (regional childcare cost — use a dataset, not a
   national average), spouse stops working, sabbatical, aging parent, relocation.

Lower priority, same engine: debt-vs-invest, windfall allocation, insurance needs.

### Discovery

Extend doc 24's advisor to propose scenarios from actual state. Deterministic
trigger conditions (HYSA balance > card balance and APR spread > X; 401k YTD well
below limit with N periods left); the model only phrases the offer.

## Frontend

A **Scenarios** route.

- Family picker → a form scoped to that family.
- **Side-by-side comparison** as the primary output: baseline and scenario on
  shared axes, with the deltas that matter called out (FI age, net worth at
  horizon, monthly cash flow).
- **Assumptions always visible and editable**, per doc 15's rule.
- Save, name, list, and compare saved scenarios.
- Prominent, permanent framing: **these are computed projections under stated
  assumptions, not advice and not predictions.**

## Verification

- `go test -p 1 ./...`.
- **A scenario run must not mutate any real data.** Snapshot every relevant table
  before and after, assert identical. Highest-priority test in the doc.
- Baseline-vs-baseline (empty overrides) produces zero delta across every output.
  If it does not, the engine is not deterministic and nothing else is meaningful.
- Rent-vs-buy against a hand-computed fixture: assert breakeven year and terminal
  net worth exactly.
- Solve-for-X bisection converges; unreachable targets return "not reachable"
  and do not loop.
- Monte Carlo: same seed → identical result.
- `input_version` round-trip: a v1 scenario still opens after a v2 change.
- Frontend `tsc -b`, `vite build`, `oxlint`.

## Out of scope

- **Tax preparation.** Scenarios surface tax *implications*; they do not file,
  generate forms, or give CPA advice. Same line doc 23 holds.
- **Financial advice.** The engine computes outcomes and tradeoffs. It does not
  recommend. The user decides.
- **Real-time market data.** Assumed real returns the user sets. Doc 14's
  `asset_prices` is for historical reporting, not forward modelling.
- Optimising across scenarios automatically.
