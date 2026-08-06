# 32 — Allocation planner

*(Wave 6 — the multi-bucket allocator. Visual companion:
[advisor-overview.html](advisor-overview.html) §6. Builds on docs 24 and 31.
The thing a real advisor does that the app cannot yet: split a lump and/or a
monthly surplus across Roth / 529 / brokerage / debt / emergency fund with
per-bucket projections, contribution-cap enforcement, and goal-mapping.)*

> **Shipped.** What follows is the plan as written. The allocator is
> `backend/internal/allocation/` (baseline, per-bucket projection, cash-drag,
> college drawdown, asset-location, store), the surface is
> `frontend/src/components/BucketAllocator.tsx`, and migration
> `00055_allocation_planner.sql` is taken. See **[Shipped notes](#shipped-notes)**
> at the end before touching this area.
>
> The short version: shipped as `00055`, not the reserved `00053` (goose strict
> ordering — docs 30/31 took the numbers below it); the schema gained two
> columns the plan's SQL block does not print — `goals.college_years` (per-goal
> years, default 4) and `households.magi` / `magi_tax_year` (user-entered MAGI
> with its year, so a stale figure reads as `unknown`); the `goals_kind_check`
> is a NEW constraint, not an edit; and MAGI eligibility stays `unknown` without
> a user-entered figure rather than flattering the household.

## Context

Doc 24 ranks **single-pick** options ("you have $X — the highest-value thing
is to pay the card"). A real advisor goes further: "you have $30k sitting in
savings and $1,800/mo surplus — split it $7k into your Roth (capped), $5k
into the 529, $18k into brokerage; that funds retirement on time, gets the
529 to 80% of college cost, and the surplus keeps the card declining." That
is an **allocation across buckets with caps and per-bucket projections**, and
nothing in the app computes it today.

Most inputs exist: `networth/limits.go` already caps Roth/IRA/HSA (and
honestly declines to cap a 529); `networth.ProjectRetirement` is
account-aware; `goals.Compute` knows what each goal needs;
`accounts.tax_treatment` tags every account. What is missing is the layer that
takes a proposed split, enforces caps, projects each bucket, maps outcomes to
goals, and — for the cash sitting idle — flags the cash-drag it causes.

**Four things this doc originally assumed existed, and does not.** They are
the difference between "wiring" and "engine work", and the estimate should
reflect it:

1. **There is no `ProjectByAccount`.** The first draft twice instructed the
   implementer to delegate to it. The real engine is
   `networth.ProjectRetirement(plans []AccountPlan, a RetirementAssumptions,
   now time.Time)`, and the per-account series *is* there —
   `RetirementPoint.ByAccount` is a `map[string]AccountPoint` keyed by
   `AccountPlan.ID` (`retirement.go:148`). Delegate to **that**.
2. **`ProjectRetirement` applies one return rate to every account.**
   `RetirementAssumptions.RealReturnRate` is a single household scalar and
   `monthlyRate` is computed once (`retirement.go:269`). This doc's UI offers a
   per-bucket editable return and doc 33 needs a per-bucket σ. See "Per-bucket
   returns" below — the engine needs a small, backwards-compatible extension.
3. **`limitGroup` is unexported** (`limits.go:156`). A new package cannot call
   it. See "Cap enforcement".
4. **A lump sum has nowhere to go that caps can see.** `capContributions`
   scales only the monthly slice. See "Lump sums and caps" — this is the one
   that silently breaks this doc's own headline test.

## AI vs deterministic split

**Deterministic:** the entire engine. Cap pooling and enforcement, per-bucket
projection (delegated to doc 15's `ProjectRetirement`), goal-mapping (via
`goals.Compute`), cash-drag, asset-location rules, college-cost projection.
Exact decimal throughout.

**AI:** presentation only. The model receives finished per-bucket figures,
cap headroom, and goal-fit, and writes prose. It never invents a bucket,
never reallocates, never projects. When doc 33 lands, the AI also names the
top-ranked plan **under a documented guardrail rule** — still from computed
likelihoods, never opinion.

## Prerequisites

- **[31-advisor-surface.md](31-advisor-surface.md)** — same wave. The chat
  exposure pattern, the tool-set budget, and `households.filing_status` (which
  the eligibility check below keys on) plus `risk_drawdown_floor`.
- **[15-fire-projections.md](15-fire-projections.md)** — shipped. Per-bucket
  projection delegates to `networth.ProjectRetirement`, reading
  `RetirementPoint.ByAccount`; do not fork it. (Doc 15's *plan* said it would
  add a function called `ProjectByAccount`; what shipped is
  `ProjectRetirement`, which does the same job. Use the shipped name.)
- **[23-paystub-income.md](23-paystub-income.md)** — wave 5, lands first.
  Contribution headroom is only honest against real YTD deferrals.
- **[27-inflation-adjusted-views.md](27-inflation-adjusted-views.md)** and
  **[26-real-asset-revaluation.md](26-real-asset-revaluation.md)** — wave 5.
  Long-horizon projections are meaningless with stale dollars and stale asset
  values. The user's explicit call to bundle these first.

## Data model

**Reserved migration: `00053_allocation_planner.sql`.** (Wave 5 ships first
and holds `00047`–`00051`; doc 31 holds `00052`. This doc originally reserved
`00049`, which collided with wave 5's un-numbered reissues — the README's
reservation table now assigns all of them. Confirm the number is free **and**
above the highest applied version before writing.)

```sql
-- Deposit-account yield, for cash-drag. Plaid does not serve this reliably,
-- so it is user-entered (a percent). NULL = unknown; the cash-drag detector
-- stays silent rather than assume zero.
ALTER TABLE accounts ADD COLUMN deposit_apy NUMERIC(5,2);

-- College-inflation rate (~5-6%/yr historically, NOT general CPI). Lives on
-- projection_assumptions so a saved scenario sees what it was built with.
ALTER TABLE projection_assumptions ADD COLUMN college_inflation_rate NUMERIC(5,2)
    NOT NULL DEFAULT 5.5;

-- A 'college' goal kind. beneficiary is goals.person_id (a household_person);
-- target is in today's dollars and inflated to enrollment year by the engine.
--
-- NOTE: goals.kind has NO check constraint today -- 00012_goals.sql:15 is a
-- plain `TEXT NOT NULL` with a comment naming the two intended values. So the
-- DROP below is a no-op kept only for idempotency, and the ADD is a NEW
-- constraint tightening a previously free column, not an edit to an existing
-- one. Two consequences: the migration fails if any live row holds a kind
-- outside the set (check before writing it), and this is the point at which
-- 'savings' | 'debt_payoff' stop being a convention in goal_handlers.go
-- (goalKindSavings / goalKindPayoff, goal_handlers.go:25-26) and become a
-- database invariant. That is an improvement; it is just not what the first
-- draft claimed it was doing.
ALTER TABLE goals DROP CONSTRAINT IF EXISTS goals_kind_check;
ALTER TABLE goals ADD  CONSTRAINT goals_kind_check
    CHECK (kind IN ('savings','debt_payoff','college'));

-- Saved allocation plans. Schema-versioned like doc 28 scenarios; results are
-- NOT stored (recomputed against the live baseline on open).
--
-- MONEY INSIDE THESE JSONB BLOBS IS STORED AS A STRING, NEVER A JSON NUMBER.
-- See "Money in JSONB" below -- this is a real hole in the continuity rule,
-- not a style preference.
CREATE TABLE allocation_plans (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id  UUID NOT NULL REFERENCES households (id) ON DELETE CASCADE,
    created_by    UUID REFERENCES users (id) ON DELETE SET NULL,
    name          TEXT NOT NULL,
    inputs        JSONB NOT NULL,        -- {lump, monthly, horizon, target, split, assumptions}
    input_version INT   NOT NULL DEFAULT 1,
    assumptions   JSONB NOT NULL,        -- snapshot at save time (doc 28 rule)
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX allocation_plans_household_idx ON allocation_plans (household_id);
```

**`input_version` is load-bearing** — saved plans are kept for years; version
from day one and migrate forward explicitly (doc 28's rule, inherited).

### Money in JSONB

The continuity rule says money is cast to text in SQL and never travels as a
JSON number, and `export.go:215` enforces it for `numeric` columns. **It cannot
reach inside a `jsonb` column.** `normalise` passes jsonb through as
`json.RawMessage` (`export.go:232`), so `{"lump": 30000.50}` in `inputs` leaves
the export as a JSON number, gets parsed by whatever reads it as a float64, and
the guarantee the rest of the export makes silently does not hold for this
table.

So: **every money field inside `inputs` and `assumptions` is a
`StringFixed(2)` string** — `{"lump": "30000.50"}` — marshalled and unmarshalled
through `decimal.Decimal`, which round-trips strings exactly. Percentages and
rates likewise. Integers that are counts (horizon in years, `input_version`)
stay numbers; they are not money and they are exact in float64 anyway.

This applies identically to doc 33's `plan_trackings.snapshot_inputs`. A test
asserts it: marshal a plan containing `0.1 + 0.2`-style values, round-trip it
through the database and the export, and assert byte-equality of the decimal.
Without that test the rule is a comment nobody notices breaking.

## Continuity

| Table / column | Category | Rationale |
|---|---|---|
| `allocation_plans` | `InExport` | User-authored decisions |
| `accounts.deposit_apy` | (existing) | `accounts` already `InExport` |
| `projection_assumptions.college_inflation_rate` | (existing) | already `InExport` |
| `goals.kind` expansion | (existing) | already `InExport` |

No new blob stores or volumes.

## Backend

### New package `backend/internal/allocation/`

**Baseline assembly** — one function (`AssembleBaseline`), reused by every
caller. Gathers: accounts grouped by `limitGroup` (limits.go) with balances
and YTD contributions; goals with their linked account and beneficiary; debts
with APR; the assumptions snapshot. Pure read.

**Override application** — pure, no side effects. **A plan must never mutate
real data.** Enforce with types: the plan operates on a *copy* of the
baseline. This is the most dangerous bug available here (same class as doc
28's scenario engine) — the verification section tests it by snapshotting
every relevant table.

**Cap enforcement** — pool contributions by limit group before applying
`AnnualLimitFor`. A plan that puts $8,000 into one Roth and $7,000 into a
traditional IRA overstates headroom; both share the IRA group. The engine
refuses to project an over-contribution and returns the spill. A 529 returns
`ok=false` from `AnnualLimitFor` and is projected uncapped — the honest
answer, never an invented state-cap.

Three corrections to how the first draft described this:

**`limitGroup` is unexported** (`limits.go:156`), so `internal/allocation`
cannot call it. Export it as `networth.LimitGroup` — it is a pure
`string → string` function with no state, the rename is mechanical, and the
alternative (a second copy of the grouping rule) is exactly the duplication
doc 17 warns about with the merchant resolver. `capContributions` keeps using
it unchanged.

**A Roth IRA is capped by the IRA limit, not "the elective deferral."** The
elective deferral is the 401(k) number (`TaxYearLimits.Elective401k`); the IRA
limit is `TaxYearLimits.IRA`, and `AnnualLimitFor` already routes
`trad_ira`/`roth_ira` to it (`limits.go:130`). The doc's own verification case
is right — $7,500 for 2026, under 50 (`limits.go:73`) — but the sentence above
it named the wrong limit, and an implementer following the prose rather than
the test would cap a Roth at $24,500.

**Caps are applied at the current year's limits for the whole horizon.**
`taxYearLimits` holds 2025 and 2026 only, and `ProjectRetirement` reads
`Limits(now.Year())` once, sets `LimitsConfigured=false` when the year is
missing, and does not cap at all in that case (`retirement.go:216`, `:254`).
For a 17-year plan in *real* dollars that is defensible — the IRS indexes these
limits to inflation, so a constant real cap is closer to the truth than a
nominal one frozen in 2026 — but it is a modelling choice and the plan output
names it: "contribution caps applied at 2026 limits, held constant in real
terms." When the running year is unconfigured, the plan says contributions were
**not** capped, matching `LimitsConfigured` rather than pretending.

### Lump sums and caps

**This is the correction that breaks the doc's headline test if it is missed.**
`capContributions` (`retirement.go:417`) scales only the `monthly[]` slice. The
only field on `AccountPlan` that can hold a lump today is `Balance`, which
`capContributions` never inspects — so folding a $8,000 Roth lump into
`Balance` and calling `ProjectRetirement` projects the whole $8,000, uncapped,
and this doc's own verification case ("$500 spill returned, not projected")
cannot pass.

Fix it in the engine, not around it: add `FirstYearContribution
decimal.Decimal` to `AccountPlan`, defaulting to zero so every existing caller
is unaffected, and include it in the group's `planned` total inside
`capContributions` before the limit is compared. The spill is returned in a
`CapNote` alongside the existing `Planned`/`Allowed` pair. The scaling rule
stays proportional for the same reason it already is: the split between a
traditional and a Roth is the user's decision and the projection has no
business re-allocating it.

The alternative — capping in `internal/allocation` before calling the
projection — was rejected because it puts the cap rule in two places, and the
next caller to add a lump path would have to remember. `capContributions` is
where the rule lives.

### Roth and HSA eligibility — a cap is not permission

`AnnualLimitFor` answers "what is the annual cap for this account type at this
age" and nothing else. It has no idea whether the household may contribute at
all, and the allocator as first drafted would happily project $7,500/yr into a
Roth IRA for a household well over the MAGI phase-out — a plan that is not
merely optimistic but *not legal to execute*, presented with the same
confidence as everything else on the page. This is the single largest
correctness gap in the wave and it is the reason doc 31 adds
`households.filing_status`.

Add `networth/eligibility.go` beside `limits.go`, in exactly the shape
`limits.go` established and for exactly the same reasons:

- **A versioned Go map** of Roth IRA MAGI phase-out ranges, keyed by
  `(tax_year, filing_status)`. Reviewed when the app is upgraded; a stale row
  in a table nobody updates is how this goes silently wrong.
- **An unconfigured year returns `ok=false`**, and the caller surfaces it. The
  plan then says eligibility is unverified — never "eligible" by default. The
  flattering assumption is the one that gets shipped by accident.
- `EligibilityFor(treatment, year, filingStatus, magi) → (status, ok)` where
  status is `eligible` / `phased_out` (with the reduced limit computed, since
  the phase-out is a documented linear formula, not a cliff) / `ineligible`.

Two honesty constraints. **MAGI is not modified AGI is not gross income**, and
the app does not have it — doc 23's paystub data gets closer but a full MAGI
needs a return. So MAGI is **user-entered**, optional, and where it is absent
`EligibilityFor` returns `unknown` and the plan is projected with a clearly
labelled "assumes you're eligible to contribute" caveat rather than silently.
And the **backdoor Roth** exists, is common above the phase-out, and is not
modelled — the plan says "ineligible for a direct Roth contribution", which is
the true statement, rather than "you cannot put money here".

The same file carries the HSA note: an HSA contribution requires HDHP coverage
for the month, which the app also cannot see. `hsa` eligibility is `unknown`
unless the household says otherwise, and the doc 31 `contribution_room` tool
reports it as such.

**Per-bucket projection** — delegate to `ProjectRetirement` and read
`RetirementPoint.ByAccount` (`retirement.go:148`), which is already keyed by
`AccountPlan.ID`. **Do not write a second projection loop** (doc 15's warning,
inherited — and note the first draft named a function, `ProjectByAccount`, that
does not exist).

### Per-bucket returns need a small engine change

`ProjectRetirement` compounds every account at one household rate:
`RetirementAssumptions.RealReturnRate`, converted once into `monthlyRate`
(`retirement.go:269`). This doc's UI offers "per-bucket assumed return
(editable, pre-filled from assumptions)" and doc 33 needs a per-bucket σ.
Neither is expressible today.

Add an optional `RealReturnRate decimal.Decimal` to `AccountPlan`, where
**zero means "use the household rate"**. That keeps every existing caller
byte-identical (a zero-valued field on a struct literal nobody edited), keeps
one projection loop, and makes the per-bucket case a lookup rather than a fork.
`monthlyRate` becomes a per-account slice computed once before the month loop,
not a scalar — the same arithmetic, indexed.

Doc 33 adds `Volatility` to the same struct under the same convention. Do these
together if the two docs land in one cycle; the second one is free.

### Not every bucket is a compound projection

The allocator's buckets are Roth / 529 / brokerage / **debt** / **emergency
fund**, and only the first three are the thing `ProjectRetirement` does. The
first draft routed all five through one function. They are three kinds of math:

| Bucket kind | Engine | Note |
|---|---|---|
| Investment (Roth, 529, brokerage, 401k) | `ProjectRetirement` → `ByAccount` | Needs a confirmed `tax_treatment`; an untagged account is **excluded and reported**, never defaulted (`retirement.go:222`) |
| Debt paydown | `goals.ComputePayoff` | Amortization, not compounding. The "return" is interest avoided, and the payoff can be "never" — see doc 19's iteration-cap note |
| Emergency fund / cash | Simple accrual at the account's `deposit_apy` | σ=0. Liquid by definition, and never projected into a volatile bucket — see "Horizon-aware assignment" |

`AssembleBaseline` therefore returns a tagged union of bucket kinds, and the
per-bucket result type carries which engine produced it so the "show the
arithmetic" panel can render the right formula. A debt bucket showing a
compound-growth formula would be worse than showing nothing.

**Goal-mapping** — for each goal, run `goals.Compute` against the plan's
contribution to that goal's linked account; report on-track / completion-date
/ shortfall.

For a `college` goal, **college is four years of spending, not a bill on the
first day.** The first draft inflated `target` to the enrollment year and
compared it to the 529 balance at that instant, which misstates funded-% in
both directions at once: it ignores that years 2–4 cost more (they inflate for
another one, two and three years) and that the balance keeps compounding while
they are being paid.

Model it as a **drawdown**, which is barely more code and is actually right:

1. Resolve enrollment year from `household_people.birthdate` + 18, or a set
   target age. Ages resolve birthdate → stored integer → `ok=false`
   (`networth.ResolveAge`, doc 21's rule) — never the clock.
2. Inflate the today's-dollars `target` (one year's cost) at
   `college_inflation_rate` **separately for each of the four years**.
3. Walk the 529 bucket forward, drawing each year's inflated cost at the start
   of that year and compounding the remainder at the bucket's assumed return
   through the four years.
4. Funded % is the share of the **total four-year inflated cost** the bucket
   covers before running dry; the shortfall is the uncovered remainder in the
   year it first appears.

The four-year count is a per-goal field with a default of 4, since community
college transfers and five-year programmes exist and the engine has no business
assuming. Report the per-year figures, not just the total — "funded through
sophomore year, $18,400 short in junior year" is the sentence a parent can act
on, and it is the one a single-point comparison cannot produce.

**Cash-drag detection** — scan depository accounts; for each, annual drag =
`balance × (benchmark_rate − accounts.deposit_apy)`. Report only above a
threshold (silence beats noise — doc 24's rule). Silent where `deposit_apy` is
NULL. Surfaced as an insight and as the `idle_cash` chat tool.

**`benchmark_rate` has to come from somewhere, and nothing stores it.** The
first draft called it `household_HYSA_rate` and added no column for it; the
verification fixture then asserts "a 4.5% HYSA rate" that the schema has no way
to hold. Two options were weighed and the second wins:

- *Bundle a rate.* Rejected: it is a market number that goes stale, and either
  it is an outbound fetch the README promises against or a transcribed constant
  nobody can verify — the same trade doc 15 refused for its return series.
- **Use the household's own best rate.** `benchmark_rate` is the **highest
  non-NULL `deposit_apy` across the household's own depository accounts**. It
  needs no new column, no market data and no maintenance, and it makes the
  claim strictly true and checkable: "this $14k is earning 0.4% while your own
  savings account earns 4.5% — moving it earns $574/yr." A household with one
  account, or with no `deposit_apy` filled in anywhere, has no benchmark and
  **the detector stays silent** rather than inventing one.

That silence is the right default: `deposit_apy` is user-entered precisely
because Plaid does not serve it reliably, so an empty field means "unknown",
never "zero". Two further honesty notes for the copy: the drag figure is
**pre-tax** (savings interest is ordinary income), and a checking account's
operating float is not idle money — the detector excludes an amount equal to
one month of trailing fixed costs before computing drag on a transaction
account, or it will tell every household to empty its checking account.

**Asset-location** — deterministic rules mapping asset class to account type
for tax efficiency: bonds → tax-advantaged (tax-deferred growth on ordinary
income); equities → taxable (long-term capital gains treatment, tax-loss
harvesting); tax-free bonds → taxable (the exemption is wasted in a
tax-advantaged account). Returns a suggestion per bucket; the user confirms.
This is the difference between "allocation" and "tax-aware allocation."

**But these are tax rules stated without a tax rate, and the app has no
bracket.** The bond-in-tax-advantaged rule is conventional, not universal: it
depends on the spread between the household's ordinary-income rate and its
long-term capital-gains rate, and at a 12% marginal rate with a 0% LTCG rate
the conventional ordering weakens or inverts. Shipping it as a bare
recommendation would be the app's first genuinely unsupported claim.

So `asset_location` ships **as disclosure, not as a recommendation**, in the
same posture doc 14 took with fee drag: it names the rule, names the
assumption the rule rests on ("assumes your ordinary-income rate exceeds your
long-term capital-gains rate — true for most households above the 22%
bracket"), and says plainly that the app does not know the household's bracket.
When a marginal-bracket table exists — versioned by tax year beside
`limits.go`, keyed by `filing_status`, `ok=false` for an unconfigured year,
owned by whichever doc first needs it — this tool computes the spread and the
disclosure becomes a figure. Until then it does not pretend to.

**Horizon-aware assignment** — the engine refuses to project a short-horizon
goal's funds into a volatile bucket without flagging it. EF (liquid by
definition) stays in the HYSA bucket; a goal under ~3 years is flagged if
its bucket's volatility exceeds the user's risk floor (doc 31's
`risk_drawdown_floor`).

### Chat tools — `chat_handlers.go`

| Tool | Purpose |
|---|---|
| `allocation_plan` | Takes a proposed split (lump + monthly + horizon + per-bucket %); returns per-bucket projected value, cap headroom/spill, **eligibility status per bucket**, goal-mapping. |
| `idle_cash` | Cash-drag report across depository accounts, against the household's own best `deposit_apy`. Silent with no benchmark. |
| `asset_location` | Asset-class-per-account-type rule **plus its stated assumption**; disclosure, not a recommendation, until a bracket table exists. |
| `college_projection` | 529 vs the four-year inflated cost; per-year funded %, first shortfall year, monthly needed. |

These belong to the `modelling` tool set (doc 31's tool-budget decision), not to
the always-on set.

### Handlers — `internal/api/allocation_handlers.go`

```
POST   /api/allocation/plan            handleRunAllocation      // run, do not save
POST   /api/allocation/plans           handleSavePlan           // save named, input_versioned
GET    /api/allocation/plans           handleListPlans
GET    /api/allocation/plans/{id}      handleGetPlan            // recompute against live baseline
DELETE /api/allocation/plans/{id}      handleDeletePlan
GET    /api/accounts/idle-cash         handleIdleCash
```

All `authenticate` + `RequireAdult`, household-scoped.

## Frontend

The **Bucket allocator** is the centerpiece of the Advisor page (doc 31's
section 03). A two-pane layout:

- **Inputs:** lump sum slider, monthly surplus slider, horizon, target
  nest-egg, per-bucket allocation sliders (auto-normalize to 100%),
  per-bucket assumed return (editable, pre-filled from assumptions).
- **Output:** per-bucket projected value + growth, cap headroom with spill
  indicator, goal-mapping ("funds retirement on time; 529 to 80% of
  college"), total projected, and — when doc 33 ships — the success-rate
  headline and P10/P50/P90 fan chart.
- **Save plan** — names and persists; re-runs against live state on reopen.
- **Show the arithmetic** — every projected figure expandable into its
  compound formula, the cap, the assumption. The credibility proposition.

The idle-cash report appears as both an insight card and a panel above the
allocator ("$14k earning 0.4% — moving it earns $574/yr") with an "include
in lump" affordance.

## AI notes

The model receives a finished `allocation_plan` result and may phrase it. It
must not propose a split on its own — splits come from the user or from a
deterministic suggestion engine. When doc 33 lands, the guardrail rule lets
the AI name the top plan from a set the user is comparing, in terms of the
computed likelihoods and the user's risk floor — never opinion.

## Verification

Decimal-exact, table-driven.

- **No real-data mutation (highest priority):** snapshot `accounts`,
  `holdings`, `goals`, `account_contributions`, `projection_assumptions`
  before and after a `POST /api/allocation/plan`; assert byte-identical.
- **Baseline-vs-baseline:** a plan with zero lump and zero monthly produces
  exactly the household's current projected position (zero delta). If not,
  the engine is not deterministic.
- **Cap enforcement, on the LUMP as well as the monthly:** a split putting an
  $8,000 **lump** into one Roth is capped at the 2026 IRA limit ($7,500 under
  50 — `limits.go:73`); the $500 spill is returned in a `CapNote`, not
  projected as a contribution and not quietly folded into the opening balance.
  Run the same assertion for an $8,000 *monthly-summed* contribution, and for a
  lump-plus-monthly combination that only breaches the cap when added together
  — that last case is the one that fails if `FirstYearContribution` is not in
  the group total.
- **Two IRAs share one cap** (pool by `networth.LimitGroup`), and a Roth capped
  at the IRA limit is never capped at `Elective401k`.
- **Unconfigured year:** with the running year absent from `taxYearLimits`,
  nothing is capped and the plan says so — matching `LimitsConfigured`, not
  substituting an adjacent year.
- **Eligibility is separate from the cap:** a household over the Roth MAGI
  phase-out for its `filing_status` gets `ineligible` and a $0 projected Roth
  contribution, not a $7,500 one; a household inside the phase-out gets the
  computed reduced limit; **a household with no MAGI entered gets `unknown`
  and a labelled caveat, never `eligible`.** An unconfigured tax year returns
  `ok=false` and is surfaced.
- **529 uncapped:** `AnnualLimitFor` returns `ok=false`; the engine projects
  the full amount without inventing a state cap.
- **Per-bucket returns:** two buckets with different `RealReturnRate` values
  compound differently in one run; a plan with every bucket rate left at zero
  produces output byte-identical to the pre-change household-rate behaviour.
- **Bucket kinds:** a debt bucket's result is amortization from
  `goals.ComputePayoff` (and reports "never" where the payment does not
  amortize), an EF bucket accrues at σ=0, and an untagged investment account is
  excluded and reported rather than defaulted.
- **College projection:** a 529 with a $20k balance, $200/mo, 6% return, and a
  beneficiary 10 years from enrollment against a $100k today's-cost **annual**
  target at 5.5% college inflation matches a hand-computed per-year drawdown —
  four inflated costs, the remainder compounding between them, and the first
  shortfall year named. Assert the four-year total is strictly greater than
  four times the enrollment-year cost, which is the error the single-point
  version made.
- **Cash-drag fixture:** a checking account at 0.4% with $10k, in a household
  whose best `deposit_apy` is 4.5%, reports $410/yr **less the one-month
  operating-float exclusion**; a NULL `deposit_apy` on the account → silent;
  **no non-NULL `deposit_apy` anywhere in the household → silent**, with no
  invented benchmark.
- **Money in JSONB round-trip:** a plan whose `inputs` carry values like
  `0.1`/`0.2`/`30000.50` survives save → export → reload byte-identical as
  decimal strings; assert no bare JSON number appears in a money field.
- **Visibility scope:** a plan returns only the caller's household accounts;
  a spouse's private account never appears in a bucket.
- **`input_version` round-trip:** a v1 plan still opens after a v2 input
  change.
- **Build:** `go build ./... && go vet ./... && go test -p 1 ./...` with
  `TEST_DATABASE_URL`. Frontend `tsc -b && vite build && oxlint`.

## Out of scope

- Executing transfers. A plan is a projection; the user acts on it.
- Specific security selection within a bucket. Regulated advice,
  permanently out.
- The likelihood layer itself (doc 33). This doc projects a deterministic
  compound-at-μ outcome; percentiles and success rates are doc 33.

  **Do not label this doc's figure "P50" in the UI.** Compounding at the mean
  and taking the median of a simulation are different statistics, and they
  disagree by roughly `e^(−Tσ²/2)` — about 15% at μ=7%, σ=15%, T=17y. Doc 33's
  fan chart renders on this doc's output, so the two numbers appear on the same
  card, and calling both "P50" is how a user finds a 15% discrepancy nobody can
  explain. This one is the **projected value at the assumed return**; doc 33's
  is the **median simulated outcome**. Doc 33 owns the reconciliation and the
  agreement test — read its "Two P50s" section before building this output.
- Auto-optimising the split. The engine computes outcomes for a split the
  user chose; it does not silently pick the "best" allocation. (Doc 33's
  guardrail names a top plan from a set the user is comparing — it does not
  generate one unprompted.)
- Real-time price feeds for forward modelling. Assumed real returns only;
  `asset_prices` stays historical.

## Shipped notes

The engine is `backend/internal/allocation/` — `baseline.go` (AssembleBaseline),
`plan.go` (the projection + cap enforcement), `cash.go` (cash-drag), `college.go`
(the four-year drawdown), `location.go` (asset-location as disclosure), `store.go`
(saved plans) — with handlers in `api/allocation_handlers.go`, chat tools in
`api/chat_tools_allocation.go`, and the frontend `frontend/src/components/BucketAllocator.tsx`.
Migration `00055_allocation_planner.sql` is taken. Four things reach outside the
plan text.

### 1. `00055`, not the reserved `00053`

The Data model's reservation note is right in principle and wrong on the number:
this doc reserved `00053`, but `00053_manual_accounts.sql` (doc 30, renumbered
up from its reserved `00047`) and `00054_advisor_surface.sql` (doc 31) landed
first, and goose refuses a migration below the current version. It took `00055`,
which is what the README's reservation table already assigned. Same lesson the
wave keeps relearning: read the migrations directory, not the inline number.

### 2. Two columns the plan's SQL block does not print

The shipped migration carries two additions the schema block above omits, each
with a named consumer documented in the migration:

- **`goals.college_years SMALLINT NOT NULL DEFAULT 4`** (CHECK 1–10). College is
  four years of spending, not a bill on the first day, and the count is per goal
  because community-college transfers and five-year programmes exist. The
  drawdown is per-goal; a hard-coded 4 would be the engine assuming.
- **`households.magi NUMERIC(20,4)` + `households.magi_tax_year INT`.** The Roth
  phase-out is keyed by filing status AND income; doc 31 shipped the status, and
  the income had nowhere to live. The YEAR travels with the figure so a stale
  MAGI reads as `unknown` rather than being silently reused — the same staleness
  rule `limits.go` enforces for IRS limits.

`allocation_plans.created_by` is `ON DELETE SET NULL` (matching `advisor_threads`):
a departing member's saved plan belongs to the household it was built for.

### 3. `goals_kind_check` is a NEW constraint, not an edit

The DROP in the migration is kept only for idempotency. `goals.kind` had **no**
check constraint since `00012_goals.sql` — it was a plain `TEXT NOT NULL` with
the vocabulary living in `goal_handlers.go`. The ADD tightens a previously free
column: it fails if any live row holds a kind outside `('savings','debt_payoff','college')`,
and `savings`/`debt_payoff` stop being a Go convention and become a database
invariant. That is an improvement; it is just not the no-op edit the first draft
implied.

### 4. Eligibility stays `unknown` without a user-entered MAGI

`AnnualLimitFor` returns a cap, and a cap is not permission. The eligibility
check (keyed on `households.filing_status` + `magi`) returns `eligible` /
`phased_out` / `ineligible` / `unknown`, and **absent or stale MAGI is `unknown`**:
the plan is projected with a labelled "assumes you're eligible to contribute"
caveat rather than silently assuming the flattering answer. The backdoor Roth is
not modelled; the plan says "ineligible for a direct contribution", which is the
true statement. All five new tables/columns are classified `InExport` in
`continuity/coverage.go`.
