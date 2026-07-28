# 07 — Alert explanation

## Context

Ledgermancy's alert engine is deterministic and self-contained.
`alerts.Evaluate` (`backend/internal/alerts/alerts.go`) runs four rule types —
`big_spend`, `budget_threshold`, `unusual_merchant`, `low_leftover` — over exact
SQL/decimal figures and records each firing as an `alert_events` row via
`InsertAlertEvent` (query in `backend/internal/db/queries/alerts.sql` ~line 167).
Each event's `payload` JSONB already holds the finished strings the rule fired
on (e.g. big_spend stores `merchant`, `amount`, `date`, `threshold` — all
`StringFixed(2)`). Events are read by `handleListAlertEvents`
(`backend/internal/api/alert_handlers.go` ~line 185) and rendered by
`describeEvent` in `frontend/src/routes/Alerts.tsx` (~line 300), which produces a
terse title + detail like "Large purchase: KFC — $500.00 on 2026-07-20". The
sweep is enqueued by `EvaluateAlertsWorker` / `EvaluateAlertsAllWorker`
(`backend/internal/jobs/jobs.go`), which currently only `slog.Info`s the count.

The gap: a fired alert states *what* happened but not *why it is notable*. "This
$500 KFC charge is unusual — you normally spend about $12 there" is far more
useful than "$500.00 on 2026-07-20". This doc adds an AI-written explanation to
each fired event. **Detection stays 100% deterministic**; the comparison figure
("you normally spend $12") comes from a SQL query, and AI only phrases it.

## AI vs deterministic split

| Concern | Owner |
| --- | --- |
| Whether an alert fires, and on what figures | **SQL/decimal** (`alerts.Evaluate`, unchanged) |
| The comparison baseline ("normally ~$12 at KFC") | **SQL** — new query, below |
| The one-sentence "why this matters" explanation | **AI** — phrasing only |

The model is handed the event payload strings **plus** the SQL-computed baseline
(typical amount, visit count, category average). It never queries, never
averages, never decides significance — it explains an anomaly the deterministic
engine already flagged, using numbers already computed.

## Prerequisites

- **04 — insight engine** (`insights` table + `Producer` interface). The chosen
  design emits an `alert_explanation`-kind insight linked to the event.
- The alert engine and `alert_events` table (already shipped;
  `00001_core_schema.sql` ~lines 288–300). No change to detection.

## Design decision: insight row, not a payload column

Two options were considered:

- **(a) Add an `explanation TEXT` column to `alert_events`** and fill it in an
  AI enrichment step after `InsertAlertEvent`. Simplest to render (the Alerts
  page already loads events), but it bolts an AI-only, nullable field onto a
  table whose whole point is deterministic durability, couples alert delivery to
  model latency, and gives us a second place (besides insights) where "AI noticed
  something" lives.
- **(b) Emit an `insights` row (kind `alert_explanation`)** whose `data` links
  back to the event by id. Consistent with doc 04 — every "AI noticed"
  surface flows through one engine, one feed, one dedupe/dismiss model, one
  push-gating path (Notifier, doc 03). Detection and its record stay untouched;
  the explanation is a separate, optional, regenerable artifact.

**Choose (b).** The tradeoff is that the Alerts page must join events to their
explanation insight (an extra lookup) rather than reading one row, and an
explanation can briefly lag its event until the next insight sweep. Both are
acceptable: explanations are enrichment, not the alert itself, and consistency
with the spine is worth more than a saved join. (If product later wants the
explanation inline and instantaneous, option (a) can be added without undoing
this — the SQL baseline query below is reused either way.)

## Data model

No new tables. One new query and the insight `data` shape.

- **Baseline query** — `GetMerchantSpendBaseline` in
  `backend/internal/db/queries/reports.sql`, using the standard visibility
  scoping (copy the join + WHERE block from `GetRecurringMerchants` /
  `GetSpendingByCategory`):

  ```sql
  -- name: GetMerchantSpendBaseline :one
  -- Typical spend at one merchant for this household, EXCLUDING the flagged
  -- transaction, so "you normally spend ~$X" is a real prior, not skewed by the
  -- charge that triggered the alert. All arithmetic stays here.
  SELECT
      COALESCE(AVG(t.amount), 0)::numeric AS typical_amount,
      COUNT(*)::bigint                    AS visit_count,
      COALESCE(MIN(t.amount), 0)::numeric AS min_amount,
      COALESCE(MAX(t.amount), 0)::numeric AS max_amount
  FROM transactions t
  JOIN accounts a    ON a.id = t.account_id
  JOIN plaid_items i ON i.id = a.plaid_item_id
  JOIN users u       ON u.id = i.user_id
  WHERE u.household_id = $1
    AND (i.user_id = $2 OR i.is_shared)
    AND a.is_active AND NOT t.excluded_from_reports AND NOT t.pending
    AND t.merchant_key = @merchant_key::text
    AND t.id <> @exclude_tx::uuid
    AND t.amount > 0;
  ```

- **`insights.data` shape** for an `alert_explanation` candidate (strings):

  ```json
  {
    "alert_event_id": "…uuid…",
    "alert_type": "unusual_merchant",
    "merchant": "KFC",
    "amount": "500.00",
    "typical_amount": "12.40",
    "visit_count": "17",
    "date": "2026-07-20"
  }
  ```

  `alert_event_id` is the link back; the Alerts UI matches on it.

## Backend

- **Producer** — `backend/internal/insights/alertexplanation.go`, kind
  `alert_explanation`, on the doc-04 sweep:
  - `Detect` lists **recent, still-unexplained** alert events. Add a query
    `ListRecentAlertEventsForExplanation` (or reuse `ListAlertEvents` shape) that
    returns event id, `alert_type`, and `payload`. Skip events already having an
    `alert_explanation` insight (the `dedupe_key` `"alert_explanation:" + event_id`
    makes the upsert idempotent, so a naive re-run is safe regardless).
  - For transaction-linked types (`big_spend`, `unusual_merchant`) read
    `merchant_key`/`amount`/`date` from the payload and call
    `GetMerchantSpendBaseline` (excluding the event's own transaction id) to get
    the prior. For aggregate types (`budget_threshold`, `low_leftover`) the
    payload already carries budgeted/spent or income/leftover — no extra query;
    the explanation contrasts those existing strings.
  - Emit one `Candidate` per event: `Data` per the shape above, `Title`/`Body`
    template text, `Priority` mirroring the alert's importance. `Period` = event
    date's month.
- **Reuse, do not re-detect**: this producer never re-runs alert logic. It reads
  `alert_events` and adds context. `alerts.Evaluate` and `InsertAlertEvent` are
  untouched.
- **`ai.ExplainAlert(ctx, AlertExplanationInput) (string, error)`** —
  `backend/internal/ai/alertexplain.go`, modeled on `MonthlySummary`
  (`backend/internal/ai/summary.go`): `ErrDisabled` when `!Enabled()`, strict
  prompt, small `MaxTokens`. Called in the doc-04 phrasing pass, not the
  producer.
- **sqlc**: add queries, regenerate from `backend/`
  (`go run github.com/sqlc-dev/sqlc/cmd/sqlc@latest generate`).

## Frontend

- **Alerts page** (`frontend/src/routes/Alerts.tsx`, `RecentEvents` /
  `describeEvent` ~line 300): after the existing deterministic `title`/`detail`,
  render the matching `alert_explanation` insight's `body` as a subtle
  second line (e.g. muted italic under `detail`). Match by `alert_event_id`
  against the insights the feed already fetches — no bespoke per-event endpoint.
  `describeEvent` stays the deterministic source of the headline; the AI line is
  additive.
- `alert_explanation` insights also appear in the doc-04 feed/`/insights` route
  for free.
- **Capabilities gating**: the explanation line only renders when `ai_enabled`
  (`handleCapabilities`). Every alert event still shows its deterministic
  title/detail with AI off — the page is unchanged from today.

## AI notes

- System prompt: "You explain why a spending alert is worth noticing, for a
  household budgeting app. Use only the figures provided; quote them exactly. One
  or two plain sentences. Contrast the flagged amount with the typical amount.
  Do not give financial advice or invent numbers." Mirrors the strict, no-invent
  rules of `summarySystemPrompt` and `categoriseSystemPrompt`.
- Input is decimal strings only: flagged amount, typical amount, visit count,
  merchant, date (or budgeted/spent for aggregate alerts). The comparison
  ("normally ~$12") is the **SQL** `typical_amount` — the model quotes it, never
  computes it.
- Template `Body` (used when AI off) states the same contrast plainly from the
  same strings, so the feature degrades cleanly.

## Verification

- `docker compose up -d --build`. Configure an `unusual_merchant` (and a
  `big_spend`) alert; the sandbox's ~102 transactions should let one fire, or
  insert a single large charge at a merchant that has cheap history.
- Confirm the alert fires (an `alert_events` row) exactly as before — detection
  unchanged. Then run the doc-04 insight sweep and `GET /api/insights` → expect
  an `alert_explanation` row whose `data.alert_event_id` matches the event and
  whose `typical_amount` equals `GetMerchantSpendBaseline` run by hand in psql
  (excluding the flagged tx).
- Verify idempotency: two sweeps produce one explanation per event (dedupe_key).
- With AI enabled, the Alerts page shows the extra sentence with correctly quoted
  figures; with the key unset, only the deterministic title/detail and the
  template body, no 5xx anywhere.
- Backend `go build/vet/test -p 1 ./...` (throwaway PG per README); frontend
  `tsc/build/lint`.

## Out of scope

- Changing alert detection, thresholds, or adding alert types (deterministic
  engine is frozen here).
- Adding an `explanation` column to `alert_events` (rejected option (a); revisit
  only if inline/instant explanations become a hard requirement).
- Pushing explanations to ntfy — delivery/gating is doc 03 (Notifier) applied to
  high-priority insights.
- LLM-driven "is this really unusual?" judgment — significance is decided by the
  deterministic rule, never the model.
