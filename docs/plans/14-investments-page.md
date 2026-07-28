# 14 — Dedicated Investments page + performance analysis

*(TODO.md "Next major initiatives" #4.)*

## Context

Plaid investment data is **ingested and then barely used**. The schema is real
and complete — `securities`, `holdings`, `investment_transactions` all landed in
`00004_investments_liabilities.sql`, and `plaid/investments.go` fetches and
normalises them (`GetHoldings`, `convertSecurity`, `convertHolding`). All of
that currently surfaces as a **single line item** in Net Worth: the
`Breakdown.Investments` figure that `networth/project.go` reads.

So the app knows the user holds 14 positions across 3 accounts and shows them one
number. There is no per-holding view, no return calculation, no allocation
breakdown, no fee visibility. For a household with real money this is where the
app stops being useful, and it is precisely the ground Empower / Personal Capital
own.

This doc adds the surface. It also establishes the **account-type tagging** that
[15-fire-projections.md](15-fire-projections.md) depends on — a 401k and a
taxable brokerage cannot be projected the same way, and nothing in the schema
distinguishes them today (`accounts.type`/`subtype` come from Plaid and are not
reliable for tax treatment).

## AI vs deterministic split

**Deterministic:** all of it. Returns, IRR, allocation percentages, drift, fee
drag, dividend totals. Money is `NUMERIC(20,4)` in Postgres and
`shopspring/decimal` in Go, per the README's money rules.

**AI:** none required. If a narrative summary is added later it gets finished
strings, like every other producer. Do not let a model near a return calculation
— a hallucinated IRR is worse than no IRR.

## Prerequisites

None hard. Runs in parallel with 13 and 16. **Doc 15 depends on this one**
for account-type tags — if both are being worked at once, land the tagging
migration and its API early and tell 15's implementer it is available.

## Data model

**Reserved migration: `00020_investment_analysis.sql`.** Three additions:

```sql
-- 1. Account-type tagging. accounts.type/subtype come from Plaid and do not
-- reliably distinguish tax treatment, which is the thing that actually matters
-- for both grouping and projection. This is a user-confirmed classification.
ALTER TABLE accounts ADD COLUMN tax_treatment TEXT
    CHECK (tax_treatment IN (
        'taxable','trad_401k','roth_401k','trad_ira','roth_ira',
        '529','hsa','trust','other'
    ));
ALTER TABLE accounts ADD COLUMN is_managed BOOLEAN;

-- 2. Historical portfolio value, so return is measurable at all. Mirrors the
-- existing net-worth snapshot machinery rather than inventing a new pattern.
CREATE TABLE investment_snapshots (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id   UUID NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    as_of        DATE NOT NULL,
    market_value NUMERIC(20,4) NOT NULL,
    cost_basis   NUMERIC(20,4),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (account_id, as_of)
);

-- 3. Benchmark series for comparison. Same shape as the CPI table TODO #12
-- would add and the FX table #13 would add — keep them consistent.
CREATE TABLE asset_prices (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    ticker     TEXT NOT NULL,
    as_of      DATE NOT NULL,
    close      NUMERIC(20,4) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (ticker, as_of)
);
```

**`tax_treatment` is nullable on purpose.** Existing accounts have no correct
value and guessing one would be exactly the kind of quiet dishonesty the app
rejects. Infer a *suggestion* from `accounts.subtype` (Plaid returns `401k`,
`ira`, `roth`, `529`, `hsa`, `brokerage`) and present it for confirmation;
persist only what the user confirms. Untagged accounts must degrade gracefully
everywhere — grouped under "Untagged", excluded from tax-aware projections in
doc 15, with a visible prompt to classify them.

## Backend

### Snapshots

A daily job in `backend/internal/jobs/` writing one `investment_snapshots` row
per investment account. **Reuse the net-worth snapshot job's scheduling and
idempotency** (`UNIQUE (account_id, as_of)` + upsert) rather than building a
parallel path. Backfill is not possible — Plaid does not serve historical
holdings — so returns are "since we started watching" until history accrues.
**Say that in the UI.** A 3-day-old install must not render a 40% annualised
return.

### Return calculations

New code in `backend/internal/reporting/`, alongside `summary.go` and
`safetospend.go`.

- **Total return** — needs cost basis; `holdings` already carries it.
- **Time-weighted return (TWR)** — the honest measure of *the portfolio's*
  performance, neutral to deposit timing. Requires sub-period returns broken at
  each external cash flow.
- **Money-weighted return (IRR)** — the honest measure of *the investor's*
  outcome. Solve by bisection or Newton on the NPV function; guard against
  non-convergence and return "not computable" rather than a wrong number. This is
  where a finance app most often quietly lies; a refusal is a legitimate answer.

Cash flows come from `investment_transactions` (buys, sells, deposits,
withdrawals). Deposits and withdrawals are *external* flows; buys and sells
inside the account are not. Getting that distinction wrong makes every return
figure wrong, so test it directly.

Periods: YTD, 1y, 3y, 5y, since inception — clamped to available snapshot
history, with the actual span returned so the UI can caveat it.

### Benchmarks

A daily job populating `asset_prices` from a free end-of-day source (Stooq's CSV
endpoint needs no key; Yahoo's chart endpoint is undocumented and breaks — prefer
the former). **Ship a small fixed default set** (SPY, VTI, BND, QQQ) and make the
job tolerant of a fetch failure: a missing benchmark degrades the comparison
chart, it must never fail the sync or block the page.

This is the app's first outbound call to a non-Plaid, non-AI host. The README
says the app "phones home to nothing but Plaid and (optionally) your AI
provider." Gate the job behind a config flag in
`backend/internal/config/config.go`, default it **off**, and update that README
line — do not silently make the claim false.

### Allocation, fee drag, dividends

- **Allocation** by asset class / sector / geography from the fields
  `convertSecurity` already normalises. Coverage is partial — show an "Unknown"
  slice honestly rather than redistributing it.
- **Target allocation + drift** — a per-household target per band, drift as
  actual − target, and a rebalance helper ("sell $X of A, buy $Y of B"). Compute
  in decimal.
- **Fee drag** — `Σ(expense_ratio × market_value)`, annualised. If Plaid does not
  supply an expense ratio for a holding, exclude it and say how many holdings
  were excluded. A fee number computed over 60% of the portfolio and presented as
  the total is misinformation.
- **Dividend income** — from existing transaction categorisation; a dividend
  category already exists.

### API

New routes under `/api/investments` in `backend/internal/api/server.go`
(the `/networth` block is the pattern to follow), with the same household
ownership and `user_id = $2 OR is_shared` visibility scoping as every other
reporting endpoint. Decimal **strings** over the wire.

## Frontend

New `frontend/src/routes/Investments.tsx` — a fourth top-level data surface
alongside Spending, Net Worth, and Report. Register in `App.tsx` and the nav.
Follow `NetWorth.tsx` for chart conventions (Tremor) and `Spending.tsx` for
period selection.

Sections:

- **Header** — total value, total unrealised gain ($ and %), period selector.
- **Performance** — TWR / IRR / total return per period, portfolio and per
  account, with an explicit "based on N days of history" caveat when thin.
- **Benchmark chart** — portfolio total return vs. selected benchmarks, same
  axes, rebased to 100 at period start.
- **Allocation** — asset class / sector / geography, with target vs. actual and
  drift when a target is set.
- **Holdings table** — ticker, shares, cost basis, value, unrealised gain $ and
  %, expense ratio, yield, last price. Sortable, exportable to CSV (reuse the
  Report page's export helper).
- **Fee drag** — annual dollar cost, with coverage disclosed.
- **Account tagging UI** — the confirm-the-suggestion flow. Put it somewhere
  unmissable while accounts are untagged; doc 15 is blocked on real values here.

## Verification

- `go test -p 1 ./...` against the throwaway Postgres.
- **Return math is the risk.** Test TWR and IRR against hand-computed fixtures
  with known answers, including: a deposit mid-period (TWR must ignore it, IRR
  must not), a full withdrawal, and a non-convergent IRR case that must return
  "not computable" rather than a number.
- Month-end and leap-day snapshot upserts are idempotent.
- Benchmark job: assert a fetch failure degrades to a missing series and does not
  error the job.
- Fee drag with partial expense-ratio coverage reports the excluded count.
- Frontend `tsc -b`, `vite build`, `oxlint`.

## Out of scope

- Live/intraday market data. End-of-day only.
- Tax-lot tracking and realised-gain reporting. Related, larger, and a
  prerequisite for TODO #8's tax summary — not this doc.
- Trading or rebalancing execution. The helper says what to do; the user does it.
- Multi-currency holdings (TODO #13).
