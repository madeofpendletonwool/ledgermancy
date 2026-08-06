# Ledgermancy — status, known gaps, and roadmap

This is the working list. The [README](README.md) is the pitch; the
[docs site](https://madeofpendletonwool.github.io/ledgermancy/) is the reference.
Everything forward-looking and historical lives here.

---

## Current status

**Phases 1–7 complete.** Auth + two-factor, households, Plaid ingest,
categorisation, the spending dashboard, net worth (investments, liabilities,
manual assets, snapshots, projections), and the exportable **Financial Summary**
report are all running. The optional phases are in too: AI enrichment (6) — LLM
categorisation fallback, the proactive insight feed, spending alerts, monthly
narratives, and natural-language budget/goal/alert parsing — and the
tool-calling chatbot (7).

---

## Known gaps

The app is feature-complete for daily use; these are known, deliberate gaps —
not bugs:

- **Debt payoff is single-debt only.** Payoff goals themselves now work end to
  end (`goals.ComputePayoff`, the parser, the Goals UI). What is still missing is
  strategy *across* debts — snowball vs. avalanche ordering, and modelling extra
  one-off payments. That belongs with the proactive advisor, which already ranks
  "pay down card Z (19% APR)" as an option.
- **An item's transaction history window cannot be widened after linking.**
  Plaid fixes it at link time; update mode preserves it but cannot raise it, and
  relinking orphans the history already tied to the old `plaid_item_id`. Where
  an institution caps what it shares (Capital One's 90 days, for example), the
  CSV importer is the only way to backfill further.

---

## Roadmap

### Recently shipped

- **The Advisor (wave 6)** — the old reactive Assistant became an Advisor
  surface, and the whole proactive-advisor initiative landed with it. Four
  pieces, four plan docs:

  - **Advisor surface** (doc 31) — a deterministic **Briefing** (net worth,
    slack, FI age, debt-free date, emergency-fund runway, top attention items),
    saved **Threads** with sealed transcripts, and an action-items tray. ~12
    existing engines are now reachable from the chat as tools, sent one
    tool-set per request (spending / planning / modelling) so ~34 definitions
    never drown a single turn. Migration `00054_advisor_surface.sql`
    (`advisor_threads`, `advisor_messages`, `advisor_action_items`, plus
    `households.filing_status` / `risk_drawdown_floor`). `advisor_messages`
    bodies are `BYTEA` under `ENCRYPTION_KEY`, so the portable export withholds
    them by type while `pg_dump` recovers them whole.
  - **Proactive cash-flow advisor** (doc 24, *TODO #3*) — ranked, deterministic
    options for surplus cash under a published **waterfall** (starter EF →
    unclaimed employer match → debt above a hurdle derived from the household's
    own assumed return → full EF → expiring tax-advantaged headroom → goals →
    below-hurdle as a stated tradeoff). Slack is `reporting.BuildSafeToSpend`'s
    median-based figure, not the "income so far + projected income" #3's
    description still prints — that predates the engine. No migration; the
    model narrates, never computes.
  - **Multi-bucket allocation planner** (doc 32) — split a lump and/or monthly
    surplus across Roth / 529 / brokerage / debt / emergency fund with
    per-bucket projection, contribution-cap enforcement, **Roth/HSA eligibility
    (a cap is not permission)**, a four-year college drawdown, cash-drag against
    your own best rate, and asset-location as disclosure. Migration
    `00055_allocation_planner.sql` — gained `goals.college_years` and
    `households.magi` / `magi_tax_year` beyond the plan's SQL block, the latter
    so a stale MAGI reads as `unknown` rather than being silently reused.
  - **Likelihood layer** (doc 33) — Monte Carlo over allocation plans
    (P10/P50/P90, success rate, P5 drawdown), a documented guardrail rule that
    names a top plan from computed likelihoods, and plan-vs-actual tracking.
    **Buckets are modelled as moving together** (independent draws would
    under-count correlated equity risk and inflate every success rate), the P5
    drawdown is a stable percentile not a maximum, and the two "P50" figures
    (compound-at-μ vs median simulated) are labelled distinctly. Migration
    `00056_likelihood_layer.sql` — one table, `plan_trackings`; simulation
    results are never persisted.

  Commit `4f38852 "wave 6 complete"`. Every table is classified `InExport` in
  `continuity/coverage.go`. The whole initiative honors the one rule the
  surface establishes: **AI never computes; tools compute, the model narrates.**

- **Predictive anomaly detection** — two producers on the existing insight spine,
  `merchant_outlier` and `duplicate_charge` (`insights/anomaly.go`), over
  `db/queries/anomaly.sql`. Migration `00046_anomaly_overrides.sql`.

  The load-bearing decision is that **there is no `merchant_baselines` table.**
  A baseline used to judge a charge has to be leave-one-out — computed excluding
  the candidate — or `PERCENTILE_DISC(0.95)` over a merchant whose largest-ever
  charge *is* the candidate returns the candidate and the detector fires on
  everything. Mean and count are subtractable (so leave-one-out is recoverable
  from them) but **median and p95 are order statistics and are not**, so the
  candidate can never be backed out afterwards. A stored table would be a cache
  that is wrong, not merely redundant. Baselines are computed on demand in a
  `CROSS JOIN LATERAL`.

  `large_transaction` and `merchant_outlier` are two halves of one behaviour:
  the per-merchant outlier claims any merchant with 5+ prior charges (its message
  strictly dominates), and `large_transaction` covers everything else — which is
  where most genuine fraud lands. Dedupe keys are transaction ids rather than
  merchant-keyed, so merging a merchant later does not resurrect a dismissed
  insight.

- **Pre-tax income & deduction tracking (paystub importer)** — closes the 30–45%
  of gross income the bank feed never sees. Migration `00048_paystubs.sql`
  (`employers`, `paystubs`, `paystub_lines`). PDF import is local text-layer
  extraction with **no network call and no model**; a scanned stub is refused
  rather than sent off-host. A stub is inert until confirmed, and confirmation
  requires it to balance — `gross − Σdeductions = net` within a cent. See
  [Paystubs](docs/features/paystubs.md).

  Two correctness rules worth naming. An employer 401(k) match is recorded
  separately and is *not* part of that equation (it is money added on top of
  gross, not taken out). And **two jobs in one year share one 401(k) limit** —
  naive year-to-date addition reports roughly twice the room you have, so
  contributions are pooled against a single cap and going over is shown loudly.
  The EIN is stored sealed (`ein_encrypted BYTEA`); the tax summary is the only
  endpoint that returns a full one, and the disclaimer travels in the payload.

- **In-app digest** — the weekly / monthly recap is persisted to `digest_entries`
  and kept on its own page, with push and SMTP as optional surfaces beside it
  rather than the only ones. Migration `00049_digest_entries.sql`. A push that
  fails no longer loses the digest: the entry is written **before** delivery is
  attempted, so the content survives an unreachable ntfy server or a mail server
  that is down. Digests are write-once — nothing recomputes a stored one, so a
  digest that disagrees with today's Spending page is correct rather than stale.

- **Real-asset revaluation & depreciation** — `manual_assets` gain an append-only
  value history (`asset_valuations`), class-specific detail (`asset_details`),
  and directly-held bonds. Migration `00051_asset_revaluation.sql`, with
  `savings_bond_rates` seeded from treasurydirect.gov.

  The defining rule: **an estimate is a proposal, never a write.** A vehicle is
  depreciated along a published curve (≈20% the first year, 15% of the remainder
  after, with a mileage tilt and a salvage floor) and the page shows the figure,
  the curve, and every input — then waits for you to accept it. Bonds are the one
  exception, because a savings bond's value is arithmetic over published rates,
  not a judgement: a Series I (fixed + inflation, semiannual compounding,
  three-month forfeiture before five years) or EE (guaranteed to double at 20
  years — a cliff, not a curve) accrues to its exact redemption value, and the
  rate table names where each row came from.

- **Inflation-adjusted views** — every long-horizon chart can be switched into
  real dollars against a bundled CPI-U series. Migration `00052_cpi_series.sql`,
  seeded January 2010 onward. Nominal stays the default and the choice is
  remembered per user; the base month is always stated ("in June 2026 dollars",
  never "today's"). The series has a permanent hole — **October 2025 was never
  published** — and a point dated there is dropped and counted rather than
  interpolated. Returns are deflated by division, not subtraction (subtraction
  is wrong by the product of the two and always flatters). See
  [Concepts](docs/concepts.md#real-dollars-and-nominal-dollars).

- **Manual accounts, manual investments, scheduled transactions** — accounts
  Plaid cannot link (TreasuryDirect, a Voya plan, a private holding) now exist as
  first-class accounts with full Investments-page parity. Migration
  `00053_manual_accounts.sql`. The defining decision was to **relax the existing
  tables** (a `source` column, NULLable Plaid ids) rather than build parallel
  manual ones, so every report query "just works" — the engines filter on
  visibility, not Plaid identity. See [Accounts → Accounts without
  Plaid](docs/features/accounts.md#accounts-without-plaid).

  Manual balances are the first user-owned balance-write path, and every change
  is paired with an `account_balance_history` row in the same transaction. A
  scheduled obligation can auto-post as a transaction **and** adjust the manual
  balance — the Voya monthly contribution case — atomically. The manual
  endpoints refuse a linked account's id outright; there is deliberately no merge
  path back to Plaid.

- **Installable PWA with read-only offline** — a manifest, a maskable icon, and
  a service worker that precaches the app shell and keeps the most recent
  read-only API responses, so the app installs to a home screen and still opens
  and renders in a tunnel. Frontend only; no backend change.

  The interesting decisions are all about honesty rather than plumbing. API
  caching is network-first and never cache-first, so a saved figure only ever
  appears when the request genuinely failed; every stored response is stamped
  with the time it was stored, and a banner under the header quotes that time
  back rather than showing a tasteful cloud icon. The cacheable set is an
  allowlist, so a route added later gets no offline copy until someone decides
  it should — the failure mode of forgetting is an empty screen, not data on
  disk that should never have been there. `/api/documents/*` is excluded
  outright: the vault decrypts on read, and caching the plaintext would undo
  the feature. Writes are refused with a sentence the user can act on and are
  explicitly **not** queued — replaying a stale recategorisation against a
  changed ledger is a correctness problem, and half of it is worse than none.

  Two things that only show up in the real deployment: sign-out clears the
  worker's caches as well as the query cache, or a shared device leaks the last
  user's balances to whoever pulls the plug next; and nginx serves `/sw.js` and
  the manifest `no-cache`, without which installed clients pin themselves to an
  old build forever.

- **Encrypted document vault** — a `/documents` surface storing receipts, tax
  returns, warranties, policies and contracts sealed with the existing AES-GCM
  cipher, linked to transactions, manual assets, accounts and goals, with an
  insight that fires before a warranty or policy expires. Storage is an
  interface: a mounted volume by default, or any S3-compatible bucket (SigV4
  signed directly — three verbs did not justify an SDK).

  The security work is where the substance is, and each piece closes a named
  hole rather than being defence in depth. Storage keys are generated UUIDs, so
  a filename of `../../etc/passwd` was never a path in the first place. Every
  route including the download resolves the row scoped to household *and* user,
  and misses return 404 rather than 403 — a 403 would confirm an id exists in
  someone else's vault. Downloads sniff the decrypted bytes against a
  five-entry allowlist instead of echoing the uploader's MIME type, always with
  an attachment disposition and `nosniff`, so an HTML file filed as a "receipt"
  downloads rather than executing on the app's origin. And reads verify a
  SHA-256 of the plaintext, which catches the one failure GCM cannot: a storage
  mixup serving the wrong, perfectly intact blob.

  Two limits are load-bearing rather than tuneable. Sealing is whole-buffer, so
  the per-file cap is what turns "too big" into a 413 instead of an OOM; values
  above 100 MB are refused at startup. The per-household quota counts private
  uploads too, because what is being rationed is the operator's disk.

  Receipt OCR ships behind its own switch, separate from `AI_API_KEY`, and is
  suggestion-only: there is no code path from a model reading a receipt to a
  written row. It offers the fields and the transactions the amount and date
  could match, with one click to attach the receipt to the one you pick.

  Crucially it is gated on `doc_type` as well as on the switch: **only
  documents filed as `receipt` can be sent**, refused server-side before the
  file is decrypted. Tax documents, policies, contracts, statements and the
  `other` bucket are ineligible whatever format they are in — a W-2 scanned to
  a PNG is as refused as a PDF of one. It is an allowlist of one entry rather
  than a blocklist, so a doc type added by a later migration is ineligible by
  default rather than sendable until somebody remembers it.

  The reading is **cached on the document** (migration `00033`), which is what
  makes the feature usable rather than a demo. A receipt is scanned at the
  register and the card charge posts days later, so a match run only at scan
  time finds nothing — and the first cut had no way to look again without
  re-uploading the image. Now the fields are stored, matching is free
  deterministic SQL that re-runs whenever the receipt is opened and in an hourly
  producer, and the charge surfaces in the feed when it lands. Matching also
  reads `authorized_date` — the swipe date, which is what the receipt prints —
  so a late posting lines up exactly rather than leaning on the ±5 day window.
  Pending rows are excluded because they are deleted when they post, which
  would silently take the attachment with them.

  Nothing is ever auto-deleted. Retention is an advisory "keep until" date per
  type, surfaced in the UI, permanently. And the document volume is **not** in
  `pg_dump` — DEPLOYING.md now carries the three-part restore (dump, volume,
  key) that this creates.

- **Smart merchant canonicalization** — a `/merchants` surface over
  `merchant_entities` / `merchant_aliases`, layered on top of
  `plaid.MerchantKey` rather than replacing it. Every reporting query that
  groups by merchant now groups by the *resolved* merchant, so a subscription
  billing under two descriptors is detected as recurring for the first time, and
  a second descriptor of a years-old merchant stops firing a false
  "new merchant" alert.

  The design turns on one rule: **a suggestion is inert.** Both passes — the
  deterministic one (token overlap, prefix containment, and vowel-dropped
  abbreviation, all pure string work) and the AI one that sees only the residue
  — write aliases with `source='suggested'`, and every reporting query excludes
  those. A pass therefore cannot move a number; it can only fill a review queue.
  That is what makes it safe to run unattended and is asserted directly: a test
  seeds a suggested alias and checks the reports are unchanged.

  Three places it refuses to be clever. Rejections are remembered, and because
  transitivity could otherwise re-form a merge through a third descriptor, a
  whole component containing a rejected pair is dropped rather than proposed.
  Merging reconciles the fragments' cached categories, but two *different*
  manual categories are reported as a conflict and nothing is touched — the
  "manual is sticky" rule outranks any merge. And splitting is a first-class
  action, because an over-eager merge will happen and one nobody can undo is
  worse than one that never happened.

- **Retirement & FIRE projections** — a `/retirement` surface built on an
  account-aware engine (`networth/retirement.go`) that sits *beside*
  `networth/project.go` rather than replacing it. Each account compounds on its
  own terms at a **real** return rate, so every figure is in today's dollars;
  contributions are held at their IRS limit, pooled across accounts that share
  one (two 401(k)s do not each get $24,500); an employer match is a percentage
  of salary bounded by both the plan's annual cap and what was actually
  deferred; and a 529 runs to its beneficiary's college horizon and is never
  counted as retirement money. FI age is found by scanning the projected series,
  and the required-savings-rate solve is a **bounded** bisection that answers
  "not reachable" instead of printing an absurd rate.

  Four places it refuses to flatter. Untagged accounts are **excluded and
  named**, with the value they hold, because silently omitting an account
  produces a confidently wrong number. An unconfigured tax year reports itself
  and projects uncapped rather than applying a stale limit. Every response
  carries the assumptions that produced it, so a client cannot render a curve
  without its inputs. And what the model does not do — tax on withdrawals, RMDs,
  return variability — is listed on the page rather than left to be discovered.

  The withdrawal-phase simulation is behind `RETIREMENT_MONTE_CARLO_ENABLED`,
  default off. Its sequences are drawn around the user's own stated return and
  volatility, not a historical backtest: bundling a market history would mean
  either an outbound fetch the README promises not to make or a table of numbers
  transcribed into source. Seeds are derived from the inputs, so the same
  scenario always gives the same answer.

- **Dedicated Investments page** — a fourth top-level data surface
  (`/investments`) over holdings that were being ingested and then shown as a
  single line in Net Worth. Time-weighted and money-weighted (IRR) returns, both
  computed in exact decimal in `reporting/returns.go`; a rebased growth chart
  with the household's own deposits stripped out, against optional benchmarks;
  allocation by asset class and by tax treatment; a sortable, CSV-exportable
  holdings table; and dividend income.

  Three deliberate refusals, because this is where a finance app most easily
  lies. The IRR solver **returns "not computable" rather than a number** when
  the cash flows do not bracket a root. A return is **never annualised below a
  year** of history. And the fee-drag endpoint reports **full exclusion** — no
  expense-ratio source exists, and a fee number computed over part of a
  portfolio and presented as the total is worse than none.

  Two supporting pieces landed with it. `investment_transactions` was a table
  nothing ever wrote to; `plaid.GetInvestmentTransactions` now populates it,
  which is what lets a return separate market movement from money the user paid
  in. And `accounts.tax_treatment` (migration `00020`) is the user-confirmed
  classification the FIRE projections need — nullable on purpose, suggested from
  the Plaid subtype only where that subtype is unambiguous, and never written
  without an explicit choice. Plaid reports a Roth 401(k) and a traditional one
  identically, and a wrong tag there silently changes every retirement figure
  built on it.

  Two pieces of the original scope stayed out: **target allocation + drift and
  the rebalance helper** (they need a stored per-household target nothing else
  wants yet, and the allocation view is honest without them), and **daily
  benchmark prices are opt-in** — `BENCHMARK_PRICES_ENABLED`, default off, since
  this is the app's only outbound call to a host that is neither Plaid nor the
  AI provider.
- **Bill calendar + cash-flow forecast** — a `recurring_obligations` table
  (migration `00019`) that persists what is *due next*, from two sources: rows
  promoted from the recurring detector (`obligations.Promote`, idempotent, and
  it never overwrites a row a user has edited) and manually-entered bills, which
  are the only way an annual premium or anything paid by cheque can be known.
  Occurrences are derived, never stored: one SQL expansion
  (`ListUpcomingObligations`) backs the calendar, the list, the balance
  projection, safe-to-spend and the insight, so they cannot disagree about a due
  date. Cadence arithmetic is in Postgres because its interval addition clamps
  month ends and Go's `time.AddDate` does not. New `/schedule` page (month grid,
  30/60/90-day list, per-account projected-balance chart with a visible zero
  line), a "due this week" strip on the Dashboard, a `safe_to_spend_after_bills`
  figure that splits the fixed component per category so no bill is counted
  twice, an `upcoming_bill` insight, and a forward-looking
  `predicted_low_balance` alert type.
- **Reconnect a disconnected institution** without losing its history — a
  `login_required` / `revoked` item opens Plaid Link in **update mode**, which
  repairs the item in place and keeps its transactions and its history window
  (`plaid.CreateUpdateLinkToken`, `POST /api/plaid/items/{id}/reconnected`,
  `frontend/src/components/ReconnectAccount.tsx`).
- Custom categories can be typed **spending / income / transfer** (a transfer is
  excluded from spending, which fixes card payments and self-transfers inflating
  spend).
- A transfer/card-payment **detection heuristic** at ingest for the cases Plaid
  returns as `OTHER_OTHER`.
- A duplicate-category guard.
- **Transactions** filtering by category and by **multiple accounts**, with
  URL-driven filters.
- **Click a day or a category** in the dashboard/spending charts to drill into
  those transactions.
- Period-scoped **insights auto-expire** once their month passes.
- A generic **CSV importer** (map your bank's columns, single signed amount or
  separate debit/credit) that de-duplicates against synced data and runs imports
  through the same categoriser — for backfilling history older than Plaid's
  window.
- **Monthly recap overhaul** — the model is fed a real breakdown (per-category
  vs. typical, biggest transactions, savings rate) rather than raw totals, writes
  in the present tense for an in-progress month and the past tense once the month
  closes, and the in-progress recap refreshes weekly (`ai/summary.go`,
  `jobs/summary.go`).
- **Smarter recurring detection** — a per-merchant **"not recurring"** override
  enforced inside `GetRecurringMerchants` itself, so the Spending table, the
  insight producers, the recap, and the chat tool all honour it at once
  (migration `00016`); a recency gate (`activeCutoff`) that drops paid-off items;
  and a cadence gate (gap stddev + a 45-day minimum span) so a coincidental
  cluster isn't flagged.
- **Insight expansion** — projected month-end cash flow, unusually-large single
  transaction, income-change detection, savings-rate milestones, goal-progress
  nudges (`insights/expansion.go`, `forecast.go`, `goal.go`), **plus real-time
  insight push**: `GenerateInsightsWorker.dispatchInsightPushes`
  (`jobs/jobs.go`) enqueues a notify job per newly-inserted insight above a
  priority threshold, mirroring the alert dispatch.
- **Budget expansion** — a **"safe to spend"** figure
  (`reporting/safetospend.go`), **rollover / envelope** budgets (migration
  `00017`), non-monthly periods — weekly and annual (migration `00018`),
  percentage / zero-based allocation (`Budgets.tsx`), and a budget-vs-actual
  trend (`insights/budgettrend.go`).

### Still planned

_(Nothing outstanding here — thousands separators in generated prose now come
from the shared `backend/internal/moneyfmt` helper, which every insight, recap
and alert body routes through.)_

---

## Next major initiatives

The items above are incremental polish on what is already shipped. Everything
below is the next tier: features that move the app from "decent self-hosted
finance tool" to best-in-class, including head-to-head with closed-source
competitors like Rocket Money. Grouped by theme. Each entry names the problem,
a concrete scope, and where it plugs into the existing code so work can start
without re-discovering the codebase.

This list is the working backlog for the next development cycle. Items are
ordered by theme, not strict priority, but the highest-leverage starting points
are the bill calendar/cash-flow forecast, the dedicated investments page, and
merchant canonicalization — the first two because they are the most visible
gaps versus competitor products, the third because it makes every existing
feature better as a side effect.

> **Every item below now has an execution-ready plan doc** in
> [`docs/plans/`](docs/plans/) (docs 13–33), each scoped so one agent can pick it
> up cold: data model, reserved migration number, backend/frontend work,
> verification, and out-of-scope. See [`docs/plans/README.md`](docs/plans/README.md)
> for the wave order, the dependency graph, and the migration reservation table.
> Where a plan doc's Context section contradicts the description below, **the
> plan doc is right** — it was checked against the code, and a few of the
> descriptions here predate work that has since shipped.

### Forward-looking money intelligence

#### 1. Bill calendar + cash-flow forecast — **shipped**

Delivered as described below; see "Recently shipped" for what landed and where.
Two pieces of the original scope stayed out on purpose: the projection covers
depository accounts only (running it over a credit card would subtract that
card's own bills from the balance they make up), and it models known obligations
only — a discretionary-spending forecast would be a guess wearing a number's
clothes. Merchant canonicalization (#6) would improve promotion accuracy and is
still its own item.

**Problem.** The app looks backward well and forward poorly. Recurring
transactions and subscriptions are detected (`backend/internal/insights/subscription.go`)
and surfaced in the Spending "Recurring" table and in Insights, but there is no
unified view of *upcoming* obligations, no day-by-day account-balance
projection, and no integration between known upcoming bills and the budget /
"safe to spend" figures. A surprise autopay is the most common personal-finance
failure mode, and this app already holds the data to prevent it. Competitor
note: Rocket Money's "Schedules" is exactly this view and it is one of their
highest-converting features.

**Scope.**

- A new **Recurring Obligations** data model that unifies two sources into one
  table:
  - Auto-detected recurring transactions (promote the in-memory detection in
    `insights/subscription.go` into persisted rows so a detected cadence is
    remembered and editable).
  - **Manually-entered recurring obligations** — amount, cadence (every N
    days/weeks/months/years), start date, optional end date, category, account,
    optional merchant. This is essential because many real bills do not show up
    in synced data: annual dues, biennial renewals (e.g. a two-year Proton
    subscription), insurance premiums paid offline, anything paid by check or
    bank transfer the Plaid feed sees as an undifferentiated ACH.
- A new **Schedule / Calendar** page in the frontend (`frontend/src/routes/`)
  with:
  - A month-grid calendar showing each upcoming bill on its due day.
  - A list view ("next 30/60/90 days") with merchant, amount, account, days
    until due.
  - A **day-by-day projected balance** per account, computed as
    `current_balance − sum(known obligations through that day)`.
- **Budget integration.** The "safe to spend" figure and the monthly budget
  pace must factor in known upcoming bills for the current period. Concretely:
  "you have $X left to spend on discretionary this month" should already have
  the $1,200 mortgage and the $80 phone bill deducted even if those transactions
  have not landed yet. This is what makes the budget honest about what is
  actually spendable.
- **New insight and alert types**, wired through the existing engines:
  - An "upcoming bill" insight that fires N days before a known obligation
    (`backend/internal/insights/engine.go`).
  - A **predicted-low-balance** alert rule type that fires when the projected
    balance for any account drops below a threshold on any day in the forecast
    window (`backend/internal/alerts/alerts.go`). Mirrors the existing alert
    dispatch path.
- Surfacing in the Dashboard: a "bills due this week" strip and a "you are
  projected to actually spend $X this month" callout that includes known bills.

**Ties into.** `insights/subscription.go`, `insights/engine.go`,
`alerts/alerts.go`, `reporting/safetospend.go` (the safe-to-spend calc), the
Budgets page, the Dashboard. A new `recurring_obligations` table and a new
`balance_projection` query are the main new data structures; everything else
is wiring into existing producers and views.

#### 2. Predictive anomaly detection — **shipped**

**Problem.** Insights currently flags "spikes" as a category-level spend
increase. That misses the most actionable anomalies: a subscription that crept
from $9.99 to $12.99, a duplicate charge from the same merchant within 24 hours,
a transaction 3x the merchant's historical typical amount. These are the
patterns that catch real fraud and billing mistakes — exactly the kind of thing
a user wants the app to notice for them.

**Scope.**

- Per-merchant statistical baselines built from existing transaction history
  (rolling mean and stddev over the last N transactions, or a seasonal model
  for merchants with strong cadence). Persist alongside the existing
  `merchant_category_map` cache — one more column family, same access pattern.
- New insight types emitted by `insights/engine.go`:
  - **Price creep** — a recurring merchant's amount has stepped up; show old,
    new, and delta.
  - **Outlier single charge** — a transaction whose amount is statistically
    implausible for its merchant (configurable sensitivity).
  - **Possible duplicate** — same merchant, same amount (or near-same), within
    a short window.
- These are deterministic rules with statistics, not LLM calls — they fit the
  "deterministic before AI" rule in the README. The LLM is only invoked, if at
  all, to write the human-readable explanation of an already-detected anomaly.

**Ties into.** `insights/engine.go`, `insights/producers.go`, the
`merchant_category_map` table, the Insights UI. Foundational for #6 (proactive
advisor) which will surface the same signals in plain English.

#### 3. Proactive cash-flow advisor — **shipped**

Delivered as described below; see "Recently shipped" for what landed. Two
corrections to the description here: slack is the median-based
`reporting.BuildSafeToSpend` figure (bill-aware when obligations are in view),
**not** the "income so far + projected income" the Scope section below
describes — that formula predates the engine and is wrong; and the ranking is an
explicit **waterfall** (starter EF → unclaimed employer match → debt above a
hurdle → full EF → expiring tax-advantaged headroom → goals → below-hurdle as a
stated tradeoff), not "guaranteed return first," which would have paid down a
3.5% mortgage ahead of a Roth and drained an emergency fund into a card. The
broader initiative — the Advisor surface, the allocation planner, and the
likelihood layer — landed alongside it in plan docs 31, 32, and 33.

**Problem.** The assistant answers questions when asked, but the most useful
money advice is unsolicited and contextual: "you have $400 of slack this month,
here are three things you could do with it." Today nothing nudges the user
toward a good decision with their spare cash; the data to do that nudging is
already in the app.

**Scope.**

- A weekly advisor pass (run as a job in `backend/internal/jobs/`) that
  computes slack = (income so far + projected income) − (fixed obligations,
  per #1) − (budgeted discretionary) − (scheduled goal contributions), and
  produces ranked, deterministic options:
  - "Boost retirement by $X — at your current savings rate and an assumed 6%
    real return, this moves FI age forward by N months" (uses #5).
  - "Pay down card Z (19% APR) — highest-guaranteed-return option."
  - "Move $W to your emergency-fund goal — completes it in M months."
- The LLM's only job is to present these deterministic options in clean prose,
  exactly the way it currently presents tool-call results in
  `backend/internal/api/chat_handlers.go`. The math is computed server-side in
  exact decimal — never LLM-generated — preserving the "auditable, not
  hallucinated" stance from the chat system prompt.
- Surfacing: a weekly advisor digest entry (see #10), an "advisor" panel on
  the Dashboard when slack exceeds a threshold, and as a proactive insight.

**Ties into.** `chat_handlers.go` (system prompt rules already forbid
LLM-invented arithmetic — extend them), `insights/engine.go`, `jobs/digest.go`,
`reporting/safetospend.go`, #1 (for the fixed-obligations input), #5 (for the
FI-age math), goals and budgets.

### Investments & retirement

#### 4. Dedicated Investments page + performance analysis — **shipped**

Delivered as described below; see "Recently shipped" for what landed, what was
deliberately left out, and the two corrections to the description here: there is
no `dividends` category (dividends come from `investment_transactions` by
subtype), and `investment_transactions` was not actually being populated until
this work added the Plaid ingest for it.

**Problem.** Plaid investment holdings are ingested (`backend/internal/plaid/investments.go`)
and shown as a line item in Net Worth, but there is no dedicated investments
surface. For users with real money this is where they graduate from
"budgeting" to "tracking net worth seriously," and Empower / Personal Capital
own that market by having deep investment analysis. We pull the holdings; we
just do not do the analysis.

**Scope.** A new **Investments** route (`frontend/src/routes/Investments.tsx`)
and supporting API/reporting layer, with:

- **Account-type awareness.** Tag each linked investment account as: taxable
  brokerage, traditional 401k, Roth 401k, traditional IRA, Roth IRA, 529, HSA,
  trust, managed vs. self-directed. These tags drive both the display grouping
  and the retirement projection (#5).
- **Performance view.** Total return, time-weighted return, and money-weighted
  (IRR) for the portfolio as a whole and per account, over user-selected
  periods (YTD, 1y, 3y, 5y, since inception). Requires a new
  `investment_snapshots` daily/weekly series analogous to the existing
  net-worth snapshots.
- **Benchmark comparison.** A new `asset_prices` table populated by a daily
  job (SPY, VTI, BND, etc., from a free end-of-day source). Plot portfolio
  total return against chosen benchmarks on the same chart.
- **Allocation analysis.** Breakdown by asset class, sector, and geography
  (Plaid supplies sector/asset class for many holdings). Optional user-defined
  target allocation with **drift %** per band and a rebalance helper ("to hit
  your target, sell $X of A, buy $Y of B").
- **Holdings table.** Ticker, shares, cost basis, current value, unrealized
  gain ($ and %), expense ratio, dividend yield, last price. Sortable and
  exportable.
- **Fee drag.** Sum of `expense_ratio × balance` across holdings, annualized:
  "you are paying $X per year in fund expenses." Surprising and actionable.
- **Dividend income tracking.** A view of dividends received over time, as an
  income stream (uses existing transaction categorization — dividends are
  already a category).

**Ties into.** `plaid/investments.go`, `networth/` (extend the snapshot
machinery to investment accounts), `reporting/`, a new `asset_prices` table
and a new daily job in `backend/internal/jobs/`. Frontend gets a fourth
top-level data surface alongside Spending, Net Worth, and Report.

#### 5. Retirement & FIRE projections, including 529s — **shipped**

Delivered as described below; see "Recently shipped" for what landed and what
stayed out. Two notes on the description: the withdrawal-phase simulation ships
**behind a flag, default off** (`RETIREMENT_MONTE_CARLO_ENABLED`) because no
historical return series is bundled to run it against, and tax drag on
withdrawals is explicitly not modelled — it is stated as an omission in the UI
rather than approximated, and it wants #8's paystub data to do properly.

**Problem.** `backend/internal/networth/project.go` does a linear net-worth
extrapolation. That is fine as a sanity check but actively misleading as a
retirement-planning tool, and "is the math honest" is part of the brand. Real
retirement projection needs account-type-aware compounding, contribution
limits, and a withdrawal-rate lens.

**Scope.**

- **Account-aware projection.** Project each account type separately using its
  contribution pattern, assumed real return, and (later) tax treatment: 401k
  with annual contribution limit and employer match, IRA with limit, Roth with
  limit, taxable, 529 (with its own contribution target and age-based horizon),
  HSA. Sum to household net worth at each future year.
- **FI / FIRE outputs.** Given current savings rate, assumed real return, and
  target withdrawal rate (4% default), compute: projected nest egg at target
  age, supported annual spending at that nest egg, FI age given current rate,
  and the savings-rate change required to hit a target FI age.
- **Monte Carlo on sequence-of-returns risk** (optional, more advanced): run
  N historical-return sequences against the projected withdrawal phase and
  report the percentage that survive for 30 years. Honest about the inputs.
- **529 planning.** Given a beneficiary's age and a contribution plan, project
  the balance at college age; show whether the plan is on track for a target
  tuition.
- Inputs surfaced as editable assumptions (expected real return, retirement
  age, social security / pension expectations, target withdrawal rate), with
  the app's deterministic projections clearly distinct from any LLM narrative.

**Ties into.** `networth/project.go` (extend or fork), the account-type tags
from #4, goals (a FIRE goal is just a retirement goal with a withdrawal-rate
horizon — reuse the goals machinery), the assistant (can answer "when can I
retire" via tool calls over these projections).

### Data quality

#### 6. Smart merchant canonicalization — **shipped**

Delivered as described below; see "Recently shipped" for what landed. One note
on the description: entity logos stayed out deliberately — they are an outbound
dependency the app otherwise does not have. They have since landed as an
explicitly opt-in one (MAD-38, `MERCHANT_LOGOS_ENABLED`, off by default), which
is the shape that reservation was asking for rather than a reversal of it: the
default imagery is still the locally-generated monogram, and the browser still
contacts nothing.

**Problem.** Every finance app suffers from merchant-string fragmentation:
`AMAZON.COM AMZN.COM/BILL WA`, `AMZ*ORDER 1234`, `AMAZON.COM*ABCD1` are all
Amazon. Today these are three separate merchants in the app, which silently
degrades Insights (recurring detection, top-merchants), Budgets, the export,
and #2 (anomaly detection). Solving this once pays off everywhere.

**Scope.**

- A new `merchant_entities` table: canonical name, optional logo/color, default
  category hint, linked category. A many-to-one mapping from raw transaction
  merchant strings to entities.
- **Auto-suggestion engine.** Deterministic first (fuzzy match on normalized
  merchant strings — strip punctuation, lowercase, keyword overlap), then LLM
  for the long tail. The LLM output is a *suggestion* queued for user
  confirmation, never an automatic merge — mirroring the categorisation
  pattern in `backend/internal/categorize/llm.go` where LLM answers are cached
  and surfaced for review.
- **Merge UI.** Either a new "Merchants" page or a section in Categories:
  review suggested merges, confirm or reject, manually merge/split entities,
  set canonical names. After a merge, every downstream view (Insights, Spending,
  Budgets, exports) immediately benefits.
- The merchant entity becomes the unit of analysis for #2 (anomaly detection
  builds baselines per entity, not per raw string), recurring detection, and
  the "top merchants" view.

**Ties into.** `categorize/llm.go`, `insights/subscription.go`,
`alerts/alerts.go`, every reporting query that groups by merchant. A
near-universal side-effect win.

### Ownership & documents

#### 7. Encrypted document vault — **shipped**

Delivered as described below; see "Recently shipped" for what landed. Two notes
on the description. Full-text search *inside* documents stayed out — it wants an
index and OCR over everything, which is separate work — and the OCR that did
ship is suggestion-only, with no path from a model's reading to a written row.

**Problem.** Self-hosters chose self-hosting because they want to own their
data. Today the app owns transaction data but nothing else — receipts, tax
documents, warranty PDFs, insurance policies all live scattered across a NAS,
a cloud drive, an email inbox. Consolidating these next to the financial data
they relate to (with the same AES-GCM encryption-at-rest already used for
  Plaid tokens) is the most on-brand feature possible and a clear differentiator
versus cloud competitors who would never let you store arbitrary documents.

**Scope.**

- Encrypted blob storage, reusing the existing AES-GCM cipher
  (`backend/internal/crypto/crypto.go`). Documents are encrypted with the
  household `ENCRYPTION_KEY` before being written to the storage backend, and
  decrypted on read.
- **Document types:** receipt, tax document, warranty, insurance policy,
  contract, account statement, other. Each document links to zero or more of:
  a transaction, a manual asset, an account, a goal. Standalone (unlinked) is
  allowed.
- **Storage backend:** local filesystem under a mounted volume (default,
  matches the Docker Compose deploy model). Optional S3-compatible backend for
  users who want offhost durability.
- **Optional receipt OCR** (AI-powered, off by default): extract amount, date,
  merchant from receipt images and pre-fill a manual transaction or match
  against an existing synced transaction.
- **Retention tags.** Tax documents: keep 7 years. Warranty: keep until
  expiration. Insurance: keep until renewal + N months. Surface upcoming
  expirations as insights.
- Size and quota controls, upload/download endpoints, and a Documents section
  in the UI.

**Ties into.** `crypto/crypto.go`, the household model (documents are
household-scoped, with the same ownership pattern as manual assets), Insights
(for expiration nudges), Transactions (receipts link to transactions), #8
(the paystub importer stores and OCRs paystubs through this vault).

### Income & payroll

#### 8. Pre-tax income & deduction tracking (paystub importer) — **shipped**

**Problem.** Today the app only sees money that has already survived the
gross-to-net transformation — every transaction synced via Plaid is post-tax,
post-deduction. The app is blind to the largest single claim on most users'
income: taxes withheld, retirement contributions, health insurance premiums,
HSA/FSA contributions, garnishments, and every other deduction that disappears
before the paycheck deposits. For a typical W-2 earner **30–45% of gross
income is invisible to the app.** That directly undercuts the core promise:
this is supposed to be the app that tracks *every dollar* the user earns and
where it went, pre-tax and post-tax. The spending side tracks every post-tax
dollar competently; the income side currently tracks only the residual that
hits the checking account.

Closing this gap does two things at once. First, it makes the income side of
the app as honest as the spending side: effective tax rate, total
compensation, true savings rate against *gross* rather than the artificial
*net*. Second, it makes the app a genuine tax-filing **companion** — not a
tax-prep product, never a filer, but with paystub data plus investment lots
plus the document vault the app holds nearly everything that goes on a 1040.
The goal is "make the burden of filing easier by already having tracked
everything," not "file for you."

**Scope.**

- **Paystub data model.** A `paystubs` table keyed to user and employer with
  gross, net, pay period start/end, pay date, and YTD gross/net. A
  `paystub_lines` table itemizes each line by category:
  `federal_income_tax`, `state_income_tax`, `fica_social_security`,
  `fica_medicare`, `medicare_surcharge`, `401k_pre_tax`, `401k_roth`,
  `401k_employer_match`, `ira_pre_tax`, `ira_roth`, `hsa`, `fsa`,
  `health_premium`, `dental`, `vision`, `life_insurance`, `garnishment`,
  `commuter`, `dependent_care`, `tuition_assistance`, `other`. Each line
  carries amount, YTD amount, and a `pre_tax` flag (drives both the
  gross-to-net visualization and the W-2 box mapping). An `employers` table
  holds employer name, address, and EIN — the data needed to render a clean
  employer record and to reconcile against the W-2 stored in the vault (#7).
- **Ingestion, three paths in order of preference:**
  1. **Plaid Payroll Income.** Plaid's Income product connects to the major
     payroll providers (ADP, Gusto, Paychex, UKG, etc.) and returns structured
     paystub data. This is the cleanest path for supported employers and fits
     the existing Plaid-first architecture in `backend/internal/plaid/client.go`.
  2. **PDF paystub OCR** via the optional AI provider, reusing the same
     encrypted-vault + OCR-then-confirm pattern as receipts in #7. The parsed
     paystub is *always* queued for user review before being written — never
     auto-applied — mirroring the categorisation-review pattern in
     `backend/internal/categorize/llm.go` where model output is suggested, not
     silent.
  3. **Manual entry** as the universal fallback. The line taxonomy above is
     the schema for the form, so even a paper paystub or an unsupported
     employer can be captured exactly.
- **Visualizations.**
  - **"Where your paycheck went."** For any single paystub or any YTD window,
    a Sankey-style or stacked-bar breakdown showing gross → federal tax →
    state tax → FICA → retirement → insurance → HSA → other → net. This is
    the single most clarifying personal-finance chart most people have never
    seen, and it is impossible to render without the data above.
  - **Effective and marginal tax rate** tracked over time, per employer and
    household-wide.
  - **Contribution-limit tracking** against the current year's IRS limits
    (401k employee deferral, IRA, HSA, FSA, dependent care), with a
    "you are $X from maxing your 401k with N pay periods left" progress view.
    Limits update yearly — this is itself a useful nudge, since most people
    leave contribution limits on the table without realizing.
  - **Total compensation** view: gross + employer match + value of benefits,
    distinct from net pay. The number that actually matters when comparing
    jobs or negotiating.
  - **Net-pay reconciliation.** Each paystub's net pay should be matchable to
    the bank-deposit transaction Plaid syncs ("we see a $X deposit on the 15th
    that matches this paystub"). Confirming the link ties the pre-tax record
    to the post-tax transaction so the two views never drift apart.
- **Tax-filing-prep output.** On demand, an annual summary that pre-populates
  what a 1040 filer needs, drawn from the paystub lines plus investment lots
  (#4) plus categorized transactions:
  - Wages (W-2 box 1), federal tax withheld (box 2), Social Security wages
    (box 3) and tax (box 4), Medicare wages (box 5) and tax (box 6).
  - 401k contributions (box 12 codes D/E/F/G/H/H-dash), IRA / SEP / SIMPLE
    (box 13), Roth contributions (box 12 code BB).
  - HSA contributions, IRA contributions, charitable giving (from transaction
    categorization), mortgage interest and property tax (from manual entries
    or mortgage account data), capital gains / dividends (from #4's lot
    tracking and income classification).
  - Combined with the actual W-2 / 1099 / 1098 documents stored in the vault
    (#7), this is the packet a user hands to their accountant or pastes into
    filing software. The app does not file; it removes the data-gathering
    burden that is most of the work for a simple return.
- **This is deliberately not a tax-prep product.** No e-file, no form
  generation, no CPA advice. The brand stays "track every dollar honestly";
  the tax-filing output is a *report* over already-tracked data, not a
  feature that competes with FreeTaxUSA / TurboTax. The honesty here matters:
  users should trust the numbers because they came from their own paystubs
  and transactions, not from an LLM guessing at tax law.

**Ties into.** Document vault (#7 — **shipped**, so `documents.Vault` is
available for paystub PDF storage and `ai.ExtractReceipt`'s image support for
OCR),
Investments (#4, for 401k / IRA contribution cash flow into retirement
accounts and for capital-gains reporting that feeds the tax summary),
Retirement projections (#5, which become genuinely useful once savings rate
is measured against gross and contributions are tracked against IRS limits),
the Advisor (#3, which can now answer "am I on track to max my 401k this
year?" and "what's my effective tax rate YTD?"), `plaid/client.go` (for the
Payroll Income integration), `categorize/llm.go` (for the OCR-confirm
pattern), the existing transaction category taxonomy (charitable, medical,
mortgage categories already exist and feed the tax summary directly).

**Ties into.** Document vault (#7), investment lots (#4), the existing
transaction categorization.

### Household multi-user

This is the app's structural advantage over single-user competitors like
Rocket Money, and it is under-used today. The household model and per-institution
sharing already exist; the features below extend them into the parts of
household money management that currently have no in-app answer.

#### 9. Shared goals, bill split, kid sub-accounts — **shipped**

**Scope.**

- **Shared household goals.** The `goals` table already supports
  `scope = 'household'`; extend it so multiple household members can contribute
  to the same goal, with a per-member contribution view and a "who funded
  what" history. A vacation fund or a joint emergency fund is the canonical
  use case.
- **Bill split.** Mark any transaction as splittable across household members
  (50/50, custom percentages, or exact amounts). Tracks each member's share
  and resulting balances. Surfaces "Member B owes Member A $X" as a running
  household-balance ledger without requiring actual money movement.
- **Reimbursement tracking.** A simple household ledger: Member A paid a
  shared expense, Member B owes $Y; mark as settled when settled. Reduces the
  "who paid for what this month" conversation to a glance.
- **Kid sub-accounts.** Limited-permission household members (or a new member
  type) for children: a parent-managed allowance schedule, a spending view the
  parent can see, optional spending limits, and a savings goal the child can
  watch grow. Uses the existing household + goals machinery with a new
  permission role.

**Ties into.** The existing household model, per-institution sharing
(`plaid_items.is_shared`), goals (`backend/internal/goals`), the household
ownership pattern enforced across all path-parameter handlers.

### Platform & delivery

#### 10. Weekly digest, in-app first — **shipped**

**Problem.** The app already generates a periodic digest
(`backend/internal/jobs/digest.go`) and can push highlights via ntfy, but the
primary surface for "what happened with my money this week" should be in the
app itself, not dependent on having notifications configured. ntfy is
real-time push but requires the user to be looking; an in-app digest is what
makes the app feel alive on a Sunday-morning check-in.

**Scope.**

- A new in-app **Weekly Digest** view: spending vs. budget for the week, the
  week's largest transactions, net-worth change, upcoming bills (per #1), the
  week's insights and any advisor suggestions (per #3). Generated by the
  existing digest job, stored as rendered entries, presented in a paginated
  history ("this week, last week, the week before…").
- **Optional SMTP send** of the same digest, off by default, configured via
  the existing pattern in `backend/internal/config/config.go`. The README's
  "sends no email" line becomes "sends no email unless you opt in."
- Reuse the existing digest job infrastructure rather than building a parallel
  path; the work is the in-app surface and the optional transport, not the
  generation.

**Ties into.** `jobs/digest.go`, `notify/`, the Dashboard (digest entries can
double as dashboard widgets), #1 (upcoming bills in the digest), #3 (advisor
suggestions in the digest).

#### 11. PWA install + offline read — **shipped**

Delivered as the read-only MVP the plan called for. Queue-and-replay for writes
stayed out on purpose and is not merely deferred: writes are refused outright
with an explanation, because a queue that silently replays a stale edit against
a ledger that moved is worse than no queue. Background sync is out for the same
reason. Browser push remains out of scope — ntfy is the notification path.

**Problem.** Self-hosters check their finances on their phones constantly, and
the current SPA works in a mobile browser but is not installable and does not
degrade gracefully offline. A native-quality installable experience closes the
gap to mobile-app competitors without writing a native app.

**Scope.**

- A `manifest.webmanifest` with app name, icons, theme color (existing brand
  palette in `BRAND.md`), display: standalone.
- A service worker that caches the app shell and the most recent API responses
  for **read-only offline access** (Dashboard, Net Worth, Transactions list
  render from cache when offline). Writes queue and replay when connectivity
  returns.
- App icons in the required sizes.
- Install-promote UX (a subtle "Install app" affordance, the browser's native
  install prompt).
- Stretch: background sync for queued writes.

**Ties into.** Frontend build (`frontend/vite.config.ts`,
`frontend/index.html`), the existing read-only API endpoints. Mostly a
frontend initiative with no backend changes required for the offline-read MVP.

### Numerical honesty

Two cross-cutting correctness issues that affect every aggregate the app
produces. Both are lower priority than the core feature work above but
genuinely matter to the "honest money" brand promise.

#### 12. Inflation-adjusted views — **shipped**

**Problem.** Any long-term trend currently compares nominal dollars across
years, which is exactly the kind of arithmetic dishonesty the app rejects
elsewhere (transfers counted twice, monthly averages dividing wrong).
"Net worth up 8% this year" in a 6% inflation year is 2% real growth, and the
app currently has no way to say that. Most users have no idea what inflation
actually was in a given year or how it compounds against their numbers — this
is a small amount of work for a large honesty payoff and an educational win.

**Scope.**

- A CPI series snapshot table populated by a monthly job pulling CPI-U from
  the BLS public API (free, no key required for the basic series). Same shape
  as the `asset_prices` (#4) and FX-rate (#13) snapshot tables.
- A **"real" toggle** on Net Worth history, spending trends, investment
  performance, and the FIRE projections: switch between nominal and
  inflation-adjusted with the CPI series as the deflator.
- An inflation strip on the Dashboard: "inflation YTD is X%", contextualized
  against the user's own income / net-worth growth so the comparison is
  concrete rather than abstract.
- Honest projections: the FIRE projection (#5) should let the user choose
  between nominal and real returns explicitly rather than presenting one as
  the truth.

**Ties into.** Net Worth, Investments (#4), FIRE projections (#5), the
Advisor (#3). Small, self-contained, and arguably should land alongside any
of those rather than as a standalone project.

#### 13. Multi-currency

**Problem.** The app hardcodes US dollars — the assistant system prompt says
so explicitly (`backend/internal/api/chat_handlers.go:43`), and there is no
currency column on accounts or transactions. Every aggregate silently assumes
one currency. For non-US users this means their numbers are tracked wrong,
not tracked at all. Marked as far-future because this is primarily a US app
today, but the structural correctness issue is real and the plumbing is
already currency-agnostic (decimal strings, `NUMERIC(20,4)`).

**Scope.**

- A `currency` column on accounts and manual transactions; existing data
  migrated assuming USD.
- A household base currency for rollups.
- A daily FX-rate snapshot table populated from a free FX API
  (`exchangerate.host` / `frankfurter.app`).
- Conversion at aggregation time, with both raw and converted amounts
  available in responses.
- Assistant prompt updated to reason about currency rather than assert USD.
- UI affordances for mixed-currency households (per-account currency badge,
  converted totals clearly labelled).

**Ties into.** Every aggregation query, the assistant prompt, the report
exports. Land after the higher-priority initiatives above; revisit when
non-US adoption becomes a real concern.

### Real assets

#### 14. Real-asset revaluation and depreciation — **shipped**

**Problem.** Manual assets today are a static number typed in once. For most
households the home is the largest line on the net-worth sheet and its value
is wrong within months of entry; vehicles depreciate on a predictable curve
but the app holds the original number forever. A paid-off car sitting in the
garage worth $20k is currently invisible to the app unless the user manually
re-enters its value periodically — which nobody does. Without addressing this
the net-worth trend, the FIRE projections, and the "honest money" promise all
drift further from reality every month.

**Scope.**

- **Asset classes** within manual assets: real estate, vehicle, and other,
  with class-specific metadata.
  - **Real estate:** address, bed/bath/sqft, lot size, condition. Used to
    drive valuation estimates and to attribute value changes over time.
  - **Vehicle:** year, make, model, trim, mileage (with periodic re-entry or
    estimated annual mileage), condition. Drives a depreciation curve.
- **Optional auto-valuation.** For real estate, pull estimates from public
  valuation sources where their ToS permits (Redfin Estimate, Realtor.com
  estimates; the Zillow Zestimate API has been progressively restricted and
  may not be viable). For vehicles, pull from Kelley Blue Book / Edmunds
  fair-value ranges given year/make/model/mileage. Always presented as an
  *estimate* the user confirms, never silently applied.
- **Scheduled revaluation nudges.** "You set your home value 18 months ago
  at $X. Recent estimates for comparable properties are around $Y — want to
  update?" Surfaced as an insight. For vehicles, a yearly depreciation
  refresh on a standard curve even when no API is available.
- **Value history per asset.** Track each revaluation as a dated entry so the
  asset has its own trend line, the same way net-worth snapshots have a trend
  line. A home that appreciated $80k over five years should be visible as
  that trend, not as a single current number.
- **Loan / payoff modeling for assets.** A vehicle that is fully paid off is
  currently just a manual asset with no associated liability; a vehicle with
  an active loan is a manual asset plus a separate Plaid-synced liability
  with no link between them. Tie the two: an asset can have an associated
  loan account, equity = value − balance, payoff progress visible per asset.
  This makes "I own both my cars outright" and "I have $30k equity in my car"
  both first-class states.

**Ties into.** Net Worth (manual assets already live in
`backend/internal/db/queries/networth.sql` and the handlers in
`networth_handlers.go`), FIRE projections (#5, which are only as good as the
asset values feeding them), Insights (for revaluation nudges), Liabilities
(for tying loans to assets), the existing manual-asset endpoints.

### Continuity & disaster recovery

#### 15. Self-hosted continuity, key management, and legacy access

**Problem.** This is the gap unique to self-hosted that no cloud competitor
has, and it is genuinely dangerous. `ENCRYPTION_KEY` is a single point of
catastrophic failure: lose it and every Plaid token, every future paystub
(#8), every future document (#7) is unrecoverable, and there is no company to
call. If the user dies or becomes incapacitated, their family is locked out
of their entire financial history — bank links, net-worth trend, documents,
everything — with no recovery path. Cloud apps handle key management and
inheritance invisibly; for self-hosted this is load-bearing and currently
undocumented. The launchworthy audit flagged "no automated database backup"
as a HIGH finding; this entry formalizes that gap and the broader continuity
story around it.

**Scope.**

- **Operator's continuity runbook** in DEPLOYING.md: where the ENCRYPTION_KEY
  must be stored (password manager, offline backup, what happens to the app
  if it is lost), what needs to be backed up (database, document volume, key,
  env), how often, and how to verify a backup by restoring it.
- **Automated, verified database backups.** A backup sidecar in
  `docker-compose.prod.yml` running the documented `pg_dump` on a schedule,
  with an automated restore-test job that restores the latest backup into a
  throwaway database and confirms row counts match. A backup that has never
  been restore-tested is a guess — the restore-test is what makes it real.
- **Backup-completeness dashboard.** A continuity panel in Settings showing:
  last successful DB backup, last successful restore-test, last document
  backup, whether the key backup is confirmed, with red/yellow/green status
  for each. Operators should be able to see at a glance whether their setup
  is recoverable.
- **Portable full-data export.** A scheduled export to a versioned JSON
  format (schema versioned, documented, plain-text-decodable) so the data can
  outlive the app itself. Includes transactions, accounts, net-worth history,
  manual assets, goals, budgets, categories, paystubs, document metadata
  (documents themselves encrypted separately). Self-hosters picked
  self-hosted because they want to own their data; this guarantees they
  actually can.
- **Optional encrypted offhost backup.** Push the backup bundle to an
  S3-compatible bucket (user-configured) so a host failure does not take the
  data with it. Encrypted client-side with a key the S3 provider cannot see.
- **Legacy access feature.** A trusted-household-member recovery mechanism
  for death or incapacitation: a configurable inactivity period (e.g. 60 days
  with no login) after which a designated household member can request
  access; or a split-key arrangement where recovery requires both the
  operator's key material and a second factor held by the legacy contact.
  This is hard to get right and the design needs care, but the absence of any
  such mechanism today means the worst-case scenario is total data loss at
  the moment it matters most.
- **Restore procedure documentation.** A tested, written-down procedure for
  rebuilding from scratch: which four things you need (code in git, env in a
  password manager, database restorable from backup, key accessible), how to
  restore each, and how to confirm the restored instance is healthy.

**Ties into.** `backend/internal/crypto/crypto.go` (the cipher whose key
must be protected), the household model (legacy contact is a household
member with elevated recovery permissions), document vault (#7, documents
must be in the backup), paystub data (#8, same), `docker-compose.prod.yml`,
`DEPLOYING.md`. Resolves the highest-severity open items from the launchworthy
production-readiness audit.

### Scenario planning

#### 16. Decision modeling — interactive what-if engine

**Problem.** Every adult faces a handful of financial decisions that
genuinely change the trajectory of their life: which house to buy, whether to
pay off debt or invest, which job offer to take, when they can afford to
retire, whether they can afford a kid or a sabbatical or a move. These are
high-stakes — a wrong answer on a house purchase or a retirement timing
decision is a six-figure mistake — and they are badly served by every tool
that exists. Spreadsheets are too tedious, online calculators use rules of
thumb and ignore your actual numbers, and the math is too interleaved
(opportunity cost, tax implications, sequence-of-returns risk, cash flow) to
do by hand. By the time this app has shipped #1, #4, #5, #8, #12, and #14 it
will hold all of the inputs needed to compute these answers *for the actual
user* rather than for a generic persona. That is the payoff this feature
exists to capture.

**Shape — an engine, not a calculator pile.** A *scenario* is a clone of the
user's current state (income from #8, recurring obligations from #1, debts
from liabilities, assets from #14, investments from #4, goals, contributions)
with any input overridden. The projection machinery from #5 runs against the
modified state over a chosen horizon. Output is side-by-side: baseline vs
scenario, across net-worth chart, FIRE age, monthly cash flow, tax picture,
and goal completion dates. Multiple scenarios can be saved, named, and
compared against each other. Each carries explicit assumptions (real return
%, inflation %, mortgage rate) so the user can interrogate the result rather
than trust a black box.

This initiative **must land after #5 ships.** The FIRE projection engine is
the foundation; this feature is the user-facing layer that makes the
projection engine actually useful for decisions. Building it before #5 means
re-implementing projection logic that will be thrown away.

**Scope — the five scenario families that matter most.**

1. **Major purchase with opportunity cost.** The universal high-stakes
   decision. Model buying a house, a car, or financing any large purchase,
   with three outputs:
   - **Direct cost:** mortgage/loan payment, total interest over the term,
     property tax + insurance + maintenance for real estate, depreciation
     schedule for vehicles (uses #14's curves).
   - **Cash-flow impact:** how the payment fits into the monthly budget
     against the bill calendar (#1), and whether it triggers predicted-low-
     balance alerts.
   - **Opportunity cost:** the invest-the-difference comparison. "If you
     rented instead and invested the down payment plus the monthly
     difference at 7% real return, you'd have $X at retirement — versus
     owning, which has you net worth $Y but no rent in retirement." Both
     paths on the same net-worth chart, with the FIRE-age delta and the
     breakeven year. This is the single most clarifying computation personal
     finance software can do, and nobody does it.

2. **Retirement stress tests.** The FIRE version of a load test, modeling the
   risks that linear projections silently ignore.
   - **Sequence-of-returns risk:** "retire next year into a 30% market drop
     in year one." Run N historical-return sequences against the withdrawal
     phase and report the survival percentage over 30 years. This is the
     most under-modeled risk in early-retirement planning and exactly where
     naive projections lie.
   - **Return-rate sensitivity:** "real returns are 4% instead of 7% for the
     next decade — does the plan survive."
   - **Social Security cuts:** "benefits are means-tested or cut 25% — does
     the plan survive."
   - **Inflation surprises:** "inflation runs 5% for five years"
     (cross-references #12).
   - Output for each: probability of plan survival, plus the savings-rate
     adjustment needed to restore the original safety margin.

3. **Life-event impact.** The scenarios people should run *before*, not after.
   - **Have a kid:** estimated childcare cost (regional averages), healthcare
     premium bump, 529 contribution impact, tax-credit modeling, FIRE-age
     shift. Use a regional-cost dataset rather than a single national
     average.
   - **Spouse stops working for N years:** savings-rate collapse, retirement-
     age shift, re-entry earning assumption.
   - **Take a sabbatical:** savings depletion rate, time to recover the gap
     after return.
   - **Support aging parent:** adds a recurring obligation tied to the bill
     calendar (#1), models the duration and the FIRE-age impact.
   - **Move to a different state / country:** tax implications at the new
     marginal rate (uses #8), cost-of-living adjustment, mortgage/rent delta.

4. **Job offer comparison.** Total-comp math that almost nobody does
   correctly, with the FIRE-age impact made explicit.
   - Two or more offers, each with: salary, 401k match (vesting schedule),
     bonus structure, RSU / option grant (vesting schedule, assumed stock
     performance), benefits dollar value, HSA contribution, retirement-age
     impact.
   - The app computes real total comp over a 4-year (or user-chosen) horizon
     using #8's tax math, retirement-account contribution headroom, and the
     projected wealth at the end of the horizon.
   - The right answer is almost never the higher-salary offer once match,
     vesting, and benefits are properly valued — the app shows why.

5. **Goal acceleration (solve-for-X).** Inverts the question: instead of
   "what happens if I change X" it answers "what X gets me to target Y."
   Targets are sticky; inputs are flexible — this matches the shape real
   decisions take.
   - "I want to retire 5 years earlier — solve for the savings rate, or the
     windfall, needed."
   - "I want to buy a house in 2028 with 20% down — solve for the monthly
     savings required."
   - "I want to fund the 529 to $200k by age 18 — solve for the contribution
     schedule."
   - "I want to be debt-free in 3 years — solve for the additional monthly
     payment."

**Additional scenarios in scope (lower priority but share the engine).**

- **Invest-vs-pay-off-debt with real numbers.** "Extra $500/mo toward my 3.5%
  mortgage or invested at 7% — run both, show breakeven and variance." Uses
  the user's actual rates and balances rather than rules of thumb. The
  variant where the app *proactively* notices a guaranteed-positive-EV move
  (e.g. $40k HYSA at 4.5% vs $40k credit-card debt at 22%) lives in the AI
  discovery layer below.
- **Windfall allocation.** "I just received $50k — given my debts, goals,
  contribution headroom, and tax situation, what's the optimal split." The
  app computes the answer rather than offering platitudes; often
  unintuitive (max neglected tax-advantaged accounts → highest-APR debt →
  taxable investing).
- **Insurance needs analysis.** "How much life insurance do I actually need"
  given dependents, income-replacement target, debt payoff, goal funding,
  education funding. Output: a concrete coverage number, not a
  multiple-of-income rule. "If I became disabled tomorrow, how long does
  the plan survive" — emergency-fund adequacy under reduced income.

**AI-assisted scenario discovery.**

A scenario engine that waits for the user to ask the right question mostly
goes unused, because most people don't know which question to ask. The
proactive advisor from #3 becomes the discovery layer — it surfaces
scenarios that matter for the user's specific situation and offers to build
them:

- "Your HYSA balance ($42k) exceeds your credit card balance ($38k) at 22%.
  Want me to model paying the cards off — it's a guaranteed 17.5% spread."
- "Your 401k contributions YTD are $8k against a $23k limit with 6 pay
  periods left. Want me to model maxing it."
- "Your mortgage rate is 3.2% and you're carrying $30k in a 4.5% HYSA. Want
  me to model the invest-versus-pay-down tradeoff over 10 years."

The pattern is the same one the chat assistant already uses
(`backend/internal/api/chat_handlers.go`): deterministic engine computes the
scenario, LLM presents it in clean prose, math is never LLM-generated. The
discovery is where the LLM earns its keep; the numbers stay auditable.

**Scope discipline / out of scope.**

- Not tax preparation. Scenarios surface tax *implications* (marginal-rate
  impact, contribution-limit headroom, Roth-vs-traditional breakevens) but
  do not file, do not generate forms, do not give CPA advice. Same line #8
  holds.
- Not financial advice. The engine presents computed outcomes and their
  tradeoffs; it does not recommend. The user decides.
- Not real-time market data. Projections use assumed real returns the user
  sets; live ticker data is out of scope (the `asset_prices` table from #4
  is for historical performance reporting, not forward modeling).

**Ties into.** Retirement projections (#5, the foundation — without these
primitives this feature is impossible), bill calendar (#1, for cash-flow
impact of purchases and life events), income & payroll (#8, for tax math
and contribution-limit headroom), real-asset values (#14, for accurate
net-worth inputs), inflation adjustment (#12, for honest long-horizon
projections), the proactive advisor (#3, for scenario discovery and
presentation), the chat assistant's tool-call pattern (`chat_handlers.go`),
goals (the solve-for-X scenarios are goal-driven).

---

## Build history

The original seven-phase build plan, for reference. Each phase shipped and was
verified against real data.

1. **Foundation** — scaffold, compose, config, schema, auth, health endpoint.
2. **Transactions ingest** — Plaid Link, `/transactions/sync` with cursor, full
   historical backfill, webhooks, CSV import.
3. **Reporting core** — categorization, monthly rollups, spending dashboard,
   per-category averages, annual totals, savings rate.
4. **Net worth + modules** — Investments and Liabilities modules, manual assets,
   monthly net-worth snapshots, projections.
5. **Financial summary** — exportable PDF + CSV report.
6. **AI enrichment** — LLM categorization fallback, recurring detection, alerts.
7. **Chatbot** — tool-calling agent over your own financial data.
