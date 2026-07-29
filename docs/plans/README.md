# Feature plan docs

Individual, execution-ready plans. Each doc is scoped so a single agent (or
person) can pick it up cold and implement it.

Waves 0–2 (docs 00–12) expanded Ledgermancy's AI beyond chat and are **shipped**
— they remain here as the reference for how the insight engine, preferences, and
notification contracts work.

Waves 3–6 (docs 13–29) cover **everything remaining in
[TODO.md](https://github.com/madeofpendletonwool/ledgermancy/blob/main/TODO.md)**: all sixteen "Next major initiatives" plus the two
small known gaps. Wave 3 is the current cycle.

Within waves 0–2, implement in order; later docs depend on earlier ones. Waves
3+ are mostly parallel — most docs have no prerequisites at all. Read the
**Prerequisites** section of any doc before starting it, and check the migration
reservation table below before writing a migration.

### Outdated claims in TODO.md, corrected here

TODO.md's initiative descriptions were written before some of the work landed.
Where a doc's Context section contradicts TODO.md, the doc is right — it was
checked against the code:

- **Real-time insight push** is wired (`dispatchInsightPushes`, `jobs/jobs.go`),
  not the open seam TODO #1's neighbours imply.
- **Price creep** is shipped (`insights/subscription.go`), despite TODO #2
  listing it as new work. Doc 22 scopes around it.
- **Plaid reconnect / Link update mode** shipped mid-planning
  (`plaid.CreateUpdateLinkToken`); it never needed a doc.
- **There is no `dividends` category**, despite doc 14 and TODO #4 both saying
  one exists. Dividends are credited inside the brokerage account and never
  reach the bank feed, so 14 sources them from `investment_transactions` by
  subtype instead. Anything else planning to read a dividend category should do
  the same.

## The one rule every doc honors

**AI never computes; it interprets.** Detection, aggregation, and arithmetic
stay in SQL / `shopspring/decimal`. AI is used only for language, messy
classification, explaining *why* a number matters, and turning natural language
into structured intent. The model is always handed *finished* figures
(`StringFixed(2)` strings) — never asked to add, average, or divide. This mirrors
the existing `buildSummaryInput` pattern (`backend/internal/api/summary_handlers.go`)
and the chat tool layer (`backend/internal/api/chat_handlers.go`).

## Read first

- **[00-shared-contracts.md](00-shared-contracts.md)** — the three new contracts
  every feature builds on: the insight engine (`insights` table + producer
  interface), the preferences store, and the `Notifier` (ntfy) abstraction. Read
  this before any feature doc.

## Waves

### Wave 0 — Foundations (mostly non-AI; unblock everything)
- **[01-budgets-ui.md](01-budgets-ui.md)** — frontend budget management over the
  existing budget API. *No AI.*
- **[12-manual-transactions.md](12-manual-transactions.md)** — enter/edit/delete
  transactions by hand to reconcile aggregator gaps, with a duplicate guard.
  *No AI.* (Wave 0 ledger-management work; pairs with 01.)
- **[02-settings-and-preferences.md](02-settings-and-preferences.md)** — rename
  Security→Settings, add the preferences store. *No AI.*
- **[03-notifications-ntfy-delivery.md](03-notifications-ntfy-delivery.md)** —
  ntfy delivery client + wiring. *No AI.*
- **[04-insight-engine-and-feed.md](04-insight-engine-and-feed.md)** — THE SPINE:
  insights table, producer interface, generation job, feed UI. The "proactive
  insight feed."

### Wave 1 — AI producers on the spine
- **[05-subscription-intelligence.md](05-subscription-intelligence.md)**
- **[06-forecast-narration.md](06-forecast-narration.md)**
- **[07-alert-explanation.md](07-alert-explanation.md)**

### Wave 2 — depend on budgets / delivery / greenfield schema
- **[08-budget-suggestions.md](08-budget-suggestions.md)** — needs 01
- **[09-nl-alerts-and-budgets.md](09-nl-alerts-and-budgets.md)** — needs 01
- **[10-scheduled-digest.md](10-scheduled-digest.md)** — needs 02, 03, 04
- **[11-goal-coaching.md](11-goal-coaching.md)** — needs 04, 06

### Wave 3 — next major initiatives

Docs 00–12 are shipped, and so are **13, 14 and 15**. Drawn from the "Next major
initiatives" section of [TODO.md](https://github.com/madeofpendletonwool/ledgermancy/blob/main/TODO.md). Unlike waves 0–2 these are
large and mostly independent. **Only 16 remains in this wave**; it has no
prerequisites and was consciously skipped to start wave 4, so pick it up next.

- **[13-bill-calendar.md](13-bill-calendar.md)** — recurring obligations
  (detected *and* manually entered), a Schedule page, day-by-day projected
  balances, safe-to-spend integration, a predicted-low-balance alert.
  *TODO #1.* No prerequisites.
- **[14-investments-page.md](14-investments-page.md)** — **shipped.** A
  dedicated Investments surface: TWR/IRR, benchmarks, allocation, fee drag, and
  the **account tax-treatment tagging 15 depends on** — `accounts.tax_treatment`
  is live (migration `00020`) and `reporting.SuggestTaxTreatment` returns a
  suggestion for confirmation, never a stored value. *TODO #4.*

  Two things 15's implementer should know. First, the doc assumed
  `investment_transactions` was populated; it was not — the table existed but
  nothing wrote to it, so `plaid.GetInvestmentTransactions` was added and
  `SyncInvestments` now stores them. Second, target allocation + drift and the
  rebalance helper were **deferred**: both need a stored per-household target
  that nothing else in the backlog wants yet, and the allocation view is honest
  and useful without them. Fee drag ships as structure plus disclosure only —
  Plaid supplies no expense ratio, so the endpoint reports full exclusion rather
  than a number computed over part of a portfolio.
- **[15-fire-projections.md](15-fire-projections.md)** — **shipped.**
  Account-aware retirement projection and a withdrawal-rate lens at
  `/retirement`, beside (not replacing) the linear model in
  `networth/project.go`. *TODO #5.* Migration `00021` is taken.

  Four things 24's and 28's implementers should know, since both build on this.
  First, the engine is `networth.ProjectRetirement` and takes `now` as a
  parameter — call it that way rather than reaching for the clock, or the tests
  become calendar-dependent. Second, IRS limits live in a versioned Go map
  (`networth/limits.go`) keyed by tax year, and an unconfigured year returns
  `ok=false` on purpose: surface it, never substitute an adjacent year. Third,
  the migration gained three columns doc 15's sketch did not have —
  `account_contributions.annual_salary` (an employer match is a percentage *of
  salary*, so a match with no salary behind it is not an amount and is refused
  at the API), and `beneficiary_current_age` / `beneficiary_target_age` for the
  529 horizon.

  Fourth and most load-bearing: **Monte Carlo did not ship as a historical
  backtest**, and nothing downstream should assume one exists. No return series
  is bundled — shipping one would mean either an outbound fetch the README
  promises against or a transcribed table of numbers nobody can verify. What
  ships instead draws sequences around the user's *own* stated real return and a
  volatility they set, seeds deterministically from the inputs, names that basis
  in every response, and is gated behind `RETIREMENT_MONTE_CARLO_ENABLED`
  (default off). Tax drag on withdrawals and RMDs are likewise **not modelled**;
  they are listed as omissions in the UI and want doc 23's income data.
- **[16-continuity-and-backups.md](16-continuity-and-backups.md)** — automated
  `pg_dump` + a **verified restore test**, portable export, optional encrypted
  off-host push, a continuity panel, and a tested restore runbook. *TODO #15;
  resolves the open HIGH audit finding.* No prerequisites.

### Wave 4 — foundations (no prerequisites, all parallel-safe)

**17 and 18 have shipped.** 16 is the only thing left behind them, deliberately
deferred rather than dropped — it keeps its `00022` reservation, and now has a
second thing to back up: see 18's note about the document volume not being in
`pg_dump`.

- **[17-merchant-canonicalization.md](17-merchant-canonicalization.md)** —
  **shipped.** Fragmented merchant strings map to canonical entities at
  `/merchants`, suggestion-then-confirm. *TODO #6.* Migration `00023` is taken.

  Four things doc 22's implementer should know, since it builds on this. First,
  **the resolution rule lives in SQL and only in SQL** — it is documented in full
  at the top of `db/queries/merchants.sql` and every consumer inlines the same
  two LEFT JOINs. A Go-side `Resolver` was written and then deleted: nothing
  needed it, and a second implementation of the rule is a thing to keep in step
  for no benefit. Copy the SQL block; do not reintroduce a Go resolver unless a
  caller genuinely cannot do the join.

  Second, **the resolved key is an entity UUID rendered as text**, and raw keys
  pass through unchanged. Resolution is idempotent (no alias is ever keyed by a
  UUID), which is what lets `recurring_overrides` and
  `recurring_obligations.merchant_key` hold either form and still match —
  including rows written before a merge. `GetRecurringMerchants` returns the
  resolved key, so anything storing that key gets an identifier that survives
  further descriptors joining the merchant. Baselines built per entity should key
  the same way.

  Third, the doc's schema shipped as written, plus a `key_a < key_b` check
  constraint on `merchant_merge_rejections` so a pair has one representation, and
  a partial index on active aliases. The one behaviour the doc did not specify:
  **a component containing any rejected pair is dropped whole**, not split around
  the refusal. Transitivity would otherwise re-form a rejected merge through a
  third descriptor.

  Fourth, one consumer outside the doc's list needed fixing and got it:
  `UnusualMerchantCandidates` (`alerts.sql`) judged "first ever seen" per raw
  descriptor, so a merged merchant's second descriptor fired a false new-merchant
  alert. It now resolves. Anything else asking "have we seen this merchant
  before?" must do the same.

  Entity logos are out of scope and stayed out — an outbound dependency the app
  otherwise does not have.
- **[18-document-vault.md](18-document-vault.md)** — **shipped.** Encrypted
  document storage over the existing AES-GCM cipher, linked to
  transactions/assets/goals, with expiry nudges. *TODO #7.* Migration `00024` is
  taken. **Doc 23's storage dependency is satisfied** — the storage layer,
  encryption and upload/download endpoints are live, so 23 can build its
  paystub-specific schema on top of `documents` today.

  Five things doc 23's implementer (and anyone touching the vault) should know.

  First, **the storage layer is a `Storage` interface with two implementations**
  (`documents/storage.go`, `documents/s3.go`) and a `Vault` on top that owns the
  cipher. Nothing outside the package knows documents are encrypted: callers
  hand `Vault.Store` plaintext and get a storage key back. A paystub importer
  should reuse `Vault` rather than sealing anything itself.

  Second, **S3 shipped without an SDK.** SigV4 is signed by hand in `s3.go`,
  because the vault needs three verbs against one bucket and aws-sdk-go-v2 is a
  large tree for that. It is tested for encoding rules, header set, determinism
  and coverage, not against a live endpoint — if you extend it beyond
  PUT/GET/DELETE, extend those tests first.

  Third, **the `mime_type` column is display metadata and nothing else.** The
  download path sniffs the decrypted bytes against a five-entry allowlist and
  falls back to `application/octet-stream`; the stored claim never reaches a
  response header. Anything that adds a serving path must do the same, or the
  HTML-as-receipt hole reopens.

  Fourth, **`document_links` has four typed nullable target columns with a
  one-of CHECK**, not a `(kind, id)` pair, so the foreign keys are real. Adding
  a fifth target kind means a migration *and* a `Target*InHousehold` ownership
  query — the insert cannot verify a target and deliberately does not try.

  Fifth, **OCR shipped as suggestion-only and gated on `doc_type`, and doc 23
  must not casually widen either.** There is no code path from `ExtractReceipt`
  to a written row; the endpoint returns fields plus candidate transactions for
  a person to act on. Eligibility is `documents.OCREligible`, an allowlist
  containing `receipt` and nothing else, checked *before* the bytes are
  decrypted — so a tax document cannot be sent whatever file format it is in.

  This is the one thing doc 23 has to think hardest about, because a paystub is
  more sensitive than a receipt and more sensitive than most tax documents: it
  carries an employer, a full name, frequently a partial SSN, and exact gross
  and net pay. Adding `paystub` to `ocrEligibleTypes` and moving on would be the
  wrong instinct. The options worth weighing are a per-document opt-in rather
  than a type-wide one, redaction before upload, or a local OCR path with no
  outbound call at all. Whichever it is, it deserves its own switch rather than
  inheriting `DOCUMENTS_OCR_ENABLED`, which a user turned on to read grocery
  receipts. `ai.Block` gained image support (`ImageBlock`) and is available —
  under that rule.
- **[19-debt-payoff-goals.md](19-debt-payoff-goals.md)** — the `debt_payoff`
  goal kind the schema has always allowed, plus thousands separators in
  `money()`. *TODO known gaps.* Small; good first task.
- **[20-pwa-offline.md](20-pwa-offline.md)** — installable PWA with read-only
  offline. *TODO #11.* **Shipped.** Frontend only, as advertised. The MVP
  landed; write queueing did not, and was closed rather than deferred — writes
  are refused with an explanation instead. Two notes for anyone touching it:
  the cacheable API set in `frontend/src/sw.ts` is an allowlist, so a new
  read-only endpoint needs adding there before it works offline, and sign-out
  must keep clearing the worker's caches (`clearApiCache`) or a shared device
  leaks the previous user's figures.
- **[21-household-sharing.md](21-household-sharing.md)** — shared-goal
  contributions, bill split + reimbursement ledger, and the `users.role` column
  kid sub-accounts need. *TODO #9.* Doc 16's admin check should consume this
  role.

### Wave 5 — depend on waves 3–4

- **[22-anomaly-detection.md](22-anomaly-detection.md)** — per-merchant
  baselines, outlier charges, duplicate detection. *TODO #2 — note price creep
  is already shipped; don't rebuild it.* **17 has shipped**, so build baselines
  per resolved merchant; read 17's notes above before choosing a key.
- **[23-paystub-income.md](23-paystub-income.md)** — the biggest hole in the
  data model: 30–45% of gross income is invisible today. Paystub schema, three
  ingest paths, paycheck breakdown, contribution limits, tax-prep summary.
  *TODO #8.* **18 has shipped**, so its dependency is met — read 18's notes
  above before choosing where paystub files live.
- **[24-proactive-advisor.md](24-proactive-advisor.md)** — ranked, deterministic
  options for surplus cash; the model only narrates. *TODO #3.* **Needs 13 and
  15.**
- **[25-in-app-digest.md](25-in-app-digest.md)** — persist and surface the
  digest that already exists but only ever becomes a push. *TODO #10.* Soft deps
  on 13 and 24.
- **[26-real-asset-revaluation.md](26-real-asset-revaluation.md)** — asset
  classes, depreciation curves, value history, asset↔loan equity. *TODO #14.*
- **[27-inflation-adjusted-views.md](27-inflation-adjusted-views.md)** — a CPI
  series and a real/nominal toggle. *TODO #12.* Small and self-contained; good
  candidate to bundle with 14 or 15.

### Wave 6 — capstone / far future

- **[28-decision-modeling.md](28-decision-modeling.md)** — the what-if scenario
  engine: rent-vs-buy with opportunity cost, retirement stress tests,
  solve-for-X, job-offer comparison. *TODO #16.* **Hard dependency on 15**;
  strongly wants 13, 23, 26, 27. Ship the engine plus one or two families first.
- **[29-multi-currency.md](29-multi-currency.md)** — currency on transactions,
  FX rates, conversion at aggregation time. *TODO #13.* Highest blast radius in
  the backlog and worth nothing to a US-only user — read its "should you be
  doing this" section first.

## Standard template

Every feature doc has these sections: **Context** (problem + why) · **AI vs
deterministic split** (explicit) · **Prerequisites** (which docs) · **Data model**
(tables/columns) · **Backend** (queries, jobs, API — naming concrete reuse
paths) · **Frontend** (routes/components, capabilities gating) · **AI notes**
(prompt/tooling, where applicable) · **Verification** (seed sandbox data, drive
endpoint, cross-check psql, `go build/vet/test`, frontend `tsc/build/lint`) ·
**Out of scope**.

## Environment notes for implementers

- Local dev stack is `docker compose up -d --build`; Plaid is in **sandbox** and
  AI is enabled (GLM-4.6 via z.ai, Anthropic-compatible). The stack already has
  ~102 sandbox transactions for testing.
- Throwaway Postgres for tests:
  `docker run -d --name lmtest-pg -e POSTGRES_PASSWORD=test -e POSTGRES_DB=lmtest -p 55432:5432 postgres:17-alpine`
  then `TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' go test -p 1 ./...`
- Migrations are numbered. **`00024_documents.sql` is the latest on
  `main`**, with `00033` also taken (see the foot of the table). `00019`–`00021`,
  `00023` and `00024` belong to docs 13, 14, 15, 17 and 18, which have shipped.
  **`00022` is still free**: doc 16 was deliberately deferred, not dropped, and
  keeps its reservation. **`00025`–`00032` remain reserved** and are still the
  numbers their docs should use — 18's follow-up jumped to `00033` precisely so
  it did not consume one.
  To avoid the collision class that already bit this repo once (two
  `00007`s), each doc that needs a migration has a **reserved number**; use it,
  but re-check it's still free at implementation time and renumber (+ update
  dependents) if an out-of-order merge took it:

  | Migration | Doc | Adds |
  |---|---|---|
  | `00019_recurring_obligations.sql` | 13 | `recurring_obligations` table |
  | `00020_investment_analysis.sql` | 14 | `accounts.tax_treatment`, `investment_snapshots`, `asset_prices` |
  | `00021_projection_assumptions.sql` | 15 | `projection_assumptions`, `account_contributions` |
  | `00022_backup_status.sql` | 16 | `backup_runs` table |
  | `00023_merchant_entities.sql` | 17 | `merchant_entities`, `merchant_aliases`, `merchant_merge_rejections` |
  | ~~`00024_documents.sql`~~ | 18 | `documents`, `document_links` — **taken** |
  | `00025_household_roles_and_splits.sql` | 21 | `users.role`, `goal_contributions`, `transaction_splits`, `child_allowances` |
  | `00026_merchant_baselines.sql` | 22 | `merchant_baselines` table |
  | `00027_paystubs.sql` | 23 | `employers`, `paystubs`, `paystub_lines` |
  | `00028_digest_entries.sql` | 25 | `digest_entries` table |
  | `00029_asset_revaluation.sql` | 26 | `asset_details`, `asset_valuations`, `manual_assets.loan_account_id` |
  | `00030_cpi_series.sql` | 27 | `cpi_series` table |
  | `00031_scenarios.sql` | 28 | `scenarios` table |
  | `00032_multi_currency.sql` | 29 | `*.currency` columns, `households.base_currency`, `fx_rates` |
  | ~~`00033_document_extractions.sql`~~ | 18 | `documents.extracted_*` — **taken** (follow-up to 18; every number below 33 was already reserved) |

  Docs **19, 20, and 24 need no migration.** Wave 3+ docs run in parallel, so
  **these reservations are load-bearing** — take only your own number.

  One pair can still genuinely collide and needs coordination rather than just
  distinct numbers:

  - **21 → 16.** 16 needs an owner/admin check and observes that no role column
    exists; 21 adds `users.role`. Whichever lands second adopts the other's
    mechanism instead of inventing a parallel one.

  (The 14 → 15 pair is resolved: 15 shipped reading `accounts.tax_treatment`
  from 14's `00020` and added no copy of it.)

  Docs 01, 05–09, and 12 needed **no** migration (UI, new queries, or reuse
  only). All migrations goose-annotated (`-- +goose Up` / `-- +goose Down`).
- Regenerate DB code after query/schema changes:
  `go run github.com/sqlc-dev/sqlc/cmd/sqlc@latest generate` (from `backend/`).
