# 03 — External push via ntfy

## Context

Ledgermancy can **detect** things worth telling a user about — the alert engine
raises `alert_events` rows on every sync and sweep — but it cannot **reach** the
user. There is no SMTP, no push, no ntfy, nothing: a raised alert only does a
`slog.Info` and writes a DB row (`backend/internal/jobs/jobs.go` line ~365–366).
The in-app `NotificationBell` (`AppLayout.tsx`) polls the unread count, and
that's the entire notification story.

This doc adds the first external delivery path: **ntfy** (self-hostable pub/sub
push, one HTTP POST per message). It *owns* the `Notifier` contract in
`00-shared-contracts.md`. It delivers **alerts** now, and exposes the plumbing
that `04` (high-priority insights) and `10` (digest) will reuse.

## AI vs deterministic split

No AI. Delivery is mechanical: a formatted, already-computed message goes out
over HTTP.

## Prerequisites

- `02-settings-and-preferences.md` — reads per-user `notify.channel`,
  `notify.ntfy_topic`, `notify.push_kinds` from the preferences store. If `02`
  is not yet merged, the `GetUserPreference` query and the reserved keys are
  the integration point; do not duplicate the store here.

## Data model

**Delivery tracking** — a push must not be sent twice for the same event. Add a
nullable `notified_at TIMESTAMPTZ` column to `alert_events` (new migration;
claim the next free number, e.g. `00010_notifications.sql`, goose-annotated like
`00008_monthly_summaries.sql`). The notify job stamps it after a successful send
and skips rows already stamped, so a re-run or overlapping sweep never
double-pushes. (For insights, `04` uses its own `read_at`/priority; the digest
in `10` dedupes on its cadence window.)

No table is needed for the notifier itself — it is stateless config + per-user
preference lookups.

## Backend

### The `notify` package (`backend/internal/notify/`)

Mirror the shape of `backend/internal/ai/client.go`: always constructed, reports
`Enabled()`, no-ops cleanly when unconfigured so callers never branch.

```go
type Notification struct {
    Title    string
    Body     string
    Priority int      // maps to ntfy 1..5
    Tags     []string // ntfy tags / emoji
    ClickURL string   // deep link back into the app
}

type Notifier interface {
    Enabled() bool
    Send(ctx context.Context, userID uuid.UUID, n Notification) error
}
```

**ntfy implementation** (`ntfy.go`):
- Constructed from config (below) + a `*dbgen.Queries` (to resolve the user's
  topic and channel from preferences).
- `Send` resolves the user's `notify.channel`; if not `"ntfy"` or no topic set,
  it is a **no-op returning nil** — callers always call, gating lives here.
- POST to `{NTFY_BASE_URL}/{topic}` with headers `Title`, `Priority` (1–5),
  `Tags` (comma-joined), `Click` ({ClickURL}); body is `n.Body`. Add
  `Authorization: Bearer {NTFY_TOKEN}` when a token is configured.
- Standard `http.Client` with a short timeout; return the error so the River job
  can retry.
- `Enabled()` reports whether `NTFY_BASE_URL` resolves and delivery will be
  attempted at all (base URL is always defaulted, so `Enabled()` is effectively
  "constructed"; per-user gating is the real switch inside `Send`).

### Config (`backend/internal/config/config.go`)

Add an `NTFYConfig` block alongside `AI` (the struct at lines 52–59 and its load
at lines 84–88 are the template):

```go
type NTFYConfig struct {
    BaseURL string // NTFY_BASE_URL, default "https://ntfy.sh"
    Token   string // NTFY_TOKEN, optional Bearer for protected/self-hosted
}
func (n NTFYConfig) Enabled() bool { return n.BaseURL != "" }
```

Load with `env("NTFY_BASE_URL", "https://ntfy.sh")` and
`os.Getenv("NTFY_TOKEN")`, and add `NTFY NTFYConfig` to `Config`. Empty/default
config still yields a usable no-op notifier (per-user preference is the gate).

### Its OWN River job

**A slow or failing push must never block alert evaluation.** So delivery is a
separate job, not inline in `EvaluateAlertsWorker`. Follow the job/worker/
registration pattern in `backend/internal/jobs/jobs.go` + `client.go`:

1. **Args + Worker** in `jobs.go`, modeled on `EvaluateAlertsArgs`/`Worker`
   (lines 331–369):
   ```go
   type NotifyArgs struct {
       UserID  uuid.UUID `json:"user_id"`
       // enough to build the Notification without re-querying, e.g. a kind +
       // pre-formatted title/body/priority/click, or an alert_event_id to load.
   }
   func (NotifyArgs) Kind() string { return "notify" }
   ```
   Give it `UniqueOpts` if you key on an event id, to collapse duplicates. The
   worker holds the `notify.Notifier` + `*dbgen.Queries`; `Work` calls
   `Send`, and on success stamps `notified_at` (for alert-driven pushes).
2. **Register** the worker in `NewWorkerClient` (`client.go`). The notifier is
   always constructed (like `SnapshotNetWorthWorker`, lines 88–90) — it is not
   AI-gated. Thread the `Notifier` into `NewWorkerClient`'s signature (currently
   `(pool, syncer, aiClient)`) and construct it in the caller
   (`cmd/…`/wherever `NewWorkerClient` is called) from `cfg.NTFY` + `queries`.
   No periodic job is needed — notify jobs are enqueued by consumers, not on a
   timer (the digest in `10` is the one timer-driven consumer).
3. The **API server** also needs the notifier if any handler ever sends
   directly; for now enqueuing suffices. If a handler must enqueue, it already
   has `s.Jobs` (`Server` struct, `server.go` line 40).

### Consumer wiring — alerts

In the `EvaluateAlertsWorker` path (`jobs.go` ~line 360–368): after
`alerts.Evaluate` inserts `alert_events` rows, enqueue one `NotifyArgs` per
**household member whose `notify.push_kinds` includes that alert's type**.
Resolve members with the existing `ListHouseholdMembers`
(`backend/internal/db/queries/households.sql` line 12). Because `Evaluate`
currently only returns a count, either (a) have it also return the raised events
(preferred — small change to `backend/internal/alerts/alerts.go`), or (b) add a
query that selects `alert_events` with `notified_at IS NULL` for the household
and enqueue from those. Enqueuing (not sending) keeps evaluation fast; the
notify job does the slow HTTP.

The push threshold / kind matching reads `notify.push_kinds` per user via the
`02` preferences query. A user with `channel='none'` or the kind not in their
list gets nothing — enforced in `Send`'s no-op path and/or before enqueue.

### Capability flag

Extend the `Capabilities` payload (`handleCapabilities`,
`summary_handlers.go` line 22) with `notify_enabled: cfg.NTFY.Enabled()` so the
`02` Settings UI can show/hide the ntfy controls (its `useNavItems`-style gate).

## Frontend

Minimal. The channel/topic/push-kinds inputs already live in the `02` Settings
UI. This doc only:
- Adds `notify_enabled` to the `Capabilities` interface in
  `frontend/src/lib/api.ts` and lets Settings surface a "notifications
  unavailable — no ntfy server configured" hint when false.
- Optionally, a "send test notification" button in Settings hitting a tiny
  `POST /api/preferences/notify-test` (enqueues a `NotifyArgs` to the caller) so
  a user can confirm their topic works. Nice-to-have, not required.

## AI notes

None.

## Verification

- Run a local ntfy (`docker run -p 8888:80 binwiederhier/ntfy serve`) or use
  `https://ntfy.sh` with a random topic; set `NTFY_BASE_URL` accordingly.
- Set a test user's `notify.channel='ntfy'`, `notify.ntfy_topic='<topic>'`,
  `notify.push_kinds=['big_spend']` via `02`'s `PUT /api/preferences`.
- Trigger an alert (seed a large transaction / drive an evaluation), subscribe
  to the topic (`curl -s {base}/{topic}/json`), confirm one push arrives with
  the right Title/Priority/Click.
- Confirm `alert_events.notified_at` is stamped and a second evaluation sweep
  does **not** re-push.
- Confirm a slow/unreachable ntfy causes the notify job to retry/fail **without**
  affecting alert evaluation (evaluation still completes, events still stored).
- `go build/vet/test ./...` in `backend/`; `tsc`/`build`/lint in `frontend/`.

## Out of scope

- Insight pushes — `04` enqueues `NotifyArgs` for high-priority insights reusing
  this exact job.
- The scheduled digest — `10`.
- Email/SMS/web-push channels — the `Notifier` interface leaves room; only ntfy
  is implemented here.
- Rich per-household notification policy beyond the per-user reserved keys.
