# 04 — Insight engine & proactive feed (THE SPINE)

## Context

This is the user's **#1 must-have**: a *proactive insight feed* — the app
noticing things and telling you, instead of you having to go look. Everything
today is pull: you open Spending, you read a chart. This doc makes the app push
observations to you.

It is also **the spine** every later AI feature plugs into. Subscription
intelligence (`05`), forecast narration (`06`), alert explanation (`07`), budget
suggestions (`08`), and goal coaching (`11`) are all *producers* on this engine.
They never touch storage, jobs, or delivery — they implement one interface and
register. This doc *owns* the insight-engine contract in `00-shared-contracts.md`.

Detection is deterministic SQL. AI is optional garnish — it rephrases a
template into nicer prose, and **the feed works fully with AI disabled**.

## AI vs deterministic split

- **Deterministic (always):** every producer detects candidates with SQL over
  existing report queries. Every number lives in `Candidate.Data` as a
  `StringFixed(2)` string. The engine stores, orders, dedupes, and serves rows.
- **AI (only if `aiClient.Enabled()`):** the engine may pass a candidate's facts
  through a phrasing call (the `buildSummaryInput` pattern) to produce a warmer
  `title`/`body`. The model **never computes** — it narrates the strings it is
  handed. With AI off, the producer's template text is used verbatim.

## Prerequisites

None hard to *build* the engine. Consumers:
- `03-notifications-ntfy-delivery.md` — high-priority insights enqueue a
  `NotifyArgs` push (optional; degrade gracefully if `03` not yet merged).
- `02` preferences — the push gating (`notify.push_kinds`) reuses `03`'s wiring.

## Data model

`insights` table — **new migration `00011_insights.sql`** (reserved number; see
the table in `README.md`). Goose-annotated like `00008_monthly_summaries.sql`.
Exact shape from the shared contract:

```sql
CREATE TABLE insights (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id  UUID        NOT NULL REFERENCES households (id) ON DELETE CASCADE,
    kind          TEXT        NOT NULL,   -- see producer kinds below
    priority      SMALLINT    NOT NULL DEFAULT 0,  -- higher = more important
    title         TEXT        NOT NULL,
    body          TEXT        NOT NULL,
    data          JSONB       NOT NULL DEFAULT '{}',  -- money as decimal STRINGS
    period        DATE,
    dedupe_key    TEXT        NOT NULL,   -- e.g. 'spending_spike:dining:2026-07'
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    read_at       TIMESTAMPTZ,
    dismissed_at  TIMESTAMPTZ,
    UNIQUE (household_id, dedupe_key)
);
CREATE INDEX insights_feed_idx
    ON insights (household_id, dismissed_at, priority DESC, created_at DESC);
```

The `UNIQUE (household_id, dedupe_key)` is load-bearing: the generation job
**upserts** on it so a re-run refreshes an insight rather than duplicating it.

## Backend

### The `insights` package (`backend/internal/insights/`)

**Producer interface** (contract):

```go
type Candidate struct {
    Kind      string
    Priority  int
    Title     string          // template headline
    Body      string          // template narrative
    Data      map[string]any  // deterministic facts; money as StringFixed(2)
    Period    *time.Time
    DedupeKey string
}

type Producer interface {
    Kind() string
    Detect(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time) ([]Candidate, error)
}
```

A `Registry` (a slice of `Producer`) that the generation job iterates. Later
docs append their producers to it. Keep registration in one place (e.g.
`insights.DefaultProducers()`), so `05`/`06`/`07` add one line.

**The engine** (`engine.go`): `Generate(ctx, q, aiClient, householdID, now)`:
1. For each registered producer, call `Detect` (deterministic).
2. For each candidate: if `aiClient.Enabled()`, build a phrasing input from
   `Candidate.Data` and call the model for a nicer title/body (see AI notes);
   **on any AI error or disabled**, keep the template `Title`/`Body`.
3. Upsert each candidate into `insights` on `dedupe_key` (new `UpsertInsight`
   query). Preserve `read_at`/`dismissed_at` on update? No — a refreshed insight
   whose facts changed should arguably re-surface; keep it simple: `ON CONFLICT
   (household_id, dedupe_key) DO UPDATE SET title, body, data, priority,
   period, created_at = now()` and leave `dismissed_at` as-is so a user's
   dismissal sticks. Document the choice inline.

### Initial deterministic producers

All reuse existing queries in `backend/internal/db/queries/reports.sql` — **no
new detection SQL where an existing query serves**. Money strings via
`decimal.StringFixed(2)`. Each builds its own template `Title`/`Body` from its
numbers and stashes the same numbers in `Data`.

1. **`spending_spike`** — this month's category spend materially exceeds its
   trailing average. Reuse `GetCategoryAverages` (line 121) + `GetSpendingByCategory`
   (line 47); candidate when this-month ≥ ~1.5× average and over a dollar floor.
   `DedupeKey`: `spending_spike:{slug}:{YYYY-MM}`.
2. **`new_recurring`** — a newly-detected recurring merchant. Reuse
   `GetRecurringMerchants` (line 190); candidate for a recently-first-seen
   merchant, `DedupeKey`: `new_recurring:{merchant_key}`. Include
   `average_amount` + a per-month estimate (computed as the recurring endpoint does).
3. **`budget_pace`** — a budget on track to blow this month. Reuse
   `GetBudgetProgress` (line 256): compare `spent` to `budgeted × elapsed
   fraction of month`; candidate when spend exceeds pace by a margin.
   `DedupeKey`: `budget_pace:{category_id}:{YYYY-MM}`. Needs budgets (`01`);
   zero candidates if none.
4. **`low_leftover`** — month's leftover (income − spending) low/negative. Reuse
   `GetSpendingSummary` (line 16). `DedupeKey`: `low_leftover:{YYYY-MM}`;
   priority scales with how negative.

**Visibility scoping:** the reused report queries already scope
`household_id AND (user_id OR is_shared) AND is_active AND NOT
excluded_from_reports AND NOT pending` (copy from `GetSpendingByCategory`).
Insights are **household-scoped** — run detection with the household's shared
visibility (pass a representative user or add a household-wide variant; document
the choice).

### Generation job

Model exactly on `LLMCategoriseAllWorker` / `EvaluateAlertsAllWorker`
(`jobs.go` lines 305–396):

- `GenerateInsightsAllArgs{}` (kind `insights_all`) + `GenerateInsightsAllWorker`
  — lists households via `ListHouseholdIDs` (`categories.sql` line 127) and
  enqueues one per-household job each.
- `GenerateInsightsArgs{HouseholdID}` (kind `insights`) + `GenerateInsightsWorker`
  — holds `*dbgen.Queries` + `*ai.Client`, calls `insights.Generate`. Give it
  `UniqueOpts` (start from `rivertype.UniqueOptsByStateDefault()` +
  `JobStateRetryable`, `ByPeriod: time.Minute`) to collapse bursts, exactly like
  `LLMCategoriseArgs.InsertOpts` (lines 270–279).

**Registration** in `client.go` (`NewWorkerClient`):
- Add an `insightInterval` const near the others (lines 21–65), e.g. hourly.
- Register the per-household worker (needs AI + queries) before client
  construction, the sweep worker (needs the client) after — the same ordering
  dance as the LLM workers (lines 112–116 and 206–212).
- **Gating:** the contract lists this among the AI-gated periodic jobs, but
  detection is deterministic and useful without a key. Prefer **always
  registering** the workers + periodic sweep and gating only the *phrasing*
  inside `Generate` on `aiClient.Enabled()`. Note this deviation inline — literal
  AI-gating would leave the feed empty without a key, defeating the point.
- Also enqueue a per-household generation from `SyncItemWorker` follow-up
  (`jobs.go` lines 96–110) alongside the alert eval, so a fresh sync surfaces
  insights promptly.

### High-priority push (consumer of `03`)

After upserting, for any **newly-created** insight with `priority` above a
threshold, enqueue a `NotifyArgs` (from `03`) for each member whose
`notify.push_kinds` includes that insight's `kind`. Reuse `03`'s notify job and
`ListHouseholdMembers`. Skip cleanly if `03` isn't merged yet.

### Read/dismiss API + client

**Queries** (new `backend/internal/db/queries/insights.sql`, regenerate sqlc):
`ListInsights` (feed order: `dismissed_at IS NULL` for unread/all filter,
`ORDER BY priority DESC, created_at DESC`), `MarkInsightRead`,
`DismissInsight`, `UpsertInsight`.

**Handlers** (new `backend/internal/api/insights_handlers.go`, shaped like the
alerts handlers): identity-scoped by `household_id`.
- `GET /api/insights?state=unread|all` → list.
- `POST /api/insights/{id}/read`, `POST /api/insights/{id}/dismiss`.

**Routes** in `server.go` — add an `/insights` group behind
`authMW.Authenticate`, alongside `/alerts` (lines 278–288).

**Client** (`frontend/src/lib/api.ts`): an `Insight` interface (`id, kind,
priority, title, body, data, period, created_at, read_at, dismissed_at`), plus
`api.insights({state})`, `api.markInsightRead(id)`, `api.dismissInsight(id)`.
Note the "// --- Insights ---" section header already exists (line 661) though it
currently holds capabilities/recurring — add the real insight methods there.

## Frontend

**Feed card on the Dashboard** (top of `frontend/src/routes/Dashboard.tsx`) — a
new `glass` section rendered above the stat tiles, showing the top few unread
insights (title, body, a priority/kind chip, read + dismiss actions). Follow the
existing card conventions and the `Empty`/`Loading` helpers already in that
file. Gate its *presence* on there being insights, not on AI (deterministic
feed). Invalidate the query on read/dismiss.

**Optional `/insights` route** — a full list page (all unread, plus a "show
dismissed" toggle). Add the route in `App.tsx` and a nav entry in
`AppLayout.tsx`. Because insights exist without AI, this is **not**
AI-gated — do not put it behind `useNavItems`' `ai_enabled` branch.

## AI notes

- Phrasing reuses the `buildSummaryInput` → `ai.Client.Complete` pattern
  (`backend/internal/api/summary_handlers.go` lines 137–186 and
  `backend/internal/ai/summary.go`). Add an `insights` narration function in
  `backend/internal/ai/` (e.g. `PhraseInsight(ctx, in)`), taking the candidate's
  finished strings and returning a `{title, body}`. System prompt: "rephrase the
  provided facts into a short, warm one-to-three-sentence insight; use only the
  given numbers; do not invent." Return `ErrDisabled` when off.
- **Mandatory fallback:** any AI error, timeout, or `Enabled()==false` → keep
  the producer's template `Title`/`Body`. The feed must be fully populated with
  no key configured.
- Numbers in `Data` are authoritative and never regenerated by the model.

## Verification

- New throwaway PG, migrate, confirm `insights` table + `insights_feed_idx`.
- Seed/keep the ~102 sandbox transactions; run the generation job (enqueue
  `GenerateInsightsArgs` for the household, or wait for the sweep). Confirm rows
  appear in psql with sane `kind`/`priority`/`data` and decimal **string**
  amounts in `data`.
- Cross-check a `spending_spike` insight's numbers against
  `GET /api/reports/by-category` and `/api/reports/averages` for the same
  period — they must match exactly.
- Run the job **twice**: confirm no duplicate rows (upsert on `dedupe_key`), and
  a dismissed insight stays dismissed.
- With `AI_API_KEY` unset: feed still populates with template text. With it set:
  titles/bodies read more naturally, numbers unchanged.
- `GET /api/insights?state=unread`, then `POST …/dismiss`, confirm it leaves the
  unread feed.
- `go build/vet/test ./...`; `tsc`/`build`/lint in `frontend/`.

## Out of scope / handed to later docs

- **Additional producers**: `05` (subscription intelligence), `06` (forecast
  narration), `07` (alert explanation), `08` (budget suggestions), `11` (goal
  coaching) each add a `Producer` and register it — no engine changes.
- **Consumers**: `03` provides the push job this doc enqueues into; `10`
  (scheduled digest) reads the feed to compose its summary.
- No per-insight user preferences beyond the `03`/`02` push-kinds gating.
- No ML/anomaly models — detection stays plain SQL thresholds.
