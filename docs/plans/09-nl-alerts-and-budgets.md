# 09 — Natural-language alerts and budgets

## Context

Setting up an alert today means understanding the four rule types and filling in
the right config fields (threshold, percent, floor, recent-days). Users don't
think in those terms — they think *"alert me if I spend over $200 on dining in a
week."* This feature lets them type that sentence and have the AI translate it
into one of the **existing** structured alert configs (or a budget), which the
user confirms before anything is saved. Enforcement is completely unchanged: the
deterministic engine in `alerts.go` still evaluates the rule exactly as if it
had been entered through the form.

The AI does one thing only: NL → structured intent. It is a parser with a fixed,
small output vocabulary, not an open-ended agent.

## AI vs deterministic split

| Concern | Owner |
| --- | --- |
| Parsing the sentence into `{type, config}` or `{budget}` | AI (constrained to the 4 alert types + budget) |
| Validating the parsed config | Deterministic `alerts.ValidateConfig` |
| Storing the alert/budget | Existing CRUD (`CreateAlert` / `UpsertBudget`) — unchanged |
| Evaluating/firing the alert | Existing `alerts.Evaluate` — unchanged, no AI |
| Refusing an unsupported request | Deterministic (empty parse) + AI's own "unsupported" signal |

The parser's output is **never auto-saved.** It is returned as a proposal for
explicit user confirmation, then flows through the same validated write path as
the manual form. A hallucinated or malformed parse fails `ValidateConfig` and is
surfaced as "couldn't understand that" rather than persisted.

## Prerequisites

- **Doc 01 — budgets UI** (a parsed *budget* intent needs somewhere to land;
  reuses `POST /api/budgets`).
- **Existing alerts**: types + `ValidateConfig` in
  `backend/internal/alerts/alerts.go`, CRUD in
  `backend/internal/api/alert_handlers.go`, UI in
  `frontend/src/routes/Alerts.tsx`.
- Requires AI: the parse endpoint returns 503 when `!s.AI.Enabled()` (mirroring
  `handleGenerateMonthlySummary`, `summary_handlers.go:91`). The NL input is
  hidden in the UI when `capabilities.ai_enabled` is false; the form still works.

## The fixed target vocabulary

The parser may only emit one of these, because these are the only things the
engine can enforce (`alerts.go:26-34`):

| Intent | Structured output |
| --- | --- |
| `big_spend` | `{ "threshold": "200.00" }` |
| `budget_threshold` | `{ "percent": 90 }` |
| `unusual_merchant` | `{ "recent_days": 7, "min_amount": "25.00" }` |
| `low_leftover` | `{ "floor": "500.00" }` |
| `budget` | `{ "category_slug": "dining", "amount": "500.00" }` |
| *unsupported* | `{ "kind": "unsupported", "reason": "…" }` |

Note the mismatch to watch for: *"over $200 on dining in a week"* names a
category **and** a window, but `big_spend` has no category or window fields —
it flags any single purchase over the threshold, engine-wide, over a fixed 30-day
look-back (`bigSpendWindowDays`, `alerts.go:93`). The parser must map to the
closest supported rule and the confirmation UI must **show the user exactly what
will be enforced** so the gap is visible before they accept. "Per-category
weekly spend" is genuinely out of scope for the current engine — the parser
should say so via the `unsupported` branch rather than pretend `big_spend`
covers it. This honesty is the whole point of confirm-before-save.

## Data model

**None.** No new tables. Output lands in the existing `alerts` table
(JSONB `config`) or `budgets`. The parse step is stateless.

## Backend

### Reuse (concrete paths)

- `alerts.IsValidType`, `alerts.ValidateConfig`, the type constants — 
  `alerts.go:26,37,50`. **Every parsed alert config is run through
  `ValidateConfig` before it is returned as a proposal**, so the confirmation UI
  never shows an unenforceable rule.
- Category resolution for budget intents: `ListCategories`
  (`category_handlers.go:27`) to map a parsed category name → real `slug`/`id`;
  reject if no match (don't invent a category).
- Write paths on confirm: `handleCreateAlert`/`handleUpdateAlert`
  (`alert_handlers.go:81,112`) and `handleCreateBudget` (`category_handlers.go:161`)
  — all unchanged. The parse endpoint does **not** write.

### New endpoint — `POST /api/alerts/parse`

Register inside the existing `r.Route("/alerts", …)` block (`server.go:278`):

```go
r.Post("/parse", s.handleParseAlert)
```

Handler `handleParseAlert` (new, in `alert_handlers.go`):

1. 503 if `!s.AI.Enabled()`.
2. Read `{ "text": "…" }`.
3. Fetch the household's categories (for budget mapping + to give the model the
   real category list, like the chat tools do).
4. One constrained AI call (see AI notes) → a proposal object.
5. If `type` is one of the four alert types, run `alerts.ValidateConfig`; if it
   fails, return `{ "kind": "unsupported", "reason": "…" }` (don't 500 on a bad
   parse — a bad parse is an expected outcome).
6. If `kind: budget`, resolve the category name to a real slug/id; if unresolved,
   return unsupported.
7. Return the proposal **plus a human-readable `summary` string** describing
   exactly what will be enforced (built deterministically from the validated
   config, e.g. "Flag any single purchase over $200, checked over the last 30
   days"), so the UI shows engine reality, not the user's phrasing.

Response:

```json
{ "kind": "alert",
  "alert": { "type": "big_spend", "config": { "threshold": "200.00" } },
  "summary": "Flag any single purchase over $200.00 (checked over the last 30 days).",
  "caveats": ["This checks every purchase, not just dining."] }
```

or `{ "kind": "budget", "budget": {…}, "summary": "…" }`
or `{ "kind": "unsupported", "reason": "Per-merchant weekly limits aren't supported yet." }`.

**No writing happens here.** Confirmation calls the existing endpoints.

## Frontend

On `frontend/src/routes/Alerts.tsx`:

- A text input at the top ("Describe an alert in your own words…") + a "Parse"
  button, shown only when `capabilities.ai_enabled`.
- On parse, call new `api.parseAlert(text)` → `POST /api/alerts/parse`.
- Render the proposal as a **confirmation card**: the `summary`, any `caveats`
  in a muted callout, and Confirm / Edit / Cancel.
  - **Confirm (alert)**: call `api.createAlert(type, config, true)` — the same
    mutation the manual `AlertRule` already uses (`Alerts.tsx:202`). If a rule
    of that type exists, route to `api.updateAlert` (types are singleton per the
    existing `TYPE_META`/`ORDER` model, `Alerts.tsx:56`).
  - **Confirm (budget)**: `api.createBudget(categoryId, amount)`.
  - **Edit**: pre-fill the existing per-type form (`TYPE_META` fields) with the
    parsed config so the user tweaks in the familiar UI, then saves normally.
  - **Unsupported**: show `reason` plainly; no save button.
- Invalidate `['alerts']` / `['budgets']` on confirm, as the existing mutations do.

## AI notes

- Use the **tool-use / forced-tool pattern** from the chat layer
  (`chat_handlers.go:215` for `ai.Tool` + `InputSchema`, `executeChatTool` for
  reading `use.Input`). Define one tool `propose_rule` whose `InputSchema` is a
  oneOf over the six shapes above, and set `ToolChoice` to force it. Parsing the
  result is a JSON unmarshal, not prose scraping. Use `ai.Client.Complete`
  (non-streaming).
- System prompt fixes the vocabulary hard: *"You translate a user's sentence
  into exactly one of these budgeting rules. You may ONLY use the listed types.
  If the request needs a capability not listed (a per-category or per-window
  spend limit, a per-merchant cap, a time-of-day rule, etc.), return kind
  `unsupported` with a short reason — do NOT force it into a rule that doesn't
  match. Amounts are decimal strings. Never invent a category; choose from the
  provided list or return unsupported."* Inject today's date (as the chat prompt
  does, `chat_handlers.go:136`) so "this month" resolves.
- Give the model the real category list (names + slugs) so budget intents map to
  existing categories, mirroring how the chat prompt learns names from
  `spend_by_category`.
- Defense in depth: the model's `unsupported` branch AND the Go
  `ValidateConfig`/category-resolution both gate the write. Either one failing
  yields a non-savable proposal. The model is never trusted to have produced a
  valid config — validation is deterministic and authoritative.

## Verification

- `docker compose up -d --build` (AI enabled in the dev stack).
- Drive good cases: POST `{"text":"tell me about any purchase bigger than
  $300"}` → expect `big_spend` `{threshold:"300.00"}`, valid summary. POST
  `{"text":"budget $600 a month for groceries"}` → `budget` mapped to the
  groceries slug.
- Drive the honest-refusal case: POST `{"text":"alert me if I spend over $200 on
  dining each week"}` → expect either a `big_spend` proposal *with a caveat that
  it isn't dining/weekly-specific*, or `unsupported`. Confirm the UI shows the
  caveat.
- Drive a garbage case: `{"text":"what's the weather"}` → `unsupported`, no
  crash.
- Confirm flow writes through existing endpoints: after Confirm, `GET
  /api/alerts/` shows the rule; trigger it with sandbox data and verify
  `alerts.Evaluate` (via the alerts sweep) raises an event — proving enforcement
  is the unchanged deterministic path.
- `go build/vet`, `go test -p 1 ./...` (add a table test that a set of sentences
  parse to expected configs and all pass `ValidateConfig`). Frontend
  `tsc/build/lint`.

## Out of scope

- New alert types or new config fields (per-category, per-window, per-merchant
  limits). The parser maps to what exists; extending the engine is a separate
  effort.
- Editing/deleting rules via NL (only creation/update from a sentence).
- Multi-rule parses from one sentence (one proposal per call).
- Auto-saving without confirmation.
- Free-text budgets outside a known category.
