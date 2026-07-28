# 13 — Bill calendar + cash-flow forecast

*(TODO.md "Next major initiatives" #1.)*

## Context

The app looks backward well and forward poorly. Recurring charges are detected
(`GetRecurringMerchants` in `backend/internal/db/queries/reports.sql:~200`,
consumed by `insights/subscription.go` and the Spending "Recurring" table), but
that detection is **derived on every read and never persisted**. Nothing in the
app knows what is *due next week*, no account has a projected balance, and the
budget's "safe to spend" figure is blind to the $1,200 mortgage that has not
landed yet.

Two consequences:

- A surprise autopay is the most common personal-finance failure mode, and this
  app holds every input needed to prevent it and does not.
- Safe-to-spend is currently *optimistic in the first half of the month*. It
  subtracts trailing-typical fixed costs (`reporting/safetospend.go:76-90`),
  which is a decent monthly estimate but says nothing about whether the money is
  still in the account on the 8th with rent due on the 10th.

There is also a whole class of bills the app cannot see at all: annual dues,
biennial renewals, insurance premiums paid offline, anything paid by check or by
an ACH that Plaid returns undifferentiated. Detection will never find these.
They have to be enterable by hand.

## AI vs deterministic split

**Deterministic:** every obligation, every due date, every projected balance,
every threshold comparison. Cadence arithmetic and balance projection are exact
decimal / date math in SQL and Go. This is the rule from
[00-shared-contracts.md](00-shared-contracts.md) and it is not negotiable here —
a bill calendar that is *approximately* right is worse than none.

**AI:** optional prose only, and only over already-computed figures — e.g. the
digest sentence "you have $2,340 of bills due before your next paycheck." The
model is handed finished `StringFixed(2)` strings. It never derives a due date
and never sums anything.

## Prerequisites

- [04-insight-engine-and-feed.md](04-insight-engine-and-feed.md) — shipped. New
  insight producers plug into the existing `Producer` interface
  (`insights/insights.go:38`) and the existing push path
  (`GenerateInsightsWorker.dispatchInsightPushes`, `jobs/jobs.go:670`).
- The recurring detector and its suppression list (migration `00016`) — shipped.

Nothing here blocks on docs 14-16, and none of them block on this. It can run
fully in parallel.

## Data model

**Reserved migration: `00019_recurring_obligations.sql`.** (Latest on `main` is
`00018_budget_periods.sql`. Re-check it is still free at implementation time.)

```sql
CREATE TABLE recurring_obligations (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id   UUID NOT NULL REFERENCES households (id) ON DELETE CASCADE,
    -- Who entered/owns it. Follows the per-item visibility pattern: a member's
    -- private obligation must not leak into the household view.
    user_id        UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    is_shared      BOOLEAN NOT NULL DEFAULT TRUE,

    label          TEXT NOT NULL,
    amount         NUMERIC(20,4) NOT NULL,
    category_id    UUID REFERENCES categories (id) ON DELETE SET NULL,
    account_id     UUID REFERENCES accounts (id) ON DELETE SET NULL,

    -- Cadence as (n, unit) so "every 2 years" and "every 2 weeks" are both
    -- expressible. Deliberately not a cron string: this must be arithmetic a
    -- user can check by hand.
    interval_count INT  NOT NULL CHECK (interval_count > 0),
    interval_unit  TEXT NOT NULL CHECK (interval_unit IN ('day','week','month','year')),
    anchor_date    DATE NOT NULL,          -- first occurrence; all others derive
    end_date       DATE,                   -- NULL = open-ended

    -- Provenance. 'detected' rows are promoted from GetRecurringMerchants and
    -- carry the merchant_key so the detector can recognise its own output and
    -- not double-count. 'manual' rows have no merchant_key.
    source         TEXT NOT NULL DEFAULT 'manual'
                        CHECK (source IN ('manual','detected')),
    merchant_key   TEXT,
    is_active      BOOLEAN NOT NULL DEFAULT TRUE,

    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX recurring_obligations_household_idx ON recurring_obligations (household_id);
-- One detected row per merchant per household; re-detection updates in place.
CREATE UNIQUE INDEX recurring_obligations_detected_key
    ON recurring_obligations (household_id, merchant_key)
    WHERE source = 'detected';
```

**Design notes that matter:**

- **`anchor_date` + `(count, unit)`, not a stored "next due date."** A stored
  next-due is a cache that goes stale and has to be swept by a job. Deriving
  occurrences from the anchor is pure and testable, and generating N occurrences
  in a window is a `generate_series`-shaped query.
- **Month arithmetic needs a documented rule.** "Monthly from Jan 31" must
  produce Feb 28 (or 29), not roll into March. Postgres interval addition
  clamps correctly (`date '2025-01-31' + interval '1 month'` → `2025-02-28`); if
  any of this is computed in Go instead, note that `time.AddDate` does **not**
  clamp — it normalises Feb 31 → Mar 3. Pick one place to do it and test the
  month-end cases explicitly.
- **Do not delete the existing detection.** `GetRecurringMerchants` stays as-is
  and keeps feeding the Spending table, the recap, and the chat tool. Promotion
  writes `source='detected'` rows *alongside* it. The `recurring_overrides`
  suppression must be honoured on promotion too — a merchant the user marked "not
  recurring" must never reappear here.

## Backend

### Occurrence expansion

One query, `ListUpcomingObligations(household_id, user_id, from, to)`, returning
one row per (obligation × occurrence) in the window, already filtered by the
standard visibility predicate (`user_id = $2 OR is_shared`) that every reporting
query uses. Respect `end_date` and `is_active`. This single query backs the
calendar, the list view, the balance projection, and the insight producer — do
not write four variants.

### Promotion job

Extend the existing insights generation pass (or add a sibling job in
`backend/internal/jobs/`) to upsert `source='detected'` rows from
`GetRecurringMerchants`. Map `avg_gap_days` → the nearest sane cadence (weekly /
biweekly / monthly / quarterly / annual) rather than storing a raw gap; a
28-32-day average is monthly. Set `anchor_date` from `last_seen`. Re-running must
be idempotent — that is what the partial unique index is for.

A detected row the user has edited should not be clobbered by the next pass. Add
`user_edited BOOLEAN NOT NULL DEFAULT FALSE` to the table above and skip those
rows on re-detection, or the first promotion pass will silently undo every
correction the user makes.

### Balance projection

`projected_balance(account, day) = current_balance − Σ(obligations due ≤ day)`.
Compute in SQL/decimal, never in the frontend. Return the series per account over
the requested horizon. Two honesty constraints:

- Only project accounts where a balance is meaningful — depository accounts.
  Projecting a credit card's balance against its own bills double-counts.
- Label it plainly as a projection from *known* obligations. It is not a
  prediction of discretionary spending and must not be presented as one.

### Safe-to-spend integration

`backend/internal/reporting/safetospend.go`. This is the highest-value and
highest-risk part of the doc: the fixed-cost input currently comes from
*trailing typical fixed spend* (`safeFixedMonths` window, lines 76-90). Adding
obligations naively **double-counts every bill that is both detected as a
recurring charge and already in the trailing fixed average.**

The rule to implement and comment, mirroring the existing no-double-counting
comment at lines 24-27: for the current month, a fixed cost counts **once** — as
its *remaining unpaid obligations* where obligations are known, and as the
trailing typical figure only for categories with no obligation coverage. Prefer
splitting the fixed component per category rather than swapping the whole input.

Add the new figure as an *additional* field (e.g. `upcoming_obligations` and a
`safe_to_spend_after_bills`) rather than silently redefining `safe_to_spend`.
The existing field is consumed by the Budgets page and the chat tool; changing
its meaning under them is how the numbers start disagreeing across surfaces.

### New insight + alert types

- **Upcoming bill** insight — fires N days before a due obligation, via the
  existing `Producer` interface. Priority high enough to reach
  `insightPushMinPriority` for large amounts so it actually pushes.
- **Predicted low balance** alert — a new type in
  `backend/internal/alerts/alerts.go`. Follow the existing shape exactly:
  register in `IsValidType`/`ValidateConfig` (lines 37-50), add a
  `predictedLowBalanceConfig` struct and an `evalPredictedLowBalance` function
  alongside `evalLowLeftover` (line 291), and it inherits the whole dispatch and
  push path for free.

## Frontend

- **New `Schedule` route** (`frontend/src/routes/Schedule.tsx`), registered in
  `App.tsx` and the nav. Follow `Spending.tsx` conventions: `glass` cards,
  `formatMoney` from `lib/money.ts`, TanStack Query.
  - Month-grid calendar, each obligation on its due day.
  - List view — next 30 / 60 / 90 days, with merchant, amount, account, days
    until due.
  - Per-account projected-balance line chart over the horizon, with a visible
    zero line. A projected dip below zero is the whole point; make it obvious.
- **Manual entry form** — label, amount, cadence (count + unit), anchor date,
  optional end date, category, account. This is the path for the bills Plaid
  cannot see, so it must be first-class, not buried.
- **Edit / deactivate** for detected rows, so a wrong cadence is fixable.
- **Dashboard**: a "bills due this week" strip, and extend the existing
  month-pace callout to include known upcoming bills.
- **Budgets page**: surface `safe_to_spend_after_bills` next to the existing
  figure, clearly labelled.

## Verification

- Throwaway Postgres per the README's instructions; `go test -p 1 ./...`.
- **Table-driven cadence tests are the core of this doc.** Cover: monthly from
  the 31st across February (leap and non-leap), `every 2 weeks` across a DST
  boundary, `every 2 years`, an obligation whose `end_date` falls mid-window, and
  a window that starts before `anchor_date` (must yield nothing).
- Idempotency: run promotion twice, assert one row per merchant, assert a
  `user_edited` row is untouched.
- Suppression: a merchant in `recurring_overrides` never gets promoted.
- **Double-counting assertion:** a household with one detected monthly
  obligation and matching trailing fixed spend must not have that bill subtracted
  twice from safe-to-spend. Assert the exact expected decimal.
- `sqlc generate` after query changes; frontend `tsc -b`, `vite build`, `oxlint`.

## Out of scope

- Paying bills. The app never moves money.
- Predicting *discretionary* spending in the balance projection. Known
  obligations only — anything else is a guess wearing a number's clothes.
- Merchant canonicalization (TODO #6). It would improve promotion accuracy;
  it is its own doc.
