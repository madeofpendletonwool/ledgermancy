# Shared contracts

Three new pieces of infrastructure that multiple feature docs build on. Defined
here once so the individual docs reference a single source of truth rather than
each inventing storage. Where a doc says "see shared contracts," this is it.

These are **proposed** contracts — the implementing agent for the owning
foundation doc may refine names, but should keep the shape so dependents stay
valid. Owning docs: insights → `04`, preferences → `02`, Notifier → `03`.

---

## 1. Insight engine (owned by `04-insight-engine-and-feed.md`)

The spine for every "AI notices something" feature. Deterministic detectors
find facts in SQL; the engine optionally runs the facts through AI for phrasing;
results are stored as rows and served to a feed. Producers (subscription
intelligence, forecast, alert explanation, "what changed") plug in — they never
touch storage or delivery directly.

### `insights` table (migration `00011_insights.sql` — see README reserved numbers)

```sql
CREATE TABLE insights (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id  UUID        NOT NULL REFERENCES households (id) ON DELETE CASCADE,
    kind          TEXT        NOT NULL,   -- 'spending_spike' | 'new_recurring' | 'budget_pace'
                                          -- | 'low_leftover' | 'subscription' | 'forecast'
                                          -- | 'alert_explanation' | 'goal' | ...
    priority      SMALLINT    NOT NULL DEFAULT 0,  -- higher = more important; drives feed order
                                                   -- and the push threshold (see Notifier)
    title         TEXT        NOT NULL,   -- short headline
    body          TEXT        NOT NULL,   -- 1–3 sentence narrative
    -- The deterministic facts the narrative was built from. Money as decimal
    -- STRINGS, never floats. Lets the UI render structured detail and lets the
    -- text be regenerated without recomputing.
    data          JSONB       NOT NULL DEFAULT '{}',
    period        DATE,                   -- month/period the insight is about (nullable)
    -- Stable identity for one logical insight, e.g.
    -- 'spending_spike:dining:2026-07'. The engine upserts on this so a re-run
    -- refreshes rather than duplicates.
    dedupe_key    TEXT        NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    read_at       TIMESTAMPTZ,
    dismissed_at  TIMESTAMPTZ,
    UNIQUE (household_id, dedupe_key)
);
CREATE INDEX insights_feed_idx
    ON insights (household_id, dismissed_at, priority DESC, created_at DESC);
```

### Producer interface (Go, `backend/internal/insights/`)

```go
// Candidate is one insight a producer wants to raise. Title/Body may be a plain
// template; the engine can pass them through AI for nicer phrasing, but the
// numbers in Data are authoritative and never recomputed by the model.
type Candidate struct {
    Kind      string
    Priority  int
    Title     string          // template headline
    Body      string          // template narrative
    Data      map[string]any  // deterministic facts; money as StringFixed(2)
    Period    *time.Time
    DedupeKey string
}

// Producer detects candidates for one household using deterministic SQL only.
type Producer interface {
    Kind() string
    Detect(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time) ([]Candidate, error)
}
```

### Generation job

- A per-household fan-out sweep, modeled on `LLMCategoriseAllWorker` /
  `EvaluateAlertsAllWorker` in `backend/internal/jobs/jobs.go` (list households →
  enqueue one job each; collapse bursts with `UniqueOpts`).
- For each household: run every registered `Producer.Detect` (deterministic),
  then — **only if `aiClient.Enabled()`** — pass each candidate's facts through a
  phrasing call using the `buildSummaryInput` pattern (`summary_handlers.go`);
  otherwise keep the template text. Upsert on `dedupe_key`.
- Registered like the other AI-gated periodic jobs in
  `backend/internal/jobs/client.go` (new interval const, e.g. `insightInterval`).

### Read/dismiss API + client

- `GET /api/insights?state=unread|all` → list (feed order).
- `POST /api/insights/{id}/read`, `POST /api/insights/{id}/dismiss`.
- Frontend `api.ts` methods + types; capabilities-gated the same way the
  assistant nav is (`AppLayout.tsx` `useNavItems`).

### Feed UI

- A Dashboard card (top of `frontend/src/routes/Dashboard.tsx`) showing the top
  few unread insights, plus an optional dedicated `/insights` route for the full
  list. Dismiss/read actions.

---

## 2. Preferences store (owned by `02-settings-and-preferences.md`)

There is **no preferences/settings storage anywhere today**. This adds a small,
extensible one used by notifications, digest, and push-type selection.

### `preferences` table (migration in doc `02`)

```sql
CREATE TABLE preferences (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    scope        TEXT NOT NULL CHECK (scope IN ('user', 'household')),
    user_id      UUID REFERENCES users (id)      ON DELETE CASCADE, -- set iff scope='user'
    household_id UUID REFERENCES households (id)  ON DELETE CASCADE, -- set iff scope='household'
    key          TEXT NOT NULL,     -- dotted namespace, see keys below
    value        JSONB NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (scope, user_id, household_id, key)
);
```
A JSONB key/value store keyed by scope — extensible without a migration per new
setting, matching how `alerts.config` is deliberately JSONB.

### Reserved keys (initial)
- `notify.channel` — `"none" | "ntfy"` (per user)
- `notify.ntfy_topic` — string (per user; the user's private ntfy topic)
- `notify.push_kinds` — array of insight/alert kinds the user wants pushed
- `digest.enabled` — bool (per user)
- `digest.cadence` — `"weekly" | "monthly"` (per user)

### API
- `GET /api/preferences` (resolved for the caller: their user prefs + household
  prefs), `PUT /api/preferences` (upsert one or many). Household-scoped writes
  gated to household members.

---

## 3. Notifier / ntfy delivery (owned by `03-notifications-ntfy-delivery.md`)

External push is **greenfield** — no config, client, or code exists. This adds a
provider-agnostic sender with an ntfy implementation, mirroring how `ai.Client`
is always constructed but reports `Enabled()`.

### Interface (Go, `backend/internal/notify/`)

```go
type Notification struct {
    Title    string
    Body     string
    Priority int      // maps to ntfy 1..5
    Tags     []string // ntfy tags / emoji
    ClickURL string   // deep link back into the app
}

type Notifier interface {
    // Enabled reports whether delivery will be attempted (config present).
    Enabled() bool
    // Send delivers to one user's configured channel. A no-op (nil) when the
    // user has no channel configured, so callers never branch.
    Send(ctx context.Context, userID uuid.UUID, n Notification) error
}
```

### ntfy implementation
- POST to `{NTFY_BASE_URL}/{topic}` with headers `Title`, `Priority`, `Tags`,
  `Click`; topic comes from the user's `notify.ntfy_topic` preference.
- Config block in `backend/internal/config/config.go` alongside `AI`:
  `NTFY_BASE_URL` (default `https://ntfy.sh`), optional `NTFY_TOKEN` (Bearer for
  self-hosted/protected topics). Empty config → `Enabled()` false → sends are
  no-ops.

### Wiring (consumers, in their own docs)
- **Alerts** (`03` wires this): after `alerts.Evaluate` inserts an
  `alert_events` row (currently only `slog.Info`, `backend/internal/jobs/jobs.go`
  ~line 366), enqueue a notify job for members whose `notify.push_kinds`
  includes that alert type.
- **Insights** (`04`): high-`priority` insights trigger a push, same gating.
- **Digest** (`10`): the periodic digest is delivered via `Notifier`.
- Delivery should be its own River job (so a slow/failed push never blocks
  evaluation), with a `notified_at`-style column where dedupe matters.

---

## Conventions all docs assume

- **Money**: `NUMERIC(20,4)` in DB, `shopspring/decimal` in Go, `StringFixed(2)`
  on the wire. Positive = money out (Plaid convention). Never floats.
- **Visibility scoping**: every household query filters
  `household_id = $1 AND (user_id = $2 OR is_shared) AND is_active AND NOT excluded_from_reports AND NOT pending`
  — copy from `GetSpendingByCategory` (`backend/internal/db/queries/reports.sql`).
- **AI gating**: AI features register jobs/nav only when `aiClient.Enabled()`;
  everything degrades to deterministic behavior without a key.
- **sqlc**: write queries in `backend/internal/db/queries/*.sql`, regenerate.
- **Migrations**: use the reserved number for your doc (see the table in
  `README.md`); goose annotated (`-- +goose Up`).
