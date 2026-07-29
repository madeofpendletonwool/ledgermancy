# Roadmap

The living source of truth is [`TODO.md`](https://github.com/madeofpendletonwool/ledgermancy/blob/main/TODO.md)
in the repo. This page mirrors it for the docs nav.

## Current status

**Phases 1–7 complete.** Auth + two-factor, households, Plaid ingest,
categorisation, the spending dashboard, net worth (investments, liabilities,
manual assets, snapshots, projections), the exportable Financial Summary report,
AI enrichment (LLM categorisation, insight feed, alerts, monthly narratives,
natural-language parsing), and the tool-calling chatbot are all running.

The major-initiative waves are landing too: the bill calendar, dedicated
investments page, and FIRE projections are in (wave 3), as are merchant
canonicalization, the encrypted document vault, debt-payoff goals, the
installable PWA, and household people + kid accounts + bill split (wave 4).

## Known gaps

Deliberate gaps — not bugs:

- **Debt payoff is single-debt only.** Payoff goals work end to end, but there's
  no strategy *across* debts — snowball vs. avalanche ordering, or modelling
  extra one-off payments. That belongs with the proactive advisor.
- **An item's transaction history window cannot be widened after linking.**
  Plaid fixes it at link time; update mode preserves it but cannot raise it, and
  relinking orphans the history tied to the old item. The CSV importer is the way
  to backfill further where an institution caps what it shares.

## Recently shipped

- **Household people, kid accounts, shared goals, bill split** — a *person* is
  now distinct from a *login*, so a child with a 529 exists without credentials.
  Custodial accounts are segregated from the nest egg; shared-goal contributions
  attribute who funded what; transaction splits and a household ledger track who
  owes whom.
- **Smart merchant canonicalization** — fragmented descriptors group into one
  canonical business at `/merchants`, suggestion-then-confirm, so a subscription
  split across two descriptors is finally detected as recurring.
- **Encrypted document vault** — receipts, tax returns, warranties, policies and
  contracts sealed with the existing AES-GCM cipher, linked to transactions,
  assets and goals, with expiry nudges and optional receipt OCR.
- **Retirement & FIRE projections** — an account-aware engine at `/retirement`
  beside the linear model: real returns, pooled contribution limits, an FI age,
  and a bounded savings-rate solve.
- **Dedicated Investments page** — time- and money-weighted returns in exact
  decimal, growth rebased against benchmarks, allocation, and a holdings table.
- **Bill calendar + cash-flow forecast** — a `/schedule` page over recurring
  obligations, a month grid, day-by-day projected balances, and safe-to-spend
  integration.
- **Installable PWA** with read-only offline — installs to a home screen and
  re-renders the last figures under a banner stating when they were saved.
- **Debt-payoff goals** — exact-decimal amortization with an explicit "this is
  never paid off" when the payment is below the interest.
- Optional Plaid products on existing connections, thousands separators in every
  server-generated figure, category typing (spending / income / transfer),
  transfer/card-payment detection, a duplicate-category guard, transactions
  filtering by category and multiple accounts, chart drill-down, period-scoped
  insight auto-expiry, and a generic CSV importer.

## Still planned

The incremental polish on the original feed is done — thousands separators in
generated prose now come from a shared formatting helper every insight, recap and
alert body routes through.

What remains is the next tier of major initiatives, each with an execution-ready
plan in [`docs/plans/`](https://github.com/madeofpendletonwool/ledgermancy/blob/main/docs/plans/):

- **Predictive anomaly detection** — per-merchant baselines, outlier charges,
  duplicate detection.
- **Pre-tax income & deduction tracking** — a paystub importer closing the
  30–45% of gross income that is currently invisible.
- **Proactive cash-flow advisor** — ranked, deterministic options for surplus
  cash.
- **In-app weekly digest**, **real-asset revaluation & depreciation**,
  **inflation-adjusted views**, the **decision-modeling** what-if engine, and
  (far future) **multi-currency**.

See the [plan docs README](https://github.com/madeofpendletonwool/ledgermancy/blob/main/docs/plans/README.md)
for the wave order and dependency graph.

## Build history

The original seven-phase plan, for reference:

1. **Foundation** — scaffold, compose, config, schema, auth, health endpoint.
2. **Transactions ingest** — Plaid Link, `/transactions/sync` with cursor, full
   historical backfill, webhooks, CSV import.
3. **Reporting core** — categorization, monthly rollups, spending dashboard,
   per-category averages, annual totals, savings rate.
4. **Net worth + modules** — Investments and Liabilities modules, manual assets,
   monthly net-worth snapshots, projections.
5. **Financial summary** — exportable PDF + CSV report.
6. **AI enrichment** — LLM categorization fallback, recurring detection, alerts.
7. **Chatbot** — tool-calling agent over your financial data.
