# 05 — Subscription intelligence

## Context

Ledgermancy already detects recurring merchants deterministically:
`GetRecurringMerchants` (`backend/internal/db/queries/reports.sql` ~line 189)
finds any merchant with **≥3 charges** over a 12-month window whose gaps sit in
the 6–40 day band and are regular (`gap_stddev <= avg_gap * 0.5`). It is served
by `handleRecurring` (`backend/internal/api/report_handlers.go` ~line 304),
exposed to chat as the `recurring_charges` tool
(`backend/internal/api/chat_handlers.go` ~line 384), and rendered by
`RecurringSection` in `frontend/src/routes/Spending.tsx` (~line 321).

What the app does **not** do: tell the user *what kind* of subscription each one
is, notice when a subscription's price has crept up, or flag the small "zombie"
charges that are easy to forget. This doc adds those three things on top of the
existing detector, surfaced both in the Spending recurring table and as
`subscription`-kind rows in the insight feed (doc 04).

Note: transactions carry a Plaid `is_recurring` boolean
(`00001_core_schema.sql` line 178, `IsRecurring` in `dbgen/models.go`) that is
**currently unused** by any query or report. It is a weak, provider-supplied
signal; this doc keeps the SQL cadence detector as the source of truth and
treats `is_recurring` only as an optional corroborating hint (see Out of scope).

## AI vs deterministic split

| Concern | Owner |
| --- | --- |
| Which merchants recur, cadence, typical amount | **SQL** (`GetRecurringMerchants`, unchanged) |
| Whether an amount has risen over time (price creep) | **SQL** (new query, below) — a detected delta |
| Whether a charge is a "zombie" (small + regular + active) | **SQL** (threshold on existing columns) |
| Category label (streaming / utility / insurance / gym / …) | **AI** — messy classification of a merchant name |
| The one-line explanation of *why* a creep/zombie matters | **AI** — phrasing only |

The model is handed finished `StringFixed(2)` strings for every amount and the
already-computed delta. It never subtracts old price from new, never averages,
never decides *whether* something crept — only labels and phrases.

## Prerequisites

- **04 — insight engine** (`insights` table + `Producer` interface). This doc is
  a producer, kind `subscription`. Do not add new storage.
- The existing recurring detector (already shipped). No schema change to
  `transactions`.

## Data model

No new tables. Two additions:

1. **New query for price creep** — `GetRecurringAmountTrend` in
   `backend/internal/db/queries/reports.sql`, next to `GetRecurringMerchants`.
   Reuse the identical CTE (`tx`, same visibility scoping and spend filters), but
   split each merchant's charges into an **early half** and a **recent half** by
   date and return the average of each plus the delta — all in SQL:

   ```sql
   -- name: GetRecurringAmountTrend :many
   -- For merchants that already qualify as recurring, compare the average of
   -- the older charges against the average of the newer charges. The split and
   -- the difference are computed here; the caller only formats and explains.
   WITH tx AS ( /* identical to GetRecurringMerchants: same joins + WHERE */ ),
   ranked AS (
       SELECT merchant_key, merchant, amount, date,
              NTILE(2) OVER (PARTITION BY merchant_key ORDER BY date) AS half
       FROM tx
   )
   SELECT
       merchant_key,
       COALESCE(MAX(merchant), '')::text                                   AS merchant,
       AVG(amount) FILTER (WHERE half = 1)::numeric                        AS early_avg,
       AVG(amount) FILTER (WHERE half = 2)::numeric                        AS recent_avg,
       (AVG(amount) FILTER (WHERE half = 2)
        - AVG(amount) FILTER (WHERE half = 1))::numeric                    AS delta
   FROM ranked
   GROUP BY merchant_key
   HAVING COUNT(*) >= 4                       -- need two charges per half
      AND AVG(amount) FILTER (WHERE half = 1) > 0
      AND (AVG(amount) FILTER (WHERE half = 2)
           - AVG(amount) FILTER (WHERE half = 1))
          >= AVG(amount) FILTER (WHERE half = 1) * 0.10;  -- ≥10% rise
   ```

   The 10% floor keeps rounding noise out. Tune in review; the point is the
   detection lives in SQL.

2. **`insights.data` shape** for a `subscription` candidate (money as strings):

   ```json
   {
     "merchant": "Netflix",
     "merchant_key": "netflix",
     "cadence": "monthly",
     "typical_amount": "15.49",
     "monthly_estimate": "15.49",
     "category": "streaming",
     "flavor": "price_creep",           // "" | "price_creep" | "zombie"
     "early_amount": "11.99",           // present for price_creep
     "recent_amount": "15.49",
     "delta": "3.50"
   }
   ```

## Backend

- **Reuse the monthly-estimate math** already in `handleRecurring`: the
  `daysPerMonth` var and `amount.Mul(daysPerMonth).Div(avgGap)` normalisation,
  plus `cadenceLabel(avgGap)` (`report_handlers.go`). Factor these into a small
  helper if the producer and handler both want them; do not re-derive.
- **Producer** — `backend/internal/insights/subscription.go`, implementing the
  doc-04 `Producer` interface:
  - `Kind() string` → `"subscription"`.
  - `Detect(ctx, q, householdID, now)`:
    1. Call `GetRecurringMerchants` (12-month lookback, same params
       `handleRecurring` uses: `HouseholdID`, `UserID`, `Date: now.AddDate(0,-12,0)`).
    2. Call `GetRecurringAmountTrend` for the creep set.
    3. Build one `Candidate` per merchant that is a **zombie** (small monthly
       estimate — e.g. `monthly_estimate <= $15` and cadence regular — a decimal
       comparison in Go) or that appears in the trend result (**price creep**).
       Plain merchants that are neither do not need a feed row; they already show
       in the Spending table.
    4. `DedupeKey` = `"subscription:" + merchant_key` so a re-run refreshes the
       same row. `Priority`: price creep > zombie (creep costs money now).
    5. `Title`/`Body` are template text ("Netflix went from $11.99 to $15.49");
       `Data` holds the authoritative strings. `category` starts empty.
  - **Classification** is deferred to the engine's phrasing pass — see AI notes.
    A producer only does SQL, per the doc-04 contract.
- **Registration**: the doc-04 generation job runs every registered producer.
  Add `&insights.SubscriptionProducer{}` to the producer list the insight worker
  builds. No new River worker or interval — it rides the `insightInterval` sweep
  from doc 04 (modeled on `LLMCategoriseAllWorker` / `EvaluateAlertsAllWorker` in
  `backend/internal/jobs/jobs.go`).
- **sqlc**: add the query, then regenerate from `backend/`:
  `go run github.com/sqlc-dev/sqlc/cmd/sqlc@latest generate`.

## Frontend

- **Enrich `RecurringSection`** (`frontend/src/routes/Spending.tsx`): add a
  **Type** column (the AI category from the matching `subscription` insight) and
  a small "price up" badge on rows whose merchant appears in a creep insight.
  Join by `merchant` / `merchant_key` client-side against the insights the feed
  already fetches — do not add a second bespoke endpoint. The table itself stays
  deterministic; the badge/label degrade to blank when AI is off.
- The feed card and `/insights` route (doc 04) render `subscription` insights
  with no extra work; ensure the `data` fields above render (typical amount, and
  for creep the early→recent line).
- **Capabilities gating**: category labels and creep explanations only appear
  when `ai_enabled` (the `handleCapabilities` flag); the recurring table and
  monthly estimates render regardless.

## AI notes

- Classification and explanation happen in the **doc-04 phrasing pass**, using
  the `buildSummaryInput` → `ai.Client` pattern
  (`backend/internal/api/summary_handlers.go`, `backend/internal/ai/summary.go`).
- Add an `ai` method, e.g. `ClassifySubscriptions(ctx, []SubscriptionInput)`,
  mirroring `CategoriseMerchants` (`backend/internal/ai/categorize.go`): a strict
  system prompt that must return one label from a **fixed set**
  (`streaming | music | utility | insurance | phone | gym | software | news | other`)
  per merchant, given only the merchant name and cadence. Reject any label not in
  the set (as `CategoriseMerchants` drops invalid slugs).
- The explanation prompt is handed the **finished** delta string
  ("was $11.99, now $15.49, up $3.50") and asked for one warm sentence. It must
  quote the numbers as given and invent none — same rule as `summarySystemPrompt`.
- When `!aiClient.Enabled()`, the producer's template `Title`/`Body` stand and
  `category` stays empty; nothing breaks.

## Verification

- Seed: the sandbox stack already has ~102 transactions; confirm at least one
  merchant has ≥3 regular charges (`docker compose up -d --build`). If needed,
  insert a few synthetic recurring charges with a rising amount for one merchant.
- Cross-check the detector in psql: run `GetRecurringMerchants` and
  `GetRecurringAmountTrend` by hand and confirm the creep merchant's `delta`
  matches `recent_avg - early_avg` exactly.
- Drive the insight sweep (enqueue the doc-04 job) and `GET /api/insights` →
  expect `subscription` rows with correct `data` strings; confirm no duplicate
  rows on a second run (dedupe_key).
- With AI enabled, verify `category` is one of the allowed labels and the body
  quotes the exact strings; with `ANTHROPIC_API_KEY`/`AI_*` unset, verify
  template text and blank category.
- Backend: `go build ./... && go vet ./... && go test -p 1 ./...` (throwaway PG
  per README). Frontend: `tsc`, `build`, `lint`.

## Out of scope

- Cancelling or managing subscriptions (no write-back to any provider).
- Using Plaid's `/transactions/recurring` streams endpoint; we keep our own
  cadence detector. Wiring `is_recurring` into detection can be a later refinement
  — treat it as a hint that raises confidence, never as the sole trigger.
- Duplicate-service detection ("you pay for both Hulu and Netflix") — a possible
  future producer, not this one.
- Push delivery of subscription insights (owned by doc 03 / the Notifier gating).
