# 15 — Retirement & FIRE projections

*(TODO.md "Next major initiatives" #5.)*

## Context

`backend/internal/networth/project.go` is honest about what it is: a plain
compounding model whose own comment says it "is an illustration of 'if things
continue like this' … nothing here predicts markets." As a net-worth sanity
check that is fine.

As a **retirement** tool it is actively misleading, and that matters because
"the numbers are honest" is the app's central claim. Specifically, `Project`:

- Applies one `AnnualReturnRate` to a single `invested` pool
  (`start.Breakdown.Investments`), so a 401k, a Roth IRA, a 529, and a taxable
  brokerage all compound identically and are indistinguishable in the output.
- Takes one `MonthlySurplus` with no notion of *where* it lands, so contribution
  limits (401k deferral, IRA, HSA) are invisible and an employer match is
  uncounted.
- Has no withdrawal phase at all, so it cannot answer the only question anyone
  actually asks: *when can I stop working, and does the money last?*

This doc adds an account-aware projection and a withdrawal-rate lens beside the
existing one. It is also the foundation TODO #16 (the what-if scenario engine)
is explicitly blocked on — building that before this means re-implementing
projection logic that gets thrown away.

## AI vs deterministic split

**Deterministic:** every projection, every limit check, every FI-age solve,
every Monte Carlo run. Exact decimal, server-side.

**AI:** presentation only, over finished figures — the pattern
`chat_handlers.go` already enforces. A model must never produce a retirement
number. Someone will make a life decision on this output.

## Prerequisites

**Hard dependency on [14-investments-page.md](14-investments-page.md)** for
`accounts.tax_treatment`. Without it there is nothing to project per account
type and this doc collapses back into the linear model it is replacing.

If 14 is in flight, its tagging migration and API land first; start here on the
projection engine against a stubbed tagging interface, then wire it. Do **not**
duplicate the migration — coordinate, or you will collide on `00020`.

Runs in parallel with 13 and 16 otherwise.

## Data model

**Reserved migration: `00021_projection_assumptions.sql`.**

```sql
-- Per-household projection inputs. Every one is user-visible and user-editable:
-- project.go's existing comment ("a forecast whose workings are hidden is not
-- something anyone should plan around") is the rule, and it applies harder here.
CREATE TABLE projection_assumptions (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id           UUID NOT NULL UNIQUE REFERENCES households (id) ON DELETE CASCADE,
    real_return_rate       NUMERIC(6,4) NOT NULL DEFAULT 0.05,   -- real, not nominal
    inflation_rate         NUMERIC(6,4) NOT NULL DEFAULT 0.03,
    withdrawal_rate        NUMERIC(6,4) NOT NULL DEFAULT 0.04,
    target_retirement_age  INT,
    current_age            INT,
    annual_ss_income       NUMERIC(20,4),   -- expected Social Security / pension
    ss_start_age           INT,
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Per-account contribution plan. Drives both compounding and limit tracking.
CREATE TABLE account_contributions (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id            UUID NOT NULL UNIQUE REFERENCES accounts (id) ON DELETE CASCADE,
    monthly_contribution  NUMERIC(20,4) NOT NULL DEFAULT 0,
    employer_match_pct    NUMERIC(6,4),    -- of salary, not of contribution
    employer_match_limit  NUMERIC(20,4),   -- annual cap on the match
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

**Store `real_return_rate`, and default to real returns throughout.** Mixing
nominal returns with today's dollars is the single most common way retirement
projections lie, and it always lies in the flattering direction. If nominal is
offered as a toggle, label both explicitly and never let the two share an axis
unlabelled. (TODO #12's CPI series, when it lands, makes this switchable
properly.)

**IRS contribution limits do not go in the database.** They change annually and a
stale limit silently produces wrong headroom. Put them in a versioned Go map
keyed by tax year in `backend/internal/networth/`, with the year visible in the
UI ("2026 limits") and a plain fallback when the running year is missing:
say "limits for 2027 aren't configured" rather than silently applying 2026's.

## Backend

New file(s) in `backend/internal/networth/`, beside `project.go`. **Do not
modify `Project`** — the Net Worth page depends on it and its simplicity is a
feature. Add `ProjectByAccount` alongside it and let the two coexist.

### Account-aware accumulation

Project each account separately by `tax_treatment`, then sum:

- Its own balance, contribution (`account_contributions`), and employer match.
- Its contribution limit, from the versioned map. **Cap contributions at the
  limit** rather than projecting an impossible one — an uncapped projection is
  wrong in the flattering direction, again.
- A 529 carries its own horizon (beneficiary age → college age) rather than the
  retirement horizon.

Keep `ProjectionPoint`'s existing split of `Contributed` vs. `Growth` — the
comment at `project.go:31-33` explains why, and per-account output should keep
that property so a user can see how much of the number is saving rather than
assumed return.

### FI / FIRE outputs

Given the assumptions and the accumulation curve, compute:

- Projected nest egg at `target_retirement_age`.
- Supported annual spending = nest egg × `withdrawal_rate`, plus
  `annual_ss_income` from `ss_start_age` onward.
- **FI age** — the first year where supported spending ≥ target spending. Solve
  by scanning the projected series, not by closed form; the series already has to
  exist and a scan is verifiable by eye.
- **Required savings rate** to hit a target FI age — invert the same function by
  bisection. Bound the search and return "not reachable" rather than an absurd
  rate. A 340%-of-income answer is not an answer.

Target spending should default to the household's actual trailing spend — the
app knows it, from `GetSpendingSummary`. Do not make the user guess a number the
app can compute.

### Monte Carlo (optional, gate behind a flag)

N historical-return sequences against the withdrawal phase; report the
percentage surviving 30 years. Two rules:

- **Seed deterministically** and store the seed, or the same inputs give
  different answers on refresh and the number stops being trustworthy.
- Report it as a survival *rate over the modelled sequences*, naming the return
  series used. "95% success" with no stated basis is theatre.

If historical return data is not available offline, ship this behind the flag,
default off, and say so — do not fabricate a return series.

### API

Routes under `/api/projections` in `server.go`, following the `/networth` block.
Household-scoped ownership as everywhere else. Decimal strings over the wire.
Return the assumptions **with** every projection response so the UI can never
render a curve without the inputs that produced it.

## Frontend

Extend `frontend/src/routes/NetWorth.tsx` with a Retirement section, or add a
sibling route if it gets large. Either way:

- **Assumptions panel, always visible, always editable.** Not behind a
  disclosure triangle. The inputs are the honest part.
- Projection chart with the contributed/growth split visible.
- FI-age callout, supported-spending figure, and the required-savings-rate answer
  when a target age is set.
- Per-account contribution editor with limit headroom ("$8,000 of $23,500 used,
  6 pay periods left").
- 529 view when a 529 account is tagged.
- Monte Carlo survival rate when enabled, with its basis stated inline.
- **Untagged accounts** (see doc 14) must be surfaced here as a gap: "3 accounts
  are untagged and excluded from this projection." Silently omitting them
  produces a confidently wrong number.

## Verification

- `go test -p 1 ./...`.
- **Hand-computed fixtures are mandatory here.** A single account, $10,000, 5%
  real, $500/month, 120 months — assert the exact expected decimal. Then the
  same with a contribution limit that binds partway through, and one with an
  employer match at its annual cap.
- FI-age solve: a household already past FI returns age = current age, not a
  negative or a panic.
- Required-savings-rate bisection: assert the unreachable case returns "not
  reachable" and does not loop.
- Monte Carlo: same seed → identical result, twice.
- Limits map: an unconfigured tax year degrades with the stated message rather
  than applying a stale year.
- Frontend `tsc -b`, `vite build`, `oxlint`.

## Out of scope

- Tax-drag modelling on withdrawals (traditional vs. Roth ordering, RMDs). Real,
  large, and dependent on TODO #8's income data. Note the omission in the UI
  rather than approximating it.
- The what-if scenario engine (TODO #16). This doc is its foundation, not its
  delivery.
- Live market data. Assumed real returns only, set by the user.
- Financial advice. The engine computes outcomes; it does not recommend.
