# 12 — Manual transactions

## Context

Aggregator feeds drop transactions. Capital One's Plaid feed, in particular, has
been observed omitting a specific posted charge indefinitely while *later* ones
flow through — so it is not a lag that self-heals, it is a hole. The account
*balance* stays correct (it comes from Plaid's balance feed, independent of the
transaction list), but every **transaction-derived** figure — category spend,
budgets, savings rate, monthly trend — silently **undercounts** by the missing
amount, and always in the same direction (you only ever lose transactions, never
gain phantom ones), so spending history drifts low over time.

This feature lets a user **enter, edit, and delete transactions by hand** so they
can reconcile these gaps (the intended workflow: a month-end sweep comparing the
issuer statement against the app). It is almost entirely additive — the schema
already supports it — plus a duplicate guard for the case where the issuer
eventually *does* deliver a charge the user already entered.

## AI vs deterministic split

**No AI.** This is CRUD over the existing `transactions` table. (A manually
entered transaction *does* flow into the AI features later — insights, digest —
but only as ordinary transaction data.)

## Prerequisites

None. Independent of every other doc. Pairs naturally with **doc 01 (budgets
UI)** as Wave 0 "ledger management UI" work. **No migration** — the schema is
ready (see below).

## Data model — reuse, no new table

`transactions` (`backend/internal/db/migrations/00001_core_schema.sql:155`)
already carries everything needed:
- `source TEXT` with `CHECK (source IN ('plaid','csv','manual'))` — manual rows
  set `source='manual'`.
- `plaid_transaction_id` is nullable and only `UNIQUE` — manual rows leave it
  `NULL`, so they never collide with Plaid rows and are never touched by sync
  (`UpsertTransaction` keys `ON CONFLICT (plaid_transaction_id)`).
- `amount NUMERIC(20,4)` — **positive = money out** (Plaid convention); a manual
  purchase is positive, a refund/credit negative.
- `category_id` + `category_source` — set `category_source='manual'` (sticky).
- `account_id NOT NULL` — a manual transaction attaches to an existing account.

**Balance independence (important):** `accounts.current_balance` is written only
by Plaid account sync (`syncAccounts` → `UpsertAccount`). Manual transactions
**never touch balances**, so entering the $11.86 by hand corrects the spending
math *without* double-counting what you owe (the balance already includes it).
State this in the UI so users trust it.

**Reports include manual rows automatically.** The report queries
(`GetSpendingByCategory`, `GetSpendingSummary`, `GetBudgetProgress`, etc. in
`backend/internal/db/queries/reports.sql`) filter on visibility / `is_active` /
`NOT excluded_from_reports` / `NOT pending` / date — **not on `source`** — so a
manual transaction lands in every total, category, and budget with no query
change.

## Backend

New sqlc queries in `backend/internal/db/queries/transactions.sql`, all
**household-scoped** via the `account → plaid_item → user` join (copy the
scoping from `SetTransactionCategory`, `backend/internal/db/queries/categories.sql:56`)
and all **guarded to `source='manual'`** so a manual endpoint can never mutate a
Plaid-synced row:
- `CreateManualTransaction` — insert with `source='manual'`, `pending=false`,
  caller-supplied `account_id` (verified to belong to the caller's household),
  `date`, `amount`, `name`, `merchant_name`, `category_id`, `notes`, and a
  `merchant_key` computed via `plaid.MerchantKey(merchantName, name)`
  (`backend/internal/plaid/merchant.go:43`) so the categorization cache and
  recurring detector see it like any other row.
- `UpdateManualTransaction` — `WHERE id=$ AND source='manual' AND <household>`.
- `DeleteManualTransaction` — same guard.

Handlers in a new `backend/internal/api/ledger_handlers.go` block (next to
`handleListTransactions`), following `handleRecategoriseTransaction`'s pattern
(`backend/internal/api/category_handlers.go:67` — parse id, decode, household-
scoped write, `pgx.ErrNoRows` → 404). Verify the target `account_id` is visible
to the caller before insert.

Routes (extend the existing `/transactions` group, `server.go:215`):
```
r.Post("/",                    s.handleCreateManualTransaction)
r.Put("/{transactionID}",      s.handleUpdateManualTransaction)   // manual only
r.Delete("/{transactionID}",   s.handleDeleteManualTransaction)   // manual only
```

### Duplicate guard

When the issuer later delivers a charge the user already entered, there is no
automatic dedup (Plaid row has a `plaid_transaction_id`; the manual row does
not). Do **not** auto-delete — flag it and let the user decide.

- New read query `ListManualDuplicateCandidates` (or a computed flag on the
  transaction list): a manual transaction is a "possible duplicate" when a
  `source='plaid'` transaction exists on the **same account**, **same amount**,
  with `abs(date - date) <= 4 days`. Keep the window tight to avoid false hits.
- Surface as a badge on the manual row in the UI with a one-click "this is the
  duplicate — delete my manual entry" action. Compute at **read time** (a query
  the transactions view runs), not in the sync hot path, so sync stays simple.

## Frontend

`frontend/src/routes/Transactions.tsx` (the existing list):
- **"Add transaction"** button → a modal form: account picker (from
  `api.accounts()` / `handleListAccounts`), date, amount with an
  **expense/income toggle** that sets the sign (users think in "I spent $X", not
  signed money-out), merchant name, category picker (from `api.categories()` /
  `ListCategories`), optional notes.
- **Edit / delete** affordances shown **only on `source==='manual'` rows**;
  Plaid rows stay read-only except category (which already has the PATCH path).
  Requires adding `source` to the transaction list response + `Transaction` type
  in `frontend/src/lib/api.ts` (it is not currently exposed).
- **Duplicate badge** on flagged manual rows with the resolve action.
- New `api.ts` methods (`createTransaction`, `updateTransaction`,
  `deleteTransaction`) via the existing `request()` helper (CSRF handled).

## Verification

- Create a manual transaction via the API/UI on the Quicksilver account; confirm
  it appears in `/transactions` and that `GetSpendingByCategory` /
  `GetSpendingSummary` for that month **increase by exactly the amount** (mirror
  the seeding style in `backend/internal/db/report_queries_test.go`), while
  `ComputeNetWorth` and the account balance are **unchanged**.
- Edit and delete it; confirm household scoping (a valid id from another
  household 404s) and that the endpoints **refuse a `source='plaid'` id**.
- Insert a matching Plaid-style row and confirm the manual one is flagged as a
  possible duplicate; resolve deletes only the manual row.
- `cd backend && go build ./... && go vet ./...` and the DB-backed suite with
  `TEST_DATABASE_URL`; frontend `tsc -b`, `npm run build`, `npm run lint`.

## Out of scope

- **Manual accounts** — manual transactions attach to existing (Plaid-linked)
  accounts only; a fully manual/cash account is separate future work.
- **CSV import** — a distinct path (`source='csv'` already reserved); not this
  doc.
- **Balance reconciliation** — the app deliberately never derives balance from
  transactions, so a manual entry closes the *spending* gap only; it does not and
  should not adjust the Plaid-sourced balance.
