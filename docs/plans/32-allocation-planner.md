# 32 — Allocation planner

*(Wave 6 — the multi-bucket allocator. Visual companion:
[advisor-overview.html](advisor-overview.html) §6. Builds on docs 24 and 31.
The thing a real advisor does that the app cannot yet: split a lump and/or a
monthly surplus across Roth / 529 / brokerage / debt / emergency fund with
per-bucket projections, contribution-cap enforcement, and goal-mapping.)*

## Context

Doc 24 ranks **single-pick** options ("you have $X — the highest-value thing
is to pay the card"). A real advisor goes further: "you have $30k sitting in
savings and $1,800/mo surplus — split it $7k into your Roth (capped), $5k
into the 529, $18k into brokerage; that funds retirement on time, gets the
529 to 80% of college cost, and the surplus keeps the card declining." That
is an **allocation across buckets with caps and per-bucket projections**, and
nothing in the app computes it today.

Every input exists: `networth/limits.go` already caps Roth/IRA/HSA (and
honestly declines to cap a 529); `networth.ProjectRetirement` is
account-aware; `goals.Compute` knows what each goal needs; `accounts.tax_treatment` tags every account. What is missing is the layer that
takes a proposed split, enforces caps, projects each bucket, maps outcomes to
goals, and — for the cash sitting idle — flags the cash-drag it causes.

## AI vs deterministic split

**Deterministic:** the entire engine. Cap pooling and enforcement, per-bucket
projection (delegated to doc 15's `ProjectByAccount`), goal-mapping (via
`goals.Compute`), cash-drag, asset-location rules, college-cost projection.
Exact decimal throughout.

**AI:** presentation only. The model receives finished per-bucket figures,
cap headroom, and goal-fit, and writes prose. It never invents a bucket,
never reallocates, never projects. When doc 33 lands, the AI also names the
top-ranked plan **under a documented guardrail rule** — still from computed
likelihoods, never opinion.

## Prerequisites

- **[31-advisor-surface.md](31-advisor-surface.md)** — same wave. The chat
  exposure pattern and the household-profile columns (risk floor, state).
- **[15-fire-projections.md](15-fire-projections.md)** — shipped. Per-bucket
  projection delegates to `ProjectByAccount`; do not fork it.
- **[23-paystub-income.md](23-paystub-income.md)** — wave 5, lands first.
  Contribution headroom is only honest against real YTD deferrals.
- **[27-inflation-adjusted-views.md](27-inflation-adjusted-views.md)** and
  **[26-real-asset-revaluation.md](26-real-asset-revaluation.md)** — wave 5.
  Long-horizon projections are meaningless with stale dollars and stale asset
  values. The user's explicit call to bundle these first.

## Data model

**Reserved migration: `00049_allocation_planner.sql`.**

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
ALTER TABLE goals DROP CONSTRAINT IF EXISTS goals_kind_check;
ALTER TABLE goals ADD  CONSTRAINT goals_kind_check
    CHECK (kind IN ('savings','debt_payoff','college'));

-- Saved allocation plans. Schema-versioned like doc 28 scenarios; results are
-- NOT stored (recomputed against the live baseline on open).
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

**Cap enforcement** — pool contributions by `limitGroup` before applying
`AnnualLimitFor`. A plan that puts $8,000 into one Roth and $7,000 into a
traditional IRA overstates headroom; both share the IRA group. The Roth
*lump* in a single tax year is capped at the elective deferral; the engine
refuses to project an over-contribution and returns the spill. A 529 returns
`ok=false` from `AnnualLimitFor` and is projected uncapped — the honest
answer, never an invented state-cap.

**Per-bucket projection** — delegate to `ProjectByAccount`. **Do not write a
second projection loop** (doc 15's warning, inherited). Each bucket compounds
at its assumed real return over the horizon; a monthly contribution is added
monthly.

**Goal-mapping** — for each goal, run `goals.Compute` against the plan's
contribution to that goal's linked account; report on-track / completion-date
/ shortfall. For a `college` goal: inflate `target` (today's dollars) at
`college_inflation_rate` to the beneficiary's enrollment year (from
`household_people.birthdate` + 18, or a set target age), then compare to the
529 bucket's projected value.

**Cash-drag detection** — scan depository accounts; for each, annual drag =
`balance × (household_HYSA_rate − accounts.deposit_apy)`. Report only above a
threshold (silence beats noise — doc 24's rule). Silent where `deposit_apy` is
NULL. Surfaced as an insight and as the `idle_cash` chat tool.

**Asset-location** — deterministic rules mapping asset class to account type
for tax efficiency: bonds → tax-advantaged (tax-deferred growth on ordinary
income); equities → taxable (long-term capital gains treatment, tax-loss
harvesting); tax-free bonds → taxable (the exemption is wasted in a
tax-advantaged account). Returns a suggestion per bucket; the user confirms.
This is the difference between "allocation" and "tax-aware allocation."

**Horizon-aware assignment** — the engine refuses to project a short-horizon
goal's funds into a volatile bucket without flagging it. EF (liquid by
definition) stays in the HYSA bucket; a goal under ~3 years is flagged if
its bucket's volatility exceeds the user's risk floor (doc 31's
`risk_drawdown_floor`).

### Chat tools — `chat_handlers.go`

| Tool | Purpose |
|---|---|
| `allocation_plan` | Takes a proposed split (lump + monthly + horizon + per-bucket %); returns per-bucket projected value, cap headroom/spill, goal-mapping. |
| `idle_cash` | Cash-drag report across depository accounts. |
| `asset_location` | Suggested asset class per account type, for the current household. |
| `college_projection` | 529 vs inflated college cost; funded %, shortfall, monthly needed. |

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
- **Cap enforcement:** a split putting $8,000 into one Roth is capped at the
  2026 limit ($7,500 under 50); the $500 spill is returned, not projected as
  a contribution. Two IRAs share one cap (pool by `limitGroup`).
- **529 uncapped:** `AnnualLimitFor` returns `ok=false`; the engine projects
  the full amount without inventing a state cap.
- **College projection:** a 529 with a $20k balance, $200/mo, 6% return, and
  a beneficiary 10 years from enrollment against a $100k today's-cost target
  (5.5% college inflation) matches a hand-computed funded percentage.
- **Cash-drag fixture:** a checking account at 0.4% with $10k vs a 4.5%
  HYSA rate → $410/yr drag, reported; a NULL `deposit_apy` → silent.
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
- The likelihood layer itself (doc 33). This doc projects deterministic
  P50 outcomes; percentiles and success rates are doc 33.
- Auto-optimising the split. The engine computes outcomes for a split the
  user chose; it does not silently pick the "best" allocation. (Doc 33's
  guardrail names a top plan from a set the user is comparing — it does not
  generate one unprompted.)
- Real-time price feeds for forward modelling. Assumed real returns only;
  `asset_prices` stays historical.
