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
