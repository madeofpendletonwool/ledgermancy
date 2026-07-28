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

- **Insights don't push in real time.** The proactive insight feed (spending
  spikes, new recurring charges, subscriptions, forecasts) surfaces in-app and
  rides along in the scheduled digest, but the high-priority push seam in
  `backend/internal/insights/engine.go` is not wired — an insight never pings
  your notification channel the moment it's detected the way an **alert** does.
  Alerts are the real-time push path (opt in per rule on the Alerts page);
  insights are pull + digest only. Wiring it would mirror the alert dispatch:
  enqueue a notify job for newly-created insights above a priority threshold.
- **Debt-payoff goals are schema-only.** The `goals.kind` column allows
  `debt_payoff`, but the feasibility maths (`backend/internal/goals`), the
  natural-language parser (`backend/internal/ai/parse.go`), and the Goals UI all
  handle `savings` only. Creating or tracking a payoff goal isn't possible yet.
- **Disconnected Plaid items cannot be reconnected from the UI.** When Plaid
  fires `ITEM_LOGIN_REQUIRED` (credentials changed, MFA re-challenge, session
  expired) the webhook handler and the syncer both correctly mark the item
  `login_required` in `plaid_items.status`
  (`backend/internal/api/webhook_handlers.go:64-75`,
  `backend/internal/plaid/sync.go:351`), and the Accounts page surfaces that
  state — but there is no button to put the item back through Plaid Link in
  update mode. Today the only recovery paths are (a) deleting the item and
  re-linking from scratch, which **destroys the transaction history tied to
  that item** (per the README's one-way-door note: a fresh link can request
  the 730-day window going forward, but everything already tied to the old
  `plaid_item_id` is orphaned), or (b) shelling out to the Plaid dashboard.
  Fix: extend `handleCreateLinkToken` (`backend/internal/api/plaid_handlers.go`)
  to accept an optional item ID; when supplied, decrypt that item's access
  token via the existing Cipher (`backend/internal/crypto/crypto.go`) and pass
  it to Plaid's `/link/token/create` in **update mode**. Add a "Reconnect"
  button on the Accounts page for any item in `login_required` (or `revoked`)
  state that opens Plaid Link with the resulting token. On success, clear the
  item's status back to `active` and enqueue a sync. The data model already
  supports this — `plaid_items.access_token_encrypted` is the encrypted token
  and the status CHECK constraint already allows the recoverable states
  (`backend/internal/db/migrations/00001_core_schema.sql:92`) — it is purely
  the missing update-mode link path and the UI affordance.

---

## Roadmap

### Recently shipped

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

### Still planned

- **Monthly recap overhaul** — format money as `$1,234.56`; feed the model a real
  breakdown (per-category vs. typical, biggest transactions, savings rate)
  instead of raw category totals; present tense for the in-progress month;
  auto-generate weekly, with a final past-tense recap when the month closes.
- **Smarter recurring detection** — a recency gate so paid-off items drop off the
  Spending "Recurring" table promptly; a per-merchant **"not recurring"**
  override; and better cadence detection so coincidental clustering isn't
  flagged.
- **Insight expansion** — projected month-end cash flow, unusually-large single
  transaction, income-change detection, savings-rate milestones, goal-progress
  nudges; plus real-time insight push (see Known gaps above).
- **Budget expansion** — a **"safe to spend"** figure (income − fixed − budgeted
  − goal contributions); **rollover / envelope** budgets; non-monthly periods
  (weekly, annual); percentage / zero-based allocation; budget-vs-actual trend.

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

### Forward-looking money intelligence

#### 1. Bill calendar + cash-flow forecast

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

#### 2. Predictive anomaly detection

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

#### 3. Proactive cash-flow advisor

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

#### 4. Dedicated Investments page + performance analysis

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

#### 5. Retirement & FIRE projections, including 529s

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

#### 6. Smart merchant canonicalization

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

#### 7. Encrypted document vault

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

#### 8. Pre-tax income & deduction tracking (paystub importer)

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

**Ties into.** Document vault (#7, for paystub PDF storage and OCR),
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

#### 9. Shared goals, bill split, kid sub-accounts

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

#### 10. Weekly digest, in-app first

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

#### 11. PWA install + offline read

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

#### 12. Inflation-adjusted views

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

#### 14. Real-asset revaluation and depreciation

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
