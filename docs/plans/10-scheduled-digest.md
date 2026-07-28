# 10 — Scheduled digest

## Context

Everything the app knows is pull-only: the user has to open it to see the
monthly summary or the insight feed. This feature pushes a periodic **digest** —
the AI monthly-summary narrative plus the top few unread insights — to users who
have opted in, delivered through the ntfy `Notifier`, gated by each user's
digest preference and cadence. It is the first *scheduled, outbound* AI surface.

Crucially, it **reuses** existing generation. The summary narrative comes from
the same `MonthlySummary` stack the on-demand button uses; the insights come
from the feed built in doc 04. The digest job assembles and delivers — it does
not recompute any figure. One useful side effect: there is currently **no
background job that generates monthly summaries** (they're produced only when a
user clicks "Generate"), so the digest job doubles as a cache-warmer for the
`monthly_summaries` table.

## AI vs deterministic split

| Concern | Owner |
| --- | --- |
| The month's figures | SQL (`buildSummaryInput`) — unchanged |
| The summary narrative | Existing `MonthlySummary` (AI phrasing over finished strings) |
| Which insights to include | Deterministic: top-N unread by priority (doc 04 feed order) |
| Whether/when to send (opt-in, cadence, dedupe) | Deterministic (preferences + a `notified_at`-style guard) |
| Delivery | `Notifier` (doc 03) — deterministic HTTP |

No new prompt. The AI involvement is exactly the existing summary call, run on a
schedule instead of on click. If AI is disabled, the digest still sends the
deterministic parts (top insights render from their template `Title`/`Body`,
which the doc-04 engine already stores) and simply omits the narrative.

## Prerequisites

- **Doc 02 — preferences**: `digest.enabled` (bool, per user), `digest.cadence`
  (`"weekly" | "monthly"`, per user), and the `notify.*` keys that route
  delivery. See shared contracts §2.
- **Doc 03 — Notifier / ntfy delivery**: `notify.Notifier` interface and the
  delivery River job. The digest sends through the same job so a slow push never
  blocks the sweep. See shared contracts §3.
- **Doc 04 — insight engine**: the `insights` table and a query for top unread
  insights per household/user.

## Data model

No new table required, but add one **dedupe guard** so a re-run (or a second
worker) can't double-send. Two viable shapes — pick the simpler for the codebase:

- **Preferred**: a `digest_deliveries` table keyed by `(user_id, period_key)`
  with `sent_at`, where `period_key` is the ISO week (`2026-W30`) or month
  (`2026-07`) the digest covers. Insert-on-send; the sweep skips a user who
  already has a row for the current `period_key`. Mirrors the
  `AlertEventExistsForPeriod` idempotency pattern (`alerts.go:259`).
- Alternatively store `digest.last_sent_period` back into `preferences` — fewer
  moving parts, but conflates config with delivery state; the table is cleaner.

Migration number: this depends on doc 02's preferences migration and doc 04's
insights migration having landed. Take the **next free number after those**
(insights is `00009` per shared contracts; preferences and Notifier take
subsequent numbers) — state the dependency explicitly and don't hard-code a
number that collides. If a dedupe table is added, it is the digest's own small
migration.

## Backend

### Reuse (concrete paths)

- Summary generation: `buildSummaryInput` (`summary_handlers.go:137`) and
  `ai.Client.MonthlySummary` (`ai/summary.go:39`). Both currently hang off an
  `*http.Request`; **factor `buildSummaryInput` into a request-free helper**
  taking `(ctx, q, identity/householdID, from, to, label)` so the job can call
  it. The HTTP handler then wraps the same helper. This is the one refactor
  this doc requires; keep behaviour identical.
- Summary cache: `GetMonthlySummary` / `UpsertMonthlySummary`
  (`summary_handlers.go:64,114`). The job checks the cache first; on a miss it
  generates and upserts (the cache-warming behaviour).
- Top unread insights: the doc-04 query (feed order:
  `priority DESC, created_at DESC`, filtered `dismissed_at IS NULL, read_at IS
  NULL`).
- Delivery: `Notifier.Send(ctx, userID, notify.Notification{…})` (shared
  contracts §3) via doc 03's delivery job.
- Job fan-out pattern: `EvaluateAlertsAllWorker` / `LLMCategoriseAllWorker`
  (`jobs.go:312,379`) — list households (or users), enqueue one job each,
  collapse bursts with `UniqueOpts`. Periodic registration: `client.go:130`
  `PeriodicJobs` + a new interval const.

### Jobs

Two new job types in `backend/internal/jobs/jobs.go`, following the
All → per-entity split:

- `DigestSweepArgs{}` (`kind: "digest_sweep"`) — the periodic entry point.
  `DigestSweepWorker` lists users who have `digest.enabled = true` and whose
  cadence says they are due *now* (see cadence gating), and enqueues a
  `DigestArgs{UserID}` for each. Because opt-in is **per user**, enumerate users,
  not households — resolve each user's household for the figures.
- `DigestArgs{UserID}` (`kind: "digest"`) — `DigestWorker`:
  1. Load the user's prefs; re-check `digest.enabled` and the dedupe guard for
     the current `period_key` (skip if already sent).
  2. Resolve the reporting window from cadence: monthly → the just-completed
     calendar month (reuse `monthPeriod`, `summary_handlers.go:37`); weekly →
     the trailing 7 days (or the summary for the current month-to-date — decide
     and document; monthly figures over a weekly cadence is the pragmatic choice
     since `MonthlySummary` is month-shaped).
  3. Get-or-generate the summary via the cache (warms it on miss). If AI is
     disabled, `MonthlySummary` returns `ErrDisabled` → omit the narrative.
  4. Fetch top-N unread insights for the household.
  5. Build a `notify.Notification` (title e.g. "Your July recap", body = the
     narrative + a short list of insight titles, `ClickURL` deep-linking to the
     dashboard/insights route) and enqueue doc 03's delivery job.
  6. Record the dedupe row.

Registration (`client.go`): a new `digestSweepInterval`. Run frequently (e.g.
hourly) but let the **cadence gating inside the sweep** decide who is actually
due — the same way `SyncAllWorker` runs hourly but acts only on due items. The
sweep is cheap when nobody is due. Register the sweep unconditionally
(deterministic parts work without AI); the summary call self-gates on
`Enabled()`.

### Cadence gating

Deterministic, in the sweep: weekly users are due on a fixed weekday (e.g.
Monday) covering the prior week; monthly users are due on day 1 (or 2, to let
the last day's transactions settle) covering the prior month. The
`digest_deliveries` `period_key` makes "already sent this week/month" a single
existence check, so exact sweep timing doesn't cause duplicates.

## Frontend

- Digest opt-in lives in the doc-02 Settings page: toggle for `digest.enabled`
  and a cadence select for `digest.cadence`, written through `PUT
  /api/preferences`. No new route.
- The digest is outbound only — there is no in-app "digest" view to build; its
  contents are the summary (already on the dashboard) and insights (doc 04
  feed). The `ClickURL` should land on the insight feed.
- Delivery requires a `notify.channel`/`notify.ntfy_topic` (doc 02/03); Settings
  should note that a digest needs a notification channel configured, and the
  sweep skips users whose `Notifier.Send` is a no-op for lack of a channel.

## AI notes

- **No new prompt and no new model call shape.** The only AI is the existing
  `MonthlySummary`, invoked from the job instead of the handler. This preserves
  the "hand the model finished strings" guarantee (`ai/summary.go:18`) verbatim
  — the job assembles the same `MonthlySummaryInput`.
- The digest body **quotes stored narrative and stored insight text**; the job
  does no arithmetic and passes no numbers to the model beyond what
  `buildSummaryInput` already formats. Insight bodies were already
  AI-or-template phrased at generation time (doc 04) and are reused as-is.
- Cache-first means a user who already generated their summary in-app gets that
  exact text; the job never regenerates over an existing cached month.

## Verification

- `docker compose up -d --build`. Set a test user's prefs: `digest.enabled=true`,
  `digest.cadence=weekly`, a `notify.ntfy_topic`.
- Warm path: clear any `monthly_summaries` row for the target month; enqueue a
  `DigestArgs{UserID}` manually (or trigger the sweep) and confirm (a) a
  `monthly_summaries` row now exists (cache warmed), (b) an ntfy request was
  sent (point `NTFY_BASE_URL` at a local sink or ntfy.sh test topic), (c) a
  `digest_deliveries` row exists.
- Dedupe: re-run the sweep; confirm no second send for the same `period_key`.
- Opt-out: set `digest.enabled=false`; confirm the sweep skips the user.
- AI-off: unset the AI key; confirm the digest still sends with insights and no
  narrative (no 500).
- Cadence: a monthly user is not sent mid-month; a weekly user is sent on the
  chosen weekday only.
- `go build/vet`, `go test -p 1 ./...` (throwaway PG). A unit test on the cadence
  "is due" predicate and the dedupe guard is worthwhile. Frontend
  `tsc/build/lint` for the Settings toggles.

## Out of scope

- New digest content beyond summary + top insights (charts, images, per-category
  breakdowns).
- Email/SMS channels — delivery is whatever `Notifier` supports (ntfy).
- Per-household (shared) digests — opt-in is per user.
- User-chosen send time/timezone beyond weekly/monthly cadence.
- A separate digest-history archive view in the app.
