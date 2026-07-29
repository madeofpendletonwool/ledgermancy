# Roadmap

The living source of truth is [`TODO.md`](https://github.com/madeofpendletonwool/ledgermancy/blob/main/TODO.md)
in the repo. This page mirrors it for the docs nav.

## Current status

**Phases 1–7 complete.** Auth + two-factor, households, Plaid ingest,
categorisation, the spending dashboard, net worth (investments, liabilities,
manual assets, snapshots, projections), the exportable Financial Summary report,
AI enrichment (LLM categorisation, insight feed, alerts, monthly narratives,
natural-language parsing), and the tool-calling chatbot are all running.

## Known gaps

Deliberate gaps — not bugs:

- **Insights don't push in real time.** The proactive feed surfaces in-app and
  in the digest, but doesn't ping your notification channel the moment it's
  detected the way an alert does. Alerts are the real-time path; insights are
  pull + digest.
- **Debt payoff is single-debt only.** Payoff goals work end to end, but there's
  no strategy *across* debts — snowball vs. avalanche ordering, or modelling
  extra one-off payments. That belongs with the proactive advisor.

## Recently shipped

- **Debt-payoff goals** — exact-decimal amortization (payoff date, total
  interest, required payment), with an explicit "this is never paid off" when
  the payment is below the interest.
- **Optional Plaid products on existing connections** — enabling Investments or
  Liabilities now backfills accounts you already linked, no relink required.
- Thousands separators in every server-generated figure.
- Category typing: **spending / income / transfer** (fixes card payments and
  self-transfers inflating spend).
- A transfer/card-payment **detection heuristic** for `OTHER_OTHER` cases.
- Duplicate-category guard.
- **Transactions** filtering by category and **multiple accounts**, URL-driven.
- **Click a day or category** in charts to drill into transactions.
- Period-scoped **insights auto-expire**.
- A generic **CSV importer** that de-duplicates against synced data and runs
  through the same categoriser.

## Still planned

- **Monthly recap overhaul** — money formatting, real per-category breakdown fed
  to the model, present tense for the in-progress month, weekly auto-generation
  with a final past-tense recap on month close.
- **Smarter recurring detection** — recency gate, per-merchant "not recurring"
  override, better cadence detection.
- **Insight expansion** — projected month-end cash flow, unusually-large single
  transaction, income-change detection, savings-rate milestones, goal-progress
  nudges; plus real-time insight push.
- **Budget expansion** — "safe to spend", rollover/envelope budgets, non-monthly
  periods, percentage / zero-based allocation, budget-vs-actual trend.

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
7. **Chatbot** — tool-calling agent over your own financial data.
