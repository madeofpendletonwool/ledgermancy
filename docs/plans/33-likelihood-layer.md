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
receives each plan's computed (goal-fit, success rate, P5 drawdown,
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
- **[30-manual-accounts.md](30-manual-accounts.md)** — wave 5, lands first.
  **This was missing from the first draft and it is a hard dependency:**
  `Reconcile` reads actual contributions from `account_balance_history`, which
  is doc 30's table (`00047_manual_accounts.sql`), and the drift test posts
  actuals through doc 30's scheduled auto-post path. Without it, plan tracking
  can only see Plaid-linked accounts — which is precisely the case where a
  manual Voya account holds the contributions being tracked.

## Data model

**Reserved migration: `00054_likelihood_layer.sql`.** (Wave 5 ships first and
holds `00047`–`00051`; docs 31 and 32 hold `00052` and `00053`. This doc
originally reserved `00050`, which collided with wave 5's un-numbered
reissues — the README's reservation table now assigns all of them.) Small — the
simulation is pure and results are never persisted (recomputed from the plan +
a deterministic seed).

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
    -- Money inside this blob is a StringFixed(2) STRING, never a JSON number:
    -- export.go casts numeric COLUMNS to text but passes jsonb through as
    -- json.RawMessage, so the continuity guarantee does not reach inside.
    -- Doc 32's "Money in JSONB" section owns the rule; this table follows it.
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

Reuse `montecarlo.go`'s machinery rather than its loop: `monteCarloSeed`,
`seedBytes`, the ChaCha8 generator choice, `ClampRuns` and the
basis-sentence-travels-with-every-figure convention are all directly
applicable. The withdrawal loop itself is not — this is accumulation, with
contributions going in rather than draws coming out, per bucket rather than one
balance. Call it a shared foundation, not a generalization; the first draft's
"do not fork it" is right about the helpers and optimistic about the loop.

### Buckets are not independent, and pretending they are inflates every headline

Sampling each bucket from its own `N(μ, σ)` independently is the default a
reader of the first draft would implement, and it is wrong in the direction
that makes plans look good. A Roth, a 529 and a taxable brokerage in a typical
household are all holding equities — largely the *same* equities. Drawn
independently they diversify against each other in the model in a way they do
not in life: total variance falls by roughly `1/k` for `k` similar buckets, the
distribution narrows, and the success rate — the largest number on the page —
comes out materially too high.

So: **one market draw per year, shared across buckets, scaled per bucket.**

```
z_t     ~ N(0,1)                      -- ONE draw per year, all buckets
r_bucket = μ_bucket + σ_bucket · z_t   -- fully correlated across risky buckets
```

σ=0 buckets (debt, HYSA) are unaffected, which is correct — a card's APR does
not move with the market. This assumes **perfect correlation** among risky
buckets, which is not true either, but it errs conservative: it understates
diversification rather than overstating it, and for a household whose buckets
really are all US equity index funds it is close to right. A per-bucket
correlation matrix is out of scope — it needs a covariance estimate the app
has no honest source for, exactly like the return series doc 15 refused to
bundle.

**Name the assumption in the basis string**, beside the existing sentence:
"bucket returns are modelled as moving together, which understates the benefit
of holding genuinely different asset classes."

### Two P50s, and why they disagree

Doc 32 renders a deterministic compound-at-μ figure. This doc renders the
median of the simulation. **They are different statistics and they do not
match.** Compounding i.i.d. `N(μ, σ)` draws gives a median terminal value below
the compound-at-μ result by roughly `e^(−Tσ²/2)` — at μ=7%, σ=15%, T=17 years,
about **15% lower**. Doc 33's fan chart renders on doc 32's allocator output, so
both numbers land on the same card, and a 15% gap between two things both
labelled "P50" is exactly the failure doc 24 names: *two surfaces disagreeing
is worse than neither existing.*

This is not a bug to fix by fudging one of them. Both are correct answers to
different questions, and the resolution is stated convention plus a test:

- **μ is the arithmetic mean of the annual return draw.** Say so where the user
  enters it, because most published "expected real return" figures are
  geometric (CAGR), and entering a CAGR as μ overstates results by ~σ²/2 per
  year. The assumptions panel says: "the average of yearly returns, not a
  long-run compound average — for a 7% compound expectation at 15% volatility,
  enter about 8.1%."
- **The two figures are labelled differently and never both called P50.**
  Doc 32's is "projected at your assumed return"; this doc's is "median
  simulated outcome". The card shows both with one line explaining the gap:
  "the median simulated outcome is lower because volatility drags compounding —
  that difference is the cost of the risk you're taking."
- **A σ=0 plan makes them identical**, and that is the agreement test (below).
  It is the only case where they *must* match, and it is a real check that the
  two engines share arithmetic rather than having drifted.

**Percentiles + drawdown** — from the sorted terminal values, compute P10 /
P50 / P90 and the success rate (fraction of runs ≥ the plan's target). Every
response names its basis ("1000 sims, μ and σ as set, buckets modelled as
correlated, seeded from inputs; not a prediction").

**Drawdown is a percentile, not a maximum.** The first draft specified
"worst-case peak-to-trough drawdown across the run paths", and a maximum over
`n` runs is an extreme order statistic: it gets monotonically worse as `n`
grows. Since `ClampRuns` admits anything from 100 to 10,000
(`montecarlo.go:210`), raising the run count would make plans look riskier and
**could flip which plan the guardrail picks** — while this doc promises the
pick is deterministic. Strictly the promise holds (same `n` → same answer), but
that is not how anyone reads it.

Use the **5th-percentile peak-to-trough drawdown** across run paths — a stable
statistic that converges as `n` grows instead of diverging — and **pin `n` for
any comparison**: `compare_plans` runs every plan at one `n`, and a comparison
across differing `n` is refused rather than rendered. The response names the
percentile and the run count, so "25% drawdown" is never ambiguous about which
of a thousand futures it describes.

**Guardrail rule** — written in a comment at the top of `rank.go` and in the
system prompt verbatim:

```
Given a set of allocation plans the user is comparing, all simulated at the
same run count n:

FILTER, in this order. Each step names why a plan was dropped.
  F1. Drop any plan whose 5th-percentile peak-to-trough drawdown exceeds
      household.risk_drawdown_floor. Failing the floor is disqualifying, not
      a penalty -- a plan the household cannot sit through is not a plan.
      If risk_drawdown_floor is NULL, F1 is skipped and the answer says so.
  F2. Of the survivors, keep those meeting every stated goal, where "meets"
      is funded % >= 100 at horizon AT THE MEDIAN SIMULATED OUTCOME (P50),
      not at doc 32's compound-at-mu figure and not at P10.
  F3. If F2 leaves nothing, fall back to the F1 survivors and mark the
      result "no plan meets every goal" -- which the AI must state.

SORT the surviving set, first key first:
  S1. Success rate, P(terminal >= target), DESCENDING.
  S2. Sigma of the terminal distribution, ASCENDING (less spread wins).
  S3. Plan name, ASCENDING -- so the order is TOTAL and two plans that tie
      on every computed figure still produce one stable pick.

The top pick is the first plan after sorting. If the filtered set is empty
after F1, there is NO top pick: every plan breaches the drawdown floor, and
that -- not a least-bad choice -- is the answer.
```

**This replaces the first draft's four AND-joined clauses.** Those read as a
conjunction but meant a filter and a sort, they left it ambiguous whether
"meets every goal" was judged on the deterministic figure or on the
distribution, and they had no total-order tiebreak — three ways for two
implementers to build two different rules from one paragraph. That matters more
here than anywhere else in the wave, because the rule's entire claim is that it
is deterministic and quotable.

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
| `plan_likelihood` | Run the MC for one plan; return P10/P50/P90, success rate, P5 drawdown. |
| `compare_plans` | Run the guardrail over a set of plan ids at one pinned `n`; return each plan's figures + the rule's pick, or "no pick" with the reason. |
| `plan_tracker` | Drift report for an accepted plan; on-track / behind / re-projected. |

`retirement_stress` (doc 31) shares the same MC path for sequence-of-returns
risk in the withdrawal phase. All three belong to the `modelling` tool set
(doc 31's tool-budget decision), never the always-on set.

### Bound the cost before shipping it

`compare_plans` over 4 plans × 5 buckets × 1,000 runs × 17 years, stepped
monthly in `shopspring/decimal`, is ~40M decimal operations inside one
synchronous HTTP request — which may itself be a tool call inside a chat turn,
under a request timeout, with a user waiting. `ClampRuns` permits 10,000, which
is 10× that.

Three bounds, all cheap:

- **Step annually, not monthly.** `SimulateWithdrawals` already does, returns
  are sampled annually anyway, and monthly stepping buys nothing here but 12×
  the work. Monthly contributions are summed to an annual figure with a
  half-year convention, and the convention is named in the basis.
- **Cap the comparison set** at 4 plans, matching the UI, and refuse more with
  a clear error rather than degrading.
- **Cap `n` for interactive paths** at 1,000 (`DefaultMonteCarloRuns`) and let
  the higher `ClampRuns` ceiling apply only to a direct endpoint call. Measure
  before shipping; if a 4-plan comparison exceeds ~2s, drop to float64 for the
  *sampling path only* — the terminal values still land in decimal, the money
  arithmetic stays exact, and the sampled return was a float the moment it left
  the generator.

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

- **Success-rate headline** — the largest number on a plan card. Phrased over
  the modelled sequences, never as a probability of a real outcome:
  **"meets your target in 94% of 1,000 simulated futures"**, not "94% chance to
  meet your target." `MonteCarloResult.SurvivalRate` already holds this line
  and the reason for it; the same discipline applies here.
- **Fan chart** — shaded P10–P90 band with the **median simulated outcome** and
  the target line in ember. Lives on the allocator output (doc 32's section 03),
  beside doc 32's compound-at-μ figure — the two are labelled distinctly and one
  line explains the gap. See "Two P50s".
- **Plan comparison view** — side-by-side cards for 2–4 saved plans at one
  pinned run count; each shows goal-fit, success rate, P5 drawdown, σ; the
  guardrail's pick is flagged with the rule cited ("top pick: meets all goals
  at the median outcome, highest success rate, P5 drawdown 19% within your 25%
  floor"). When every plan breaches the floor the view says **no pick** and
  why — it does not quietly promote the least-bad one.
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

Three further prohibitions, each covering a way the model can undo the work
this doc does to make the numbers honest:

- **Never restate a success rate as a probability.** "94% of 1,000 simulated
  futures" is the claim; "94% chance" is not, and the model will reach for the
  shorter phrasing unless told not to. `monteCarloBasis` already carries this
  discipline for the withdrawal-phase figure — extend the same sentence here.
- **Never present the top pick as advice.** The rule picked it; the model
  explains *which clause* placed it there. "Plan B is the pick — it meets both
  goals at the median outcome and has the highest success rate of the three
  within your drawdown floor" is in bounds. "You should choose Plan B" is not.
- **When there is no pick, say so.** An empty filtered set (every plan breaches
  the floor) is a real answer, and the model must not resolve it by quietly
  naming the least-bad plan.

Doc 24's options waterfall and this guardrail are two different rules answering
two different questions, and the model must not blend them — see doc 24,
"Relationship to doc 33's guardrail rule". Where the two disagree on the page,
the explanation is the disagreement, not a synthesis.

## Verification

Decimal-exact inputs, reproducible simulation.

- **Determinism:** same plan + assumptions + `n` → identical sorted terminal
  values (assert byte-equal after rounding). Same seed → same result.
- **Baseline-vs-baseline:** an empty plan produces zero variance; P10 = P50 =
  P90 = current balance.
- **The two-P50 agreement test:** a plan with **σ=0 on every bucket** produces
  a median simulated outcome exactly equal to doc 32's compound-at-μ figure, to
  the cent. This is the check that the two engines share arithmetic. Then
  assert the *inequality* that follows for σ>0: the median simulated outcome is
  strictly **below** doc 32's figure, and the gap widens with σ and with the
  horizon. A build where they match at σ=15% has a bug in one of them.
- **Correlated draws:** a plan with `k` identical risky buckets has the same
  terminal σ as one bucket of `k×` the size — the property independent sampling
  breaks. Assert directly, because this is the error that silently inflates
  every success rate on the page.
- **Drawdown stability:** the reported P5 drawdown converges as `n` goes
  100 → 1,000 → 10,000 (successive differences shrink). Assert the same run at
  two run counts does not move the figure more than a stated tolerance — the
  test that a maximum would fail and a percentile passes.
- **Guardrail determinism:** two runs over the same plan set name the same
  pick with the same cited figures — and the sort is **total**: two plans
  identical on success rate and σ still yield one stable pick, by name.
- **Guardrail structure, step by step:** a plan breaching
  `risk_drawdown_floor` is excluded at F1 even when its success rate is
  highest; a set where *every* plan breaches the floor returns **no pick** with
  that reason, not a least-bad choice; a set where no plan meets every goal
  falls through to F3 and the response says so; `risk_drawdown_floor IS NULL`
  skips F1 and the response says that too. Goal-fit is judged at P50 — a plan
  meeting its goals at the compound-at-μ figure but not at P50 does **not**
  pass F2.
- **Pinned `n`:** `compare_plans` refuses a comparison assembled from runs at
  differing run counts rather than rendering it.
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
