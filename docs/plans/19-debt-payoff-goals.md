# 19 — Debt-payoff goals (+ money formatting)

*(TODO.md "Known gaps" — the two genuine leftovers from the pre-wave-3 list.)*

## Context

Two small, unrelated items bundled because neither justifies its own doc and both
are good warm-up work.

### 1. Debt-payoff goals are schema-only

`00012_goals.sql:15` declares `kind TEXT NOT NULL, -- 'savings' | 'debt_payoff'
(start with 'savings')`. The "start with savings" part is where it stopped. Every
layer above the column handles savings only:

- `goals.Compute` (`backend/internal/goals/goals.go:44`) computes
  `remaining = target − current` and `requiredMonthly = remaining / monthsLeft`.
  That is accumulation maths. It has **no notion of interest**, so applied to a
  debt it understates every figure.
- `ai.ParseGoal` (`backend/internal/ai/parse.go:110-140`) has
  `"kind": {"enum": ["savings"]}` in its schema and a prompt that says
  `kind: always "savings" for now`.
- `frontend/src/routes/Goals.tsx:344` — the input placeholder is literally
  "Describe a savings goal…".

So a user cannot create or track a payoff goal, and the column is a promise the
app does not keep.

The data to do it properly is already there: `liabilities`
(`00004_investments_liabilities.sql:81`) carries `apr`, `balance`,
`minimum_payment`, and `next_payment_due_date`, linked one-to-one to an account.

### 2. `money()` has no thousands separators

`backend/internal/insights/producers.go:28`:

```go
func money(d decimal.Decimal) string { return "$" + d.StringFixed(2) }
```

Every insight body, alert message, and recap line that quotes a four-figure
amount renders `$1234.56`. The frontend formats correctly via `lib/money.ts`;
only server-generated prose is affected. One helper, many call sites.

## AI vs deterministic split

**Deterministic:** all payoff maths — amortization, interest, payoff date,
required payment. Never a model.

**AI:** extending `ParseGoal` to recognise a payoff sentence and emit structured
intent, and phrasing coaching over finished figures. Same rule as everywhere: the
model reads the sentence, it does not compute the schedule.

## Prerequisites

None. Fully independent — a good first task for a fresh instance.

## Data model

**No migration.** `goals.kind` already permits `debt_payoff`, and `goals.account_id`
already links a goal to the account whose `liabilities` row carries the APR.

One judgement call to make and document: for a payoff goal, `target_amount` is
naturally **zero** (pay it to nothing), which reads oddly against a column
described as "a positive target". Prefer treating `target_amount` as *the balance
to eliminate* — set at creation from the current balance — so the existing
`CHECK`-free positive-target convention holds and progress is
`original_balance − current_balance`. Write this down in the migration-free doc
comment on `Compute`, because the next reader will otherwise assume the savings
semantics.

## Backend

### Payoff maths in `backend/internal/goals/`

A new function beside `Compute` — do **not** overload `Compute`, whose doc
comment enumerates savings semantics precisely and is worth keeping intact.

```go
// ComputePayoff derives a debt-payoff goal's standing.
func ComputePayoff(balance, apr, monthlyPayment decimal.Decimal, targetDate *time.Time, now time.Time) PayoffFeasibility
```

Returns: months to payoff at the given payment, total interest paid, payoff date,
and — when a `targetDate` is set — the monthly payment required to hit it.

The maths, in exact decimal:

- Monthly rate = `apr / 100 / 12`. **Note `liabilities.apr` is stored as a
  percentage, not a fraction** (`00004:86` says so explicitly) — the `/100` is
  mandatory and is the obvious bug to write here.
- Standard amortization: each month, `interest = balance × monthlyRate`, then
  `balance = balance + interest − payment`.
- **The non-amortizing case is the one that matters.** If
  `payment ≤ balance × monthlyRate`, the debt never pays off. Return an explicit
  "never" rather than looping to an iteration cap and reporting a large number.
  Someone with a minimum payment below the interest accrual needs to be told
  that plainly — it is the single most useful thing this feature can say.
- Required payment for a target date: closed-form annuity, or bisection over the
  simulation. Bisection reuses the loop and is easier to verify.
- Zero or NULL APR degrades to `balance / months`, which is correct for an
  interest-free debt.

### Parser

Extend `ParseGoal` in `ai/parse.go`: add `debt_payoff` to the `kind` enum, teach
the prompt to recognise "pay off my credit card by December", and return an
optional account/liability hint. Keep the structured-output schema style already
there. When the sentence names a debt the household does not have, return the
unsupported path rather than inventing one — `ParseRule` already models this
behaviour at line 47.

### API

Extend the existing goals handlers to branch on `kind`, returning the payoff
feasibility shape for payoff goals. Progress for a payoff goal is derived from
the linked account's current balance, consistent with the "progress is DERIVED,
never stored" rule in the `00012` header comment — do not add a stored balance.

### `money()` fix

Add a thousands-separator formatter. `golang.org/x/text/message` with
`language.English` handles it correctly, or write a small grouping helper over
`StringFixed(2)` to avoid the dependency — either is fine, but **put it in one
place** and route every call site through it. Check for the same pattern
open-coded elsewhere: `insights/producers.go` is the canonical `money()`, but
grep for `"$" +` across `backend/internal/` before declaring it done.

Negative amounts and amounts under 1000 must be unchanged apart from grouping.

## Frontend

`frontend/src/routes/Goals.tsx`:

- Goal-kind selector on create; the placeholder at line 344 stops saying
  "savings".
- Payoff goals render payoff date, total interest, and required payment rather
  than the savings feasibility fields.
- **Surface the never-pays-off case loudly.** "At $50/month this card is never
  paid off — the interest is $61/month" is the most valuable sentence in the
  feature.
- Account picker restricted to accounts with a `liabilities` row.

## Verification

- `go test -p 1 ./...`.
- **Amortization fixtures with hand-computed answers:** $5,000 at 18.99% APR,
  $200/month — assert the exact month count and total interest. Then the same
  debt at $75/month, which must return "never".
- APR percentage-vs-fraction: a test that would fail if the `/100` were dropped.
  This is the highest-value single test in the doc.
- Zero-APR debt reduces to simple division.
- Required-payment bisection converges, and an unreachable target date returns
  a clear failure rather than an absurd payment.
- `money()`: `$0.00`, `$999.99`, `$1,000.00`, `$1,234.56`, `$1,234,567.89`,
  `-$1,234.56`. Snapshot an insight body end-to-end so the formatting is proven
  where it actually renders.
- Frontend `tsc -b`, `vite build`, `oxlint`.

## Out of scope

- Debt snowball / avalanche strategy across multiple debts. Genuinely useful and
  a natural follow-on; it belongs with the advisor (doc 24), which already ranks
  "pay down card Z (19% APR)" as an option.
- Modelling extra one-off payments.
- Editing APR by hand. It comes from Plaid; a manual override is separate work.
