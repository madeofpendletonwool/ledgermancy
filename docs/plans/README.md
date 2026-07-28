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

Docs 00–12 are shipped. Wave 3 is the current cycle, drawn from the "Next major
initiatives" section of [TODO.md](https://github.com/madeofpendletonwool/ledgermancy/blob/main/TODO.md). Unlike waves 0–2 these are
large and mostly independent — **13, 14, and 16 can run fully in parallel**;
only 15 has a hard prerequisite.

- **[13-bill-calendar.md](13-bill-calendar.md)** — recurring obligations
  (detected *and* manually entered), a Schedule page, day-by-day projected
  balances, safe-to-spend integration, a predicted-low-balance alert.
  *TODO #1.* No prerequisites.
- **[14-investments-page.md](14-investments-page.md)** — a dedicated Investments
  surface over already-ingested Plaid holdings: TWR/IRR, benchmarks, allocation
  drift, fee drag, and the **account tax-treatment tagging 15 depends on**.
  *TODO #4.* No prerequisites.
- **[15-fire-projections.md](15-fire-projections.md)** — account-aware
  retirement projection and a withdrawal-rate lens, beside (not replacing) the
  linear model in `networth/project.go`. *TODO #5.* **Needs 14** for
  `accounts.tax_treatment`. Foundation for TODO #16's scenario engine.
- **[16-continuity-and-backups.md](16-continuity-and-backups.md)** — automated
  `pg_dump` + a **verified restore test**, portable export, optional encrypted
  off-host push, a continuity panel, and a tested restore runbook. *TODO #15;
  resolves the open HIGH audit finding.* No prerequisites.

### Wave 4 — foundations (no prerequisites, all parallel-safe)

- **[17-merchant-canonicalization.md](17-merchant-canonicalization.md)** — map
  fragmented merchant strings to canonical entities, suggestion-then-confirm.
  *TODO #6.* Improves recurring detection, categorisation, and doc 22 as a side
  effect — land it early where you can.
- **[18-document-vault.md](18-document-vault.md)** — encrypted document storage
  over the existing AES-GCM cipher, linked to transactions/assets/goals, with
  expiry nudges. *TODO #7.* **Doc 23 depends on it.**
- **[19-debt-payoff-goals.md](19-debt-payoff-goals.md)** — the `debt_payoff`
  goal kind the schema has always allowed, plus thousands separators in
  `money()`. *TODO known gaps.* Small; good first task.
- **[20-pwa-offline.md](20-pwa-offline.md)** — installable PWA with read-only
  offline. *TODO #11.* Frontend only, zero backend change — the safest doc to
  run alongside anything.
- **[21-household-sharing.md](21-household-sharing.md)** — shared-goal
  contributions, bill split + reimbursement ledger, and the `users.role` column
  kid sub-accounts need. *TODO #9.* Doc 16's admin check should consume this
  role.

### Wave 5 — depend on waves 3–4

- **[22-anomaly-detection.md](22-anomaly-detection.md)** — per-merchant
  baselines, outlier charges, duplicate detection. *TODO #2 — note price creep
  is already shipped; don't rebuild it.* Much better after 17.
- **[23-paystub-income.md](23-paystub-income.md)** — the biggest hole in the
  data model: 30–45% of gross income is invisible today. Paystub schema, three
  ingest paths, paycheck breakdown, contribution limits, tax-prep summary.
  *TODO #8.* **Needs 18.**
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
- Migrations are numbered. **`00018_budget_periods.sql` is the latest on
  `main`.** To avoid the collision class that already bit this repo once (two
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
  | `00024_documents.sql` | 18 | `documents`, `document_links` |
  | `00025_household_roles_and_splits.sql` | 21 | `users.role`, `goal_contributions`, `transaction_splits`, `child_allowances` |
  | `00026_merchant_baselines.sql` | 22 | `merchant_baselines` table |
  | `00027_paystubs.sql` | 23 | `employers`, `paystubs`, `paystub_lines` |
  | `00028_digest_entries.sql` | 25 | `digest_entries` table |
  | `00029_asset_revaluation.sql` | 26 | `asset_details`, `asset_valuations`, `manual_assets.loan_account_id` |
  | `00030_cpi_series.sql` | 27 | `cpi_series` table |
  | `00031_scenarios.sql` | 28 | `scenarios` table |
  | `00032_multi_currency.sql` | 29 | `*.currency` columns, `households.base_currency`, `fx_rates` |

  Docs **19, 20, and 24 need no migration.** Wave 3+ docs run in parallel, so
  **these reservations are load-bearing** — take only your own number.

  Two pairs can genuinely collide and need coordination rather than just
  distinct numbers:

  - **14 → 15.** 15 needs `accounts.tax_treatment` from 14's `00020` and must
    not add its own copy.
  - **21 → 16.** 16 needs an owner/admin check and observes that no role column
    exists; 21 adds `users.role`. Whichever lands second adopts the other's
    mechanism instead of inventing a parallel one.

  Docs 01, 05–09, and 12 needed **no** migration (UI, new queries, or reuse
  only). All migrations goose-annotated (`-- +goose Up` / `-- +goose Down`).
- Regenerate DB code after query/schema changes:
  `go run github.com/sqlc-dev/sqlc/cmd/sqlc@latest generate` (from `backend/`).
