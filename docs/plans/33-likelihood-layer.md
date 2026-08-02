# 33 — Likelihood layer (Monte Carlo, guardrail AI, plan tracking)

*(Wave 6 — closes the loop. Visual companion:
[advisor-overview.html](advisor-overview.html) §6 (success rate + fan chart)
and §8 (plan tracking). Builds on docs 32 and 24.)*

## Context

Doc 32's allocator projects a **deterministic P50** per bucket — one number
per outcome. That is honest but incomplete: "Plan A projects to $610k in 17
years" hides the question a real advisor actually gets asked — *"will this
work?"* The answer is a distribution, not a point. Plan A might meet the
target in 94% of plausible futures and Plan B in 71%, and that difference —
not the median — is what makes one plan the right pick.

`networth/montecarlo.go` already exists (gated behind
`RETIREMENT_MONTE_CARLO_ENABLED`, default off). It draws sequences around the
user's stated real return and a volatility they set, seeds deterministically
from the inputs, and names that basis in every response. This doc generalizes
it from the withdrawal phase to **allocation plans**, adds a documented
guardrail rule that lets the AI name a top pick from computed likelihoods (not
opinion), and adds **plan tracking** so an accepted plan is reconciled against
reality over time. The advisor becomes a relationship, not a calculator.

## AI vs deterministic split

**Deterministic:** the entire simulation, percentile computation, worst-case
drawdown, the guardrail rule, and the plan-vs-actual reconciler. Exact decimal
inputs; the simulation is seeded and reproducible.

**AI:** two narrow jobs. (1) Presentation — prose over computed percentiles.
(2) **Naming the top pick** under the documented guardrail rule — the model
receives each plan's computed (goal-fit, success rate, worst-case drawdown,
σ) plus the rule, and explains the pick in those terms. The rule is written
in a comment and in the system prompt; same inputs → same pick → same
explanation. The model never invents a likelihood, never reorders outside the
rule, never substitutes judgement for arithmetic.

## Prerequisites

- **[32-allocation-planner.md](32-allocation-planner.md)** — same wave. The
  MC runs over its allocation plans.
- **[15-fire-projections.md](15-fire-projections.md)** — shipped.
  `montecarlo.go` is the engine; do not fork it.
- **[31-advisor-surface.md](31-advisor-surface.md)** — `risk_drawdown_floor`
  on `households` is the personal layer of the guardrail rule.

## Data model

**Reserved migration: `00050_likelihood_layer.sql`.** Small — the simulation
is pure and results are never persisted (recomputed from the plan + a
deterministic seed).

```sql
-- Plan-vs-actual reconciliation. One row per (plan, period). "Period" is the
-- snapshot date; drift is computed at read time from actuals, not stored, so
-- the comparison stays honest as the underlying transactions are edited.
CREATE TABLE plan_trackings (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    plan_id     UUID NOT NULL REFERENCES allocation_plans (id) ON DELETE CASCADE,
    as_of       DATE NOT NULL,
    -- expected is the plan's projected position at as_of (deterministic, from doc 32)
    expected_lump   NUMERIC(20,4) NOT NULL,
    expected_total  NUMERIC(20,4) NOT NULL,
    snapshot_inputs JSONB NOT NULL,       -- the live baseline captured at this as_of
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (plan_id, as_of)
);
CREATE INDEX plan_trackings_plan_idx ON plan_trackings (plan_id, as_of DESC);
```

Actuals are **never stored** here — they are read live from
`investment_transactions`, `account_balance_history`, and `transactions` at
read time, so editing a past contribution corrects the drift without a
migration. `expected_*` is stored because recomputing it requires the plan's
inputs at `as_of`, which the engine can replay (doc 32's determinism).

## Continuity

| Table | Category | Rationale |
|---|---|---|
| `plan_trackings` | `InExport` | User's tracked decisions; `expected_*` money cast to text |

No new blob stores or volumes.

## Backend

### New package `backend/internal/likelihood/`

**Simulation** — `Run(plan, assumptions, n)` generalizes
`montecarlo.go`'s withdrawal-phase loop to an *accumulation*-phase allocation
plan. Per bucket: sample annual returns from `N(μ, σ)` where μ is the bucket's
assumed real return and σ its volatility; debt and HYSA buckets have σ=0
(guaranteed). Apply the plan's lump at t=0 and monthly contributions each
period. Returns the per-bucket and total terminal distribution across `n`
runs. **Deterministic seed from the inputs** — same plan, same assumptions,
same `n` → identical distribution. Caching is in-memory only; never persist
(doc 28's rule, inherited).

**Percentiles + drawdown** — from the sorted terminal values, compute P10 /
P50 / P90 and the success rate (fraction of runs ≥ the plan's target).
Compute worst-case peak-to-trough drawdown across the run paths at the
household level. Every response names its basis ("1000 sims, μ and σ as set,
seeded from inputs; not a prediction").

**Guardrail rule** — written in a comment at the top of `rank.go` and in the
system prompt verbatim:

```
Given a set of allocation plans the user is comparing, the top pick is the
plan that, in order:
  1. Meets every stated goal (each goal's funded % ≥ 100 at horizon), AND
  2. Among those, has the highest success rate (P(terminal ≥ target)), AND
  3. Whose worst-case drawdown ≤ household.risk_drawdown_floor (else
     it fails the guardrail and is excluded), AND
  4. Break ties by lower σ.
If no plan meets all goals, the top pick is the highest-success-rate plan
that meets the drawdown floor, and the AI says so explicitly.
```

The rule is deterministic. The AI's only freedom is phrasing — and even that
is constrained to cite the computed figures.

**Plan tracking** — `Reconcile(plan, asOf)` reads the plan's expected position
(replayed from inputs), reads actual contributions per bucket from
`investment_transactions`/`account_balance_history`/`transactions` over the
period, and reports per-bucket drift and a re-projected position against the
live baseline. Writes a `plan_trackings` row with the snapshot; drift is
computed at read. Surfaced as an insight when drift crosses a threshold
("you're $180/mo behind your Plan A — on track to land $14k short at
horizon").

### Chat tools — `chat_handlers.go`

| Tool | Purpose |
|---|---|
| `plan_likelihood` | Run the MC for one plan; return P10/P50/P90, success rate, worst-case drawdown. |
| `compare_plans` | Run the guardrail over a set of plan ids; return each plan's figures + the rule's pick. |
| `plan_tracker` | Drift report for an accepted plan; on-track / behind / re-projected. |

`retirement_stress` (doc 31) shares the same MC path for sequence-of-returns
risk in the withdrawal phase.

### Handlers — `internal/api/likelihood_handlers.go`

```
POST   /api/likelihood/plan/{id}          handleLikelihood       // run MC
POST   /api/likelihood/compare            handleCompare          // guardrail over a set
GET    /api/likelihood/plans/{id}/track   handleTracking         // drift history
```

All `authenticate` + `RequireAdult`, household-scoped. MC is gated behind
`RETIREMENT_MONTE_CARLO_ENABLED` for now (matches doc 15's gate); with it
off, the endpoints return the deterministic P50 only and name the basis
accordingly.

## Frontend

- **Success-rate headline** — the largest number on a plan card. "94% chance
  to meet your target."
- **Fan chart** — shaded P10–P90 band with P50 line and the target line in
  ember. Lives on the allocator output (doc 32's section 03).
- **Plan comparison view** — side-by-side cards for 2–4 saved plans; each
  shows goal-fit, success rate, worst-case drawdown, σ; the guardrail's pick
  is flagged with the rule cited ("top pick: meets all goals, highest success
  rate, within your 25% drawdown floor").
- **Plan tracker panel** — for an accepted plan: a drift sparkline, the
  "behind / on track" status, and a one-line re-projection. Closes the loop.

The framing is permanent and prominent: **these are computed projections
under stated assumptions, not advice and not predictions.** (Doc 28's rule,
inherited.)

## AI notes

The guardrail rule appears verbatim in the system prompt. The model is
forbidden from naming a top pick without citing the computed figures that
place it there, and forbidden from recommending a plan that fails the
drawdown floor. With AI disabled, `compare_plans` returns the figures and the
rule's pick as a plain labelled list — the feature works with no key.

## Verification

Decimal-exact inputs, reproducible simulation.

- **Determinism:** same plan + assumptions + `n` → identical sorted terminal
  values (assert byte-equal after rounding). Same seed → same result.
- **Baseline-vs-baseline:** an empty plan produces zero variance; P10 = P50 =
  P90 = current balance.
- **Guardrail determinism:** two runs over the same plan set name the same
  pick with the same cited figures.
- **Drawdown floor:** a plan whose worst-case drawdown exceeds
  `risk_drawdown_floor` is excluded from the pick and flagged, even if its
  success rate is highest.
- **Plan tracking drift:** seed a plan, post actual contributions via doc 30's
  scheduled path, assert the drift matches hand-computed expected − actual;
  editing a past contribution corrects the drift without a migration.
- **No real-data mutation:** snapshot every table before/after a likelihood
  run; assert identical.
- **`ErrDisabled` / gate off:** with MC gated off, `plan_likelihood` returns
  the deterministic P50 only and names the basis; no error.
- **Build:** `go build ./... && go vet ./... && go test -p 1 ./...` with
  `TEST_DATABASE_URL`. Frontend `tsc -b && vite build && oxlint`.

## Out of scope

- Claiming prediction. The MC is illustrative under stated assumptions;
  percentiles, never a single forecast number presented as fate.
- Auto-generating a plan to optimize. The guardrail picks among plans the user
  is comparing; it does not synthesise one.
- Historical backtesting as ground truth. When a historical return series is
  bundled (a later, opt-in switch — privacy contract per doc 30), it seeds
  the distribution; until then μ and σ are user-set. The basis is always
  named.
- Tax-drag modelling inside the simulation. Withdrawal-phase tax drag is a
  doc 15 omission inherited here; called out in the UI.
