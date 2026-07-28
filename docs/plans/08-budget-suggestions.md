# 08 — Budget suggestions

## Context

A household that has never set a budget faces a blank grid: they must guess a
number for every category from memory. The reporting layer already knows the
answer — `GetCategoryAverages` computes each category's true monthly average
from up to a year of transactions. This feature turns that fact into an
actionable proposal: for each spending category, propose a sensible *round*
budget target anchored on its computed average, explain the one-line reasoning,
and let the user review and approve. Approved rows go through the existing
budget write API unchanged.

The value is entirely in the framing. The average is a jagged number
(`$487.63`); a budget people can hold in their head is a round one (`$500`).
Picking that round target and phrasing *why* is a language/judgement task; the
average itself is not.

## AI vs deterministic split

| Concern | Owner |
| --- | --- |
| Per-category monthly average, annual total, txn count | SQL (`GetCategoryAverages`) — already exact |
| Which categories to propose (skip fixed? skip trivially small?) | Deterministic Go filter |
| Rounding the average to a memorable target | AI (bounded), with a deterministic fallback |
| One-line rationale text | AI phrasing only |
| Writing the approved budget | Existing `UpsertBudget` — unchanged |

The model is handed the finished `StringFixed(2)` average and returns a target +
a sentence. **It never sees raw transactions and never computes the average.**
If a returned target is not a plausible round number near the average (see
guardrail below) it is discarded and the deterministic rounding is used, so a
hallucinated figure can never reach the write API.

## Prerequisites

- **Doc 01 — budgets UI.** This feature adds a "Suggest budgets" action and a
  review panel *onto* that page. Doc 01 owns the base grid, the
  `api.createBudget` client method, and `GET /api/budgets` progress rendering.
- AI is optional. With no key configured the endpoint still returns proposals,
  just with deterministic rounding and template rationale (see AI notes).

## Data model

**None.** No new tables. Proposals are computed on demand and never persisted —
they are a view over `GetCategoryAverages` plus a target. Approval writes to the
existing `budgets` table via `UpsertBudget` (`reports.sql:286`). Existing
budgets are read back through `GetBudgetProgress` so the UI can show which
categories already have a budget and pre-check "skip these".

## Backend

### Reuse (concrete paths)

- Query: `GetCategoryAverages` — `backend/internal/db/queries/reports.sql:121`.
  Params `HouseholdID, UserID, Date, Date_2`; returns `CategoryID, CategoryName,
  CategorySlug, CategoryColor, IsFixed, Total, MonthlyAverage, TransactionCount`.
  Trailing-12-month default range is set exactly as in
  `handleCategoryAverages` (`report_handlers.go:215`) — copy that date defaulting.
- Existing budgets: `GetBudgetProgress` (`reports.sql:256`) to know which
  categories are already budgeted.
- Write path: `handleCreateBudget` / `UpsertBudget` — **unchanged.** The UI
  calls `POST /api/budgets` once per approved row, exactly as doc 01's manual
  entry does. This feature does **not** add a bulk-write endpoint; approval is a
  loop of existing single writes so validation and audit stay identical.

### New endpoint — `POST /api/budgets/suggest`

Register in `server.go` inside the existing `r.Route("/budgets", …)` block
(`server.go:220`), authenticated like its siblings:

```go
r.Post("/suggest", s.handleSuggestBudgets)   // add alongside Get/Post/Delete
```

Handler `handleSuggestBudgets` (new, in `category_handlers.go` next to the
other budget handlers):

1. Resolve identity; run `GetCategoryAverages` with the trailing-12-month
   default range (reuse `report_handlers.go:215` logic).
2. Deterministic filter: drop income/transfer (already excluded by the query),
   drop categories whose `MonthlyAverage` is below a floor (e.g. `< $10` — not
   worth a budget), and optionally drop `IsFixed` categories or flag them
   (`is_fixed` passed through so the UI can group them). Cap at the top N by
   total (e.g. 20) to bound the AI call.
3. For each surviving category build a `SuggestionInput{Name, MonthlyAverage
   string, IsFixed bool}` — average as `StringFixed(2)`.
4. If `s.AI.Enabled()`: one AI call for the whole batch (see AI notes) returns a
   `{slug → {target, rationale}}` map. Otherwise use deterministic rounding and
   a template rationale.
5. **Guardrail**: for each proposal, parse the AI target as decimal; accept only
   if it is within a band of the average (e.g. `0.8×avg ≤ target ≤ 1.3×avg`) and
   is "round" (divisible by a step that scales with magnitude — see AI notes).
   On failure, substitute the deterministic rounding. The `computed_average` in
   the response is **always** the SQL figure, never the model's.

Response shape:

```json
{ "period_months": 12,
  "proposals": [
    { "category_id": "…", "category_name": "Groceries", "slug": "groceries",
      "is_fixed": false, "computed_average": "487.63",
      "suggested_amount": "500.00", "rationale": "You've averaged $487.63/mo…",
      "already_budgeted": true, "current_budget": "450.00" } ] }
```

`already_budgeted`/`current_budget` come from a `GetBudgetProgress` lookup so the
UI can show "raise from $450 → $500" rather than proposing blind.

### Deterministic rounding (the fallback and the guardrail's anchor)

A pure-Go helper `roundTarget(avg decimal.Decimal) decimal.Decimal`: round *up*
to a step chosen by magnitude — nearest $10 under $200, nearest $25 under $500,
nearest $50 under $1000, nearest $100 above. This is what the model is asked to
mimic, and what is used verbatim when AI is off or the guardrail rejects a
value. Round up, not nearest, so a budget is never set below the historical
average by construction.

## Frontend

On the doc 01 Budgets route:

- A **"Suggest budgets"** button. On click, call a new `api.suggestBudgets()`
  (`frontend/src/lib/api.ts`, mirroring `api.budgets`) → `POST
  /api/budgets/suggest`.
- Render a review panel: one row per proposal showing category, computed
  average, the suggested amount in an **editable** field (pre-filled, user can
  override), the rationale as helper text, and a checkbox (default checked;
  default *un*checked when `already_budgeted` and the current budget already
  covers the average). Group `is_fixed` categories separately with a note that
  fixed costs are usually budgeted at their actual amount.
- An "Apply selected" button loops the checked rows through the existing
  `api.createBudget(categoryId, amount)` mutation, then invalidates the
  `['budgets']` query so the grid refreshes.
- The "Suggest budgets" button is **not** AI-gated in the nav sense — the
  endpoint works without AI. But show a subtle "AI-tailored" vs "rule-based"
  label depending on `capabilities.ai_enabled`, so the rationale text sets the
  right expectation.

## AI notes

- **One batched call**, not one per category — cheaper and lets the model keep
  targets consistent across the list. Modeled on `buildSummaryInput`
  (`summary_handlers.go:137`): hand the model finished strings only.
- Prompt shape: a system prompt like *"You suggest round, memorable monthly
  budget targets. You are given each category's true average monthly spend
  (already computed — never recompute or invent it). For each, pick a round
  target at or slightly above the average that a person can remember, and write
  one warm sentence citing the average. Round to sensible steps ($10/$25/$50/
  $100 by size). Return JSON only."* User content: the category lines with
  `Name: $Average`.
- Prefer the **tool-use / structured-output path** used by the chat layer
  (`chat_handlers.go:215` tool defs, `executeChatTool`) — define a single tool
  whose `InputSchema` is `{proposals: [{slug, target, rationale}]}` and force it
  with `ToolChoice`, so parsing is a JSON unmarshal of the tool input rather
  than scraping prose. Reuse `ai.Client.Complete` (non-streaming; this is not a
  chat turn).
- Guardrail belongs in Go, not the prompt: even a well-behaved model output is
  validated against the band + roundness test before it can populate
  `suggested_amount`. The rationale text is never validated for numbers because
  it is never a source of truth — but the handler should still substitute the
  SQL average string into the response's `computed_average` regardless of what
  the model echoed.

## Verification

- Seed: the dev stack's ~102 sandbox transactions already yield non-trivial
  averages. `docker compose up -d --build`.
- Drive: `curl -XPOST localhost:8080/api/budgets/suggest` (with an authed
  cookie); confirm every `computed_average` matches `GET
  /api/reports/averages` for the same category, and every `suggested_amount`
  is ≥ its average and round.
- Cross-check psql: run `GetCategoryAverages` by hand for one category and
  diff against the response.
- AI-off path: unset the AI key, restart, confirm proposals still return with
  deterministic rounding and template rationale.
- Approve flow: apply two proposals, confirm two `budgets` rows via
  `GetBudgetProgress` / psql, and that re-running suggest now marks them
  `already_budgeted`.
- `go build ./... && go vet ./... && go test -p 1 ./...` (throwaway PG per
  README). Frontend: `tsc --noEmit && npm run build && npm run lint`.

## Out of scope

- Bulk budget-write endpoint (approval reuses single `POST /api/budgets`).
- Per-user (owner-scoped) budgets — `UpsertBudget` writes household scope only.
- Auto-applying suggestions without review (always confirm-before-write).
- Learning/adjusting targets over time, seasonality, or envelope budgeting.
- Persisting dismissed suggestions.
