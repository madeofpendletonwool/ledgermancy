# 01 — Budgets management UI

## Context

The budget feature is **backend-complete and frontend-invisible**. The schema,
CRUD API, and reporting query all exist and work; there is simply no page that
calls them. A user can see budget *progress* nowhere, and has no way to set,
edit, or delete a budget. This doc builds the missing UI over the existing API.
No new backend is required for the core deliverable.

This is a Wave-0 foundation doc: `08-budget-suggestions.md` and
`09-nl-alerts-and-budgets.md` both assume a working budgets surface.

## AI vs deterministic split

Entirely deterministic. No AI in this doc. Every figure (budgeted, spent,
remaining) is already computed in SQL and arrives as a decimal string; the UI
only renders and, for the progress bar, divides two server-exact strings for a
percentage width (display-only, never a headline number).

## Prerequisites

None. This doc unblocks others; it depends on nothing.

## Data model

No changes for the core task. The `budgets` table already exists
(`backend/internal/db/migrations/00001_core_schema.sql`, ~line 246) with columns
`owner_scope` (`'household'|'user'`), `user_id`, `period` (`'monthly'|'yearly'`),
`amount NUMERIC(20,4)`, and `effective_from DATE`. The unique constraint that
`UpsertBudget`'s `ON CONFLICT` targets lives in
`00003_budget_unique.sql`.

## Backend

**Already exists — reuse as-is, do not rebuild:**

- Routes in `backend/internal/api/server.go` (~line 220):
  ```
  r.Route("/budgets", …)
    GET    /          → s.handleBudgetProgress
    POST   /          → s.handleCreateBudget   (upsert)
    DELETE /{budgetID}→ s.handleDeleteBudget
  ```
- Handlers in `backend/internal/api/category_handlers.go` (lines 114–214):
  `handleBudgetProgress` (returns `budgetResponse{budget_id, category_id, name,
  slug, color, budgeted, spent, remaining}`), `handleCreateBudget` (takes
  `{category_id, amount}` where `amount` is a decimal **string**, validates
  `> 0`), `handleDeleteBudget`.
- Queries in `backend/internal/db/queries/reports.sql` (lines 256–296):
  `GetBudgetProgress` (each household budget + spend this period, scoped by
  `user_id = $2 OR is_shared`), `UpsertBudget` (**hardcodes**
  `owner_scope='household'`, `period='monthly'`), `DeleteBudget`.

The upsert semantics matter for the UI: **POST is create-or-update**. Setting a
budget for a category that already has one overwrites the amount. There is no
separate edit endpoint — "edit" is just another POST for the same category.

Category list for the picker comes from the existing `GET /api/categories`
(`handleListCategories`); filter out `is_income` and `is_transfer` categories
client-side — you only budget spending categories.

## Frontend

Build a new `Budgets` route. Follow the conventions in
`frontend/src/routes/Spending.tsx` and `frontend/src/routes/Dashboard.tsx`:
`glass` cards for sections, the `recentMonths()` month-selector pattern from
`Spending.tsx` (lines 9–22, 59–75), `formatMoney` from `frontend/src/lib/money.ts`,
and TanStack Query for fetching/mutating.

**Wiring the existing (currently-uncalled) client stubs** in
`frontend/src/lib/api.ts`:
- `api.budgets(params)` — line ~595, returns `BudgetProgress[]` (interface at
  lines 227–236). Accepts `PeriodQuery {from,to}`.
- `api.setBudget(categoryID, amount)` — line ~598, POSTs the upsert.
- `api.deleteBudget(id)` — line ~604.

These are already defined and typed; this doc is the first caller. No changes to
`api.ts` needed for the core task.

**Page structure (`frontend/src/routes/Budgets.tsx`):**

1. Header + month selector (copy `recentMonths()` and the `<select>` block from
   `Spending.tsx`). Pass `{from, to}` into `api.budgets()`.
2. A summary row of `glass` tiles: total budgeted, total spent, total remaining
   for the month (sum the server-exact strings for display only, mirroring the
   note in `Dashboard.tsx`'s `sumBalances`).
3. A list of budget rows, one per budgeted category, each showing a **progress
   bar** of `spent / budgeted`. Model the bar on `SplitTile` in
   `Spending.tsx` (lines 230–258): a low-contrast track with a filled inner bar
   whose width is `Math.min(pct, 100)%`. Color the fill by state — under budget
   vs over budget — using the `STATUS` tokens (`frontend/src/components/charts/tokens.ts`,
   already imported by `Spending.tsx`). Clamp/guard the divide (a `budgeted` of
   0 must not produce `NaN`).
4. Each row has an inline edit (a number input + Save that calls `setBudget`) and
   a delete button (`deleteBudget`). On success, invalidate the `['budgets', …]`
   query key so the list refetches.
5. An "Add budget" control: a category picker (from `api.categories()`, minus
   income/transfer) + amount input → `setBudget`. Because POST is an upsert,
   picking a category that already has a budget simply updates it — the UI
   should show existing budgets pre-filled rather than offering a duplicate.

**Empty state:** categories with no budget yet — offer to add one. Follow the
`Empty` component pattern in `Dashboard.tsx`.

**Router** — add the route in `frontend/src/App.tsx`: import `Budgets`, add
`<Route path="/budgets" element={<Budgets />} />` inside the authed layout block
(alongside `/spending` at line ~48).

**Nav** — add to `NAV` in `frontend/src/components/AppLayout.tsx` (the array at
lines 10–20), e.g. `{ to: '/budgets', label: 'Budgets', end: false }`, slotted
near Spending. No capability gate — budgets are not an AI feature.

## AI notes

None.

## Verification

- `docker compose up -d --build`; the sandbox has ~102 transactions.
- Create a budget from the UI; confirm a row appears in psql:
  `SELECT category_id, amount, owner_scope, period FROM budgets;` — expect
  `owner_scope='household'`, `period='monthly'`.
- Drive the API directly to cross-check the UI:
  `GET /api/budgets?from=YYYY-MM-01&to=YYYY-MM-DD` returns budgeted/spent/
  remaining; confirm `spent` matches `GET /api/reports/by-category` for the same
  category and period.
- Edit (re-POST same category) → amount updates, no duplicate row. Delete →
  204, row gone.
- Frontend: `npm run build` / `tsc` / lint clean in `frontend/`.
- `go build ./...` and `go vet ./...` in `backend/` (should be untouched for the
  core task).

## Optional stretch — personal & yearly budgets (clearly out of the core scope)

Currently unreachable because `UpsertBudget` hardcodes `owner_scope='household'`
and `period='monthly'`, and `handleCreateBudget` sends neither. The table and
its check constraints already support `owner_scope='user'` (with `user_id`),
`period='yearly'`, and `effective_from`. To expose them:

1. Widen `UpsertBudget` in `reports.sql` to take `owner_scope`, `user_id`,
   `period`, `effective_from` as parameters (mind the partial unique index in
   `00003_budget_unique.sql` — the `ON CONFLICT` target differs for
   `user_id IS NULL` vs personal). Regenerate sqlc (`go run
   github.com/sqlc-dev/sqlc/cmd/sqlc@latest generate` from `backend/`).
2. Extend `createBudgetRequest` (`category_handlers.go` line 156) and
   `GetBudgetProgress` to scope/return the new fields.
3. Extend `api.setBudget` and the UI (a household/personal toggle, a
   monthly/yearly toggle).

Ship the core household-monthly UI first; treat this as a follow-up.

## Out of scope

- Any AI ("suggest a budget" is `08-budget-suggestions.md`).
- Budget-threshold alerts (already handled by the alerts engine).
- Rollover / carry-over budgets, multi-currency budgets.
