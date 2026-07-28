# 11 — Goal coaching

## Context

The app can tell you where your money *is* and, via the forecast (doc 06 /
`networth.Project`), where it's *heading* — but it has no concept of where you
*want* it to go. There is no goal or target anywhere in the schema. This feature
adds savings/target goals ("save $10k for a trip by December"), computes whether
you're on track using the existing deterministic projection engine, and — as an
insight producer on the doc-04 spine — coaches with a phrased nudge ("you're
$180/mo short of your trip goal").

This is the only **greenfield-schema** feature in the set: a new `goals` table,
CRUD API, and a Goals UI. NL goal entry is parsed by AI (confirm-before-save,
exactly like doc 09). All feasibility math is deterministic; the AI only phrases
coaching and parses the sentence.

## AI vs deterministic split

| Concern | Owner |
| --- | --- |
| Required monthly contribution, months remaining, on-track/behind | Deterministic (projection engine + decimal) |
| Current progress toward a goal (linked balance / accumulated) | SQL |
| Parsing "save $10k for a trip by December" → structured goal | AI (confirm-before-save) |
| Coaching/nudge wording | AI phrasing over finished strings |
| Storing goals, evaluating status | Deterministic CRUD + producer |

The producer computes the shortfall in `decimal` and hands the model finished
`StringFixed(2)` strings; the model never divides target by months. A parsed
goal is a proposal, never auto-saved.

## Prerequisites

- **Doc 04 — insight engine**: goals surface coaching through a `Producer`
  (`insights.Producer`, shared contracts §1) emitting `kind: "goal"` insights.
- **Doc 06 — forecast narration** / `networth.Project` + `Assumptions`
  (`backend/internal/networth/project.go`). The feasibility path reuses the same
  compounding projection, so a goal's math agrees with the forecast the user
  already sees.
- AI is optional throughout: NL entry is hidden without a key (form still
  works); the coaching producer emits template text when AI is off, per the
  doc-04 engine contract.

## Data model

### `goals` table — migration `000NN_goals.sql`

**Migration number**: this depends on the insights migration (`00009` per shared
contracts) and doc 06's forecast work. Take the **next free number after
whatever the insights and preferences/notify migrations consumed** — do not
hard-code `00009`; it is already claimed by insights. Coordinate with docs
02/03/04 for the actual number and state it in the PR.

```sql
-- +goose Up
CREATE TABLE goals (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id  UUID        NOT NULL REFERENCES households (id) ON DELETE CASCADE,
    -- Scope mirrors budgets/preferences: a shared household goal or a personal
    -- one. user_id set iff scope='user'.
    scope         TEXT        NOT NULL CHECK (scope IN ('user','household')),
    user_id       UUID        REFERENCES users (id) ON DELETE CASCADE,
    kind          TEXT        NOT NULL,  -- 'savings' | 'debt_payoff' (start with 'savings')
    name          TEXT        NOT NULL,  -- "Trip to Japan"
    target_amount NUMERIC(20,4) NOT NULL,      -- money out convention N/A; this is a positive target
    target_date   DATE,                        -- nullable: an open-ended goal
    -- Optional links that make progress measurable without manual updates.
    account_id    UUID        REFERENCES accounts (id)   ON DELETE SET NULL,
    category_id   UUID        REFERENCES categories (id) ON DELETE SET NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    achieved_at   TIMESTAMPTZ,                 -- set when progress first reaches target
    archived_at   TIMESTAMPTZ
);
CREATE INDEX goals_household_idx ON goals (household_id, archived_at);

-- +goose Down
DROP TABLE IF EXISTS goals;
```

Money is `NUMERIC(20,4)` / `shopspring/decimal` / `StringFixed(2)` on the wire,
per shared-contracts conventions. Progress is **derived**, not stored: for a
`savings` goal linked to an account it's that account's balance; unlinked, it's
the household's accumulated surplus since `created_at` (deterministic SQL). This
avoids a stale `current_amount` column drifting from reality.

## Backend

### New package / queries

- `backend/internal/db/queries/goals.sql` — `CreateGoal`, `ListGoals`
  (scoped: household + own, mirroring the visibility filter in shared
  contracts §Conventions), `GetGoal`, `UpdateGoal`, `ArchiveGoal`,
  `MarkGoalAchieved`. Regenerate sqlc (`sqlc generate` from `backend/`).
- Progress query: sum toward the goal. For a linked account, reuse the balance
  the net-worth layer already reads. For an unlinked savings goal, a query for
  surplus (income − spending) accumulated since `created_at`, using the same
  spend/income definitions as `GetSpendingSummary` (`reports.sql:16`).

### Feasibility (deterministic)

A helper `goal.Feasibility(target, current, targetDate, monthlySurplus)` in a
new `backend/internal/goals/` package, computing in `decimal`:

- `remaining = target − current`
- `months_left = whole months from now to target_date` (nil date → open-ended)
- `required_monthly = remaining / months_left` (guard months_left ≥ 1)
- `on_track = monthlySurplus ≥ required_monthly`
- `shortfall = max(0, required_monthly − monthlySurplus)`

`monthlySurplus` is the household's trailing-average leftover — the **same input
`handleProjection` already derives** (`networth_handlers.go:344`,
`Assumptions.MonthlySurplus`). Optionally call `networth.Project` with the goal's
required contribution to show the projected net-worth path alongside — but the
on-track decision only needs the arithmetic above. All of this is one file of
decimal math with no model involvement, matching `project.go`'s stated ethos
("the arithmetic is one line per month").

### API — `r.Route("/goals", …)`

Register in `server.go` alongside the other authed routes (pattern:
`server.go:220` budgets block):

```go
r.Route("/goals", func(r chi.Router) {
    r.Use(authMW.Authenticate)
    r.Get("/", s.handleListGoals)        // each goal + derived progress + feasibility
    r.Post("/", s.handleCreateGoal)
    r.Put("/{goalID}", s.handleUpdateGoal)
    r.Delete("/{goalID}", s.handleArchiveGoal)
    r.Post("/parse", s.handleParseGoal)  // AI NL → proposal, 503 if !AI.Enabled()
})
```

`handleListGoals` returns each goal with its derived `current_amount`,
`required_monthly`, `on_track`, `shortfall`, `months_left` — all as
`StringFixed(2)` / ints computed server-side. Amounts arrive as decimal strings
(never floats), exactly like `handleCreateBudget` (`category_handlers.go:170`).

### Coaching producer (doc-04 spine)

A `GoalProducer` implementing `insights.Producer` (shared contracts §1):

- `Kind() string` → `"goal"`.
- `Detect(ctx, q, householdID, now)` → for each active goal, compute feasibility
  deterministically; raise a `Candidate` when behind (shortfall > 0) or when
  newly achieved. `DedupeKey` like `goal:{goalID}:{yyyy-mm}` so a monthly re-run
  refreshes rather than duplicates. `Data` carries the finished strings
  (`target`, `current`, `required_monthly`, `shortfall`, `months_left`);
  `Priority` scales with how far behind. Template `Title`/`Body` for the AI-off
  path; the engine passes `Data` through phrasing when AI is on.

This means goal coaching is delivered by the **existing** insight generation job
and feed — no new job wiring beyond registering the producer where doc 04
registers producers.

## Frontend

- New route `/goals` (add to `NAV` in `AppLayout.tsx:10`; not AI-gated — goals
  work without AI). A Goals list: name, target, target date, a progress bar
  (`current / target`), and a status chip driven by `on_track` ("On track" /
  "$180/mo short"). Data via a new `api.goals()` in `frontend/src/lib/api.ts`
  (mirror `api.budgets`).
- Create form: name, target amount (decimal string field like the budget/alert
  forms), optional target date, optional linked account/category selects
  (populate accounts from `api.accounts`, categories from `api.categories`).
  Save via `api.createGoal(...)` → `POST /api/goals`.
- **NL entry** (shown only when `capabilities.ai_enabled`): a text box + "Parse"
  → `api.parseGoal(text)` → `POST /api/goals/parse`, rendering a
  confirmation card (name/target/date the model extracted) with Confirm / Edit /
  Cancel — **identical UX to doc 09's confirm-before-save**. Confirm calls the
  same `api.createGoal`.
- Goal coaching appears in the doc-04 insight feed automatically (kind `goal`),
  so no separate coaching UI is needed on this route; optionally surface the
  latest `goal` insight inline on the goal card.

## AI notes

- **Two AI touch-points, both narrow:**
  1. **NL parse** (`/goals/parse`): the forced-tool pattern from
     `chat_handlers.go:215` — one tool `propose_goal` with `InputSchema`
     `{name, target_amount (string), target_date (YYYY-MM-DD, nullable), kind}`,
     `ToolChoice` forcing it, parsed by unmarshalling `use.Input`. Inject today's
     date (`chat_handlers.go:136`) so "by December" resolves to a concrete date.
     Never auto-save; validate the amount parses as positive decimal and the
     date is in the future before returning the proposal.
  2. **Coaching phrasing**: handled by the doc-04 engine's existing pass over a
     `Candidate`'s `Data`. The producer supplies finished strings and a template;
     the model only rewords. Same "hand the model finished strings" guarantee as
     `ai/summary.go`.
- The model computes **nothing**: not the required monthly contribution, not
  on-track/behind, not the date arithmetic for months-left. Those are decimal in
  `goals.Feasibility`. The parse step extracts a target and a date from prose;
  even there, the numeric target is re-validated in Go.
- AI-off degradation: NL box hidden; goals created via the form; coaching
  insights emitted with template text.

## Verification

- Migration: `goose up`; confirm `goals` exists with the FK/scope constraints
  (psql `\d goals`).
- CRUD: create a goal via `POST /api/goals` (decimal-string amount); `GET
  /api/goals` returns derived `current_amount`, `required_monthly`, `on_track`.
  Cross-check `required_monthly` by hand: `(target − current) / months_left`.
- Feasibility unit tests (throwaway PG per README): a goal comfortably funded →
  `on_track=true, shortfall=0`; an aggressive goal → correct positive shortfall;
  an open-ended goal (nil date) → no required-monthly, no divide-by-zero.
- NL parse (AI on): `POST /api/goals/parse {"text":"save $10k for a trip by
  December"}` → proposal `{name~"trip", target_amount:"10000.00",
  target_date:"2026-12-…"}`; confirm it does not create a row until the confirm
  call.
- Coaching insight: with a behind-schedule goal, run the doc-04 generation job
  and confirm a `kind='goal'` insight appears in `GET /api/insights` with the
  shortfall in its `data`, and template text when AI is disabled.
- `go build/vet`, `go test -p 1 ./...`; `sqlc generate` produces the new query
  methods. Frontend `tsc --noEmit && npm run build && npm run lint`.

## Out of scope

- Debt-payoff goal kind beyond the schema stub (start with `savings`).
- Auto-moving money / creating transfers toward a goal.
- Multi-currency goals.
- Notifying goal milestones via ntfy (could layer on doc 03's Notifier later;
  coaching lands in the in-app feed here).
- Historical goal-progress charting; progress is computed live, not archived.
