# 22 — Predictive anomaly detection

*(TODO.md "Next major initiatives" #2.)*

**Shipped.** Migration `00046_anomaly_overrides.sql` is taken. Two producers,
`merchant_outlier` and `duplicate_charge`, in `insights/anomaly.go` over
`db/queries/anomaly.sql`.

Six things anyone touching this should know, because five of them are places the
plan below was wrong against the code.

**There is no `merchant_baselines` table, and there should not be one.** The
baseline has to be *leave-one-out* — computed excluding the charge being judged —
or `PERCENTILE_DISC(0.95)` over a merchant whose largest-ever charge IS the
candidate returns the candidate, `amount >= p95` is trivially true for every new
maximum, and the detector fires on everything. A stored table cannot supply that:
mean and count are subtractable so leave-one-out is recoverable from them, but
**median and p95 are order statistics and are not**, so the candidate can never be
backed out afterwards. The table this doc specified would have been a cache that
is *wrong*, not merely redundant. `GetMerchantSpendBaseline` already knew this —
it takes an `exclude_tx` parameter for exactly this reason. Baselines are computed
on demand in a `CROSS JOIN LATERAL`, which also means no rebuild job and no
`Derived` continuity entry.

**`largeTransactionProducer` and `merchantOutlierProducer` are two halves of one
behaviour**, and the doc did not notice they collide. `large_transaction` fires at
priority 4 (which pushes) for any charge over max(4× the household-wide average,
$150), so without a hand-off every large charge at a known merchant raised two
insights and pushed twice. The rule: **`merchant_outlier` claims any merchant with
5+ prior charges; `large_transaction` covers everything else.** Where a baseline
exists the per-merchant message strictly dominates — "Netflix normally charges
$15.99, this one is $900" beats "above your typical purchase of about $55" — and
where none exists, which is where most genuine fraud lands, the household-wide
gate is the only signal there is. `large_transaction` keeps its kind, priority,
wording and **its exact dedupe key**, so feed rows raised before this shipped
still match. The gate is `outlierMinSamplesFloor`, the *lowest* `minSamples`
across all sensitivities, so tightening the household setting cannot open a gap
where neither producer speaks; `TestNoSensitivityDropsBelowTheHandoffFloor`
asserts that.

**`transactions.date` is a `DATE`.** So is `authorized_date`. There is no
time-of-day anywhere in the schema, and this doc's "within a short window
(24–48h)" was unimplementable as written. The window is `ABS(b.date - a.date) <= 1`.

**Amount equality does not solve the two-coffees problem** — two $5.75 lattes
*are* exactly equal, so the doc's remedy was no remedy. What works is the dollar
floor plus a **habitual-repeat check**: if a merchant on that card has ever
produced a same-amount adjacent-day pair before the recent window, doubling up is
what it does there and it is silenced permanently. That covers transit fares,
vending, parking and the fixed-price daily coffee. Two guards the doc did not
list also matter: matching within `account_id` (one institution linked twice
reports the same charge on two cards) and `is_spend()` dropping reversals by
sign. The recurring-cadence guard the doc asks for is **vacuous** at a ±1 day
window — the tightest cadence `obligations.CadenceForGapDays` recognises is
weekly — so it is documented in the query rather than coded.

**Dismissal already survived regeneration before this shipped**, because
`UpsertInsight` deliberately omits `dismissed_at` from its `DO UPDATE SET`. The
thing that actually needed asserting is **dedupe-key stability**, which is why
both kinds key on a transaction id rather than the `merchant:date:amount` shape
`large_transaction` uses: transaction UUIDs are stable across syncs, whereas a
merchant-keyed key changes the moment the user merges that merchant — silently
resurrecting an insight they had already dismissed.
`TestAnomalyDedupeKeySurvivesAMerchantMerge` is the guard.

**"Frontend mostly free" was wrong.** No insight kind read `insight.data`
anywhere in the app; `InsightRow` rendered only a chip, title, body and
timestamp. The inline comparison is the first consumer of that field and is
net-new component work (`AnomalyDetail`). It is kind-switched and guards every
field, so an insight stored before a payload change degrades to title-and-body
rather than breaking the feed.

One thing the doc got right and is worth restating: the merchant key falls back
to `'name:' || lower(name)` when `merchant_key` is NULL. Plaid populates that
column; CSV imports and manual entries have no key at all, and requiring one (as
`GetRecurringMerchants` does) would have silently dropped that whole population
from `large_transaction` too, which used to cover it.

## Context

**First, a correction to TODO #2's framing.** It lists "price creep" as new work.
It is already shipped: `insights/subscription.go:100-118` emits a price-creep
insight with `old_amount` / `recent_amount` / delta, backed by
`reports.sql:379`'s `AVG(amount) FILTER (WHERE half = 2) AS recent_avg`. Do not
rebuild it.

What is genuinely missing is everything that needs a **per-merchant statistical
baseline**. Today's anomaly surface is category-level spike detection plus that
one subscription-specific comparison. Neither catches:

- A single transaction 3× the merchant's historical typical amount.
- The same merchant charging the same amount twice within 24 hours — the
  classic duplicate-charge and the most common real billing error.
- An outlier at a merchant that is not a detected subscription at all.

These are the patterns that catch fraud and billing mistakes, and they are the
things a user most wants the app to notice unprompted. The app holds the
transaction history to compute all of them and does not.

## AI vs deterministic split

**Deterministic:** detection, baselines, thresholds, every figure. These are
statistics, not judgement — squarely inside the README's "deterministic before
AI" rule.

**AI:** at most, phrasing an *already-detected* anomaly in plain English, handed
finished strings. The model never decides what is anomalous. A false fraud alert
generated by a model would be both alarming and unexplainable.

## Prerequisites

None hard, but **materially better after [17-merchant-canonicalization.md](17-merchant-canonicalization.md)**.
Baselines built over fragmented merchant strings are built over the wrong unit:
`AMZ*ORDER` and `AMAZON.COM` get separate, thinner baselines and the outlier
threshold is wrong for both. If 17 is landing, build against the resolver
interface from the start.

Rides the existing insight spine from
[04-insight-engine-and-feed.md](04-insight-engine-and-feed.md) — shipped.

## Data model

**Reserved migration: `00036_merchant_baselines.sql`.**

```sql
-- Rolling per-merchant statistics. A cache, not a source of truth: it is
-- derivable from transactions at any time and can be rebuilt from scratch.
-- Keyed on merchant_key to match the existing merchant_category_map access
-- pattern; when doc 17 lands, resolve through the entity first.
CREATE TABLE merchant_baselines (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id  UUID NOT NULL REFERENCES households (id) ON DELETE CASCADE,
    merchant_key  TEXT NOT NULL,
    sample_count  INT  NOT NULL,
    mean_amount   NUMERIC(20,4) NOT NULL,
    stddev_amount NUMERIC(20,4) NOT NULL,
    median_amount NUMERIC(20,4) NOT NULL,
    -- p95 is the practical outlier gate for skewed spend distributions, where
    -- mean+3σ is distorted by the very outliers it is meant to catch.
    p95_amount    NUMERIC(20,4) NOT NULL,
    computed_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (household_id, merchant_key)
);
```

**Do not extend `merchant_category_map`.** TODO #2 suggests "one more column
family, same access pattern," but that table is a *decision* cache with a
`manual | llm | rule` source and stickiness semantics; mixing derived statistics
into it muddies both. A separate table is rebuildable and droppable.

**Spend distributions are right-skewed.** Mean + 3σ over a merchant with one past
outlier produces a threshold nothing ever crosses. Compute both, prefer a
median/p95-based gate, and make the sensitivity configurable.

## Backend

New producers in `backend/internal/insights/`, registered via `DefaultProducers()`
(`insights/insights.go:56`). Each implements the existing `Producer` interface and
inherits dedupe, expiry, the feed, and the push path
(`dispatchInsightPushes`, `jobs/jobs.go:670`) for free.

### Baseline maintenance

A job that recomputes baselines over a trailing window. Exclude transfers,
income, and `excluded_from_reports` rows using the same predicates every report
query uses — a baseline computed over a different transaction universe than the
detector produces inconsistent results.

**Require a minimum sample count** (5+) before a merchant has a baseline at all.
Two data points do not have a standard deviation worth acting on, and the first
week of a new install must not fire outlier alerts on everything.

### Outlier single charge

Compare each new transaction to its merchant's baseline. Emit when it exceeds the
configured gate **and** clears a minimum absolute dollar amount — a $4 coffee
that is 3× a $1.30 baseline is statistically an outlier and practically noise.
Both gates, always.

### Possible duplicate

Same merchant, same or near-same amount, within a short window (24–48h).

The false-positive sources are specific and must all be handled:

- **Pending → posted.** `transactions.pending` exists; a pending row and its
  posted counterpart are the same charge. Exclude pending from comparison
  entirely.
- **Legitimate same-day repeats** — two coffees, two tank fills. Require exact or
  near-exact amount equality, not just proximity.
- **Recurring charges** landing on their cadence are not duplicates.

Priority should be high enough to reach `insightPushMinPriority` for large
amounts. A duplicate $900 charge is worth a push; a duplicate $6 one is not.

### Feedback

A dismissed anomaly must not re-fire on the next pass. The insight feed already
supports dismissal — verify it survives regeneration for these producers, and add
a per-merchant "this is normal" suppression modelled on `recurring_overrides`
(migration `00016`), which solved exactly this problem for the recurring
detector.

## Frontend

Mostly free — these render in the existing Insights feed.

Additions:

- Anomaly insights show the comparison inline: this charge, the typical charge,
  and the sample size. "Unusual" with no baseline shown is unactionable.
- "This is normal" dismissal that writes the per-merchant suppression.
- A sensitivity setting in Settings (conservative / balanced / sensitive) mapping
  to the gate thresholds.

## Verification

- `go test -p 1 ./...`.
- **Baseline maths** against hand-computed fixtures, including a merchant with a
  single historical outlier — assert the p95 gate still fires on a genuine new
  outlier where a mean+3σ gate would not. That contrast is the reason for the
  design.
- Minimum sample count: a merchant with 3 transactions produces no baseline and
  no insight.
- **Duplicate detection false-positive suite:** pending+posted pair → no insight;
  two same-day coffees at different amounts → no insight; a monthly subscription
  on cadence → no insight; a genuine same-amount double charge → insight.
- The absolute-dollar floor suppresses the 3×-a-$1.30-baseline case.
- Dismissal survives a regeneration pass.
- Price creep still works and is not duplicated by the new outlier producer —
  run both against a creeping subscription and assert exactly one insight.
- Frontend `tsc -b`, `vite build`, `oxlint`.

## Out of scope

- Rebuilding price creep. It exists.
- Seasonal modelling per merchant. Named in TODO #2 as an option; a rolling
  window is enough here and seasonality needs years of history.
- Contacting the merchant or disputing a charge.
- Cross-household anomaly signals.
