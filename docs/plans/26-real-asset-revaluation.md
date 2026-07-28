# 26 — Real-asset revaluation and depreciation

*(TODO.md "Next major initiatives" #14.)*

## Context

`manual_assets` (`00004_investments_liabilities.sql:110`) is a static number typed
in once: `name`, `kind` (`home | vehicle | cash | collectible | other | debt`),
`value`, `as_of`, `notes`. There is no history and no mechanism to update it.

For most households the home is the largest line on the net-worth sheet, and its
value is wrong within months of entry. Vehicles depreciate on a predictable curve
but the app holds the original figure forever. Nobody re-enters these by hand, so
in practice **the net-worth trend drifts further from reality every month** — and
doc 15's retirement projections inherit that drift, compounded over decades.

The `as_of` column already admits the problem exists. This doc gives it teeth.

## AI vs deterministic split

**Deterministic:** depreciation curves, equity, value history, every figure.

**AI:** none required. If auto-valuation is implemented it is an API call, not a
model — and a model must never be asked to estimate a home's value. That would
be a fabricated number in the most load-bearing line on the balance sheet.

## Prerequisites

None. Parallel-safe.

**Doc 15 benefits directly** — projections are only as good as the asset values
feeding them. If both are in flight, no conflict: 15 reads net worth, this doc
improves what net worth reports.

## Data model

**Reserved migration: `00029_asset_revaluation.sql`.**

```sql
-- Class-specific metadata. Kept in a side table rather than widening
-- manual_assets, so a 'cash' or 'collectible' asset carries no vehicle columns.
CREATE TABLE asset_details (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    manual_asset_id UUID NOT NULL UNIQUE REFERENCES manual_assets (id) ON DELETE CASCADE,

    -- Real estate
    address         TEXT,
    beds            NUMERIC(4,1),
    baths           NUMERIC(4,1),
    sqft            INT,
    lot_sqft        INT,

    -- Vehicle
    year            INT,
    make            TEXT,
    model           TEXT,
    trim            TEXT,
    mileage         INT,
    annual_mileage  INT,          -- for estimating between manual updates

    condition       TEXT CHECK (condition IN ('excellent','good','fair','poor')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Every valuation, dated. manual_assets.value stays as the CURRENT value so no
-- existing query changes; this is the trend behind it.
CREATE TABLE asset_valuations (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    manual_asset_id UUID NOT NULL REFERENCES manual_assets (id) ON DELETE CASCADE,
    value           NUMERIC(20,4) NOT NULL,
    as_of           DATE NOT NULL,
    source          TEXT NOT NULL CHECK (source IN ('manual','estimated','api')),
    note            TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (manual_asset_id, as_of)
);
CREATE INDEX asset_valuations_asset_idx ON asset_valuations (manual_asset_id, as_of DESC);

-- Tie an asset to the loan secured against it. Equity = value − balance.
ALTER TABLE manual_assets ADD COLUMN loan_account_id UUID
    REFERENCES accounts (id) ON DELETE SET NULL;
```

**Backfill a `manual_assets.value` + `as_of` row into `asset_valuations` on
migration**, so every asset has a history starting from what the user originally
entered rather than a gap.

**Keep `manual_assets.value` authoritative for current value.** Every existing
net-worth query reads it; making them join to find the latest valuation is a wide
blast radius for no benefit. Writing a valuation updates both, in one
transaction.

## Backend

### Depreciation

A vehicle curve in `backend/internal/networth/`: a steep first-year drop then a
declining annual rate, adjusted for mileage against `annual_mileage`.

Two constraints:

- **Estimated values are proposals, not writes.** An estimate produces a
  *suggested* revaluation the user confirms. Silently depreciating an asset means
  net worth changes with no user action and no explanation — unacceptable in an
  app whose pitch is that the numbers are honest and checkable.
- **Document the curve's source** in a comment and show it in the UI. A
  depreciation figure the user cannot interrogate is indistinguishable from a
  guess.

### Optional auto-valuation

Off by default, behind a config flag, same treatment as doc 14's benchmark fetch
and doc 25's SMTP — and **update the README's "phones home to nothing but Plaid"
line** if not already updated by those docs.

Be realistic in the doc about availability: the Zillow Zestimate API has been
progressively restricted and is likely not viable; Redfin and Realtor.com have
no clean public API and their ToS restrict scraping. Vehicle values via KBB /
Edmunds similarly require commercial agreements. **Build the manual + curve path
first and treat any API as an optional adapter behind an interface.** Do not let
this doc block on a data source that may not exist.

### Revaluation nudges

An insight producer (existing `Producer` interface) firing when an asset's
`as_of` is older than a threshold — 12 months for real estate, 12 for vehicles.
"You set your home value 18 months ago at $X — want to update it?" Rides the
existing feed and push path.

This is the highest-value part of the doc and it needs no external API at all.

### Equity

With `loan_account_id` set, equity = asset value − linked liability balance.
Surface per asset. This makes "I own both cars outright" and "I have $30k equity
in my car" both first-class, where today the asset and the loan are unrelated
rows.

Guard against double-counting: net worth already counts the asset as an asset and
the loan as a liability. **Equity is a derived display figure — it must not enter
the net-worth sum.** Assert this.

## Frontend

Extend the manual-assets section of `NetWorth.tsx`:

- Class-specific forms (real estate / vehicle / other) driven by `kind`.
- **Per-asset value history chart** — a home that appreciated $80k over five
  years should be visible as a trend, not a single number.
- Revaluation flow: current value, suggestion with its basis, accept or enter
  your own.
- Equity display when a loan is linked, with a payoff-progress bar.
- Staleness indicator on assets with an old `as_of`.

## Verification

- `go test -p 1 ./...`.
- Migration backfill: every pre-existing asset gets exactly one seed valuation
  matching its `value`/`as_of`.
- Depreciation curve against fixtures — a 3-year-old car at average mileage, and
  a high-mileage case. Assert exact decimals.
- **An estimate never writes a value without confirmation.** Run the estimator
  and assert `manual_assets.value` is unchanged.
- **Equity does not double-count:** net worth for a household with a linked
  asset+loan equals net worth with them unlinked. Exact decimal.
- Writing a valuation updates `manual_assets.value` and inserts the history row
  atomically; a failure leaves neither.
- Nudge fires at the staleness threshold and not before.
- Frontend `tsc -b`, `vite build`, `oxlint`.

## Out of scope

- Automated property-tax or insurance tracking. Those are recurring obligations
  (doc 13).
- Cost-basis and capital-gains tracking on real estate sale.
- Depreciation for tax purposes. Different rules entirely; do not conflate.
- Collectibles valuation. No usable data source.
