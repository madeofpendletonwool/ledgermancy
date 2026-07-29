# 26 — Real-asset revaluation, depreciation, and directly-held bonds

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

### Bonds: the other stale asset

Bonds split into two cases and only one of them works today.

**Held in a brokerage** — Treasuries bought through Schwab, corporates, munis, a
bond fund. These are already fine. Plaid returns them as securities with
`type = 'fixed income'`, `assetClassLabel` maps that to "Fixed income"
(`reporting/investments.go:307`), and they flow into holdings and allocation
normally. One thing to preserve rather than fix: every consumer sums
`holdings.institution_value` and **nothing recomputes `quantity × price`**. Bond
prices quote as a percent of par — 98.75 means $987.50 per $1,000 — so a
recomputation would be wrong by roughly 100×. Do not introduce one.

**Held directly at TreasuryDirect** — Series I, Series EE, marketable Treasuries
bought at auction. TreasuryDirect is not Plaid-linkable, so the only home for
these today is a `manual_assets` row: a frozen number that never accrues.

This is the same drift as the house, except worse, because the correct value is
not an estimate — it is **arithmetic against published rates**:

- **Series I**: a fixed rate plus an inflation rate, composite recomputed every
  May 1 and November 1, compounding semiannually and accruing monthly. Redeem
  before five years and the last three months' interest is forfeit. Given issue
  date, denomination and the rate table, the redemption value is exact.
- **Series EE**: a fixed rate, but **guaranteed to double at 20 years**. That is
  a cliff, not a curve. Any pure compounding model underprices an EE bond
  approaching year 20, sometimes badly.

An I bond sitting in `manual_assets` understates net worth by a growing amount
forever, and unlike a home valuation there is no estimation error to hide behind.
That is exactly the quiet dishonesty this codebase keeps refusing.

It is also a children's-assets feature. Savings bonds in a child's name are one
of the most common gifts a grandparent makes, and doc 21's
`manual_assets.person_id` is what attaches them to the child.

## AI vs deterministic split

**Deterministic:** depreciation curves, equity, value history, bond accrual and
redemption values, every figure.

**AI:** none required. If auto-valuation is implemented it is an API call, not a
model — and a model must never be asked to estimate a home's value, or to recall
a Treasury rate. Both would be fabricated numbers in load-bearing lines.

## Prerequisites

None. Parallel-safe.

**Doc 15 benefits directly** — projections are only as good as the asset values
feeding them. If both are in flight, no conflict: 15 reads net worth, this doc
improves what net worth reports.

**Doc 21 is complementary, not required.** 21 adds `manual_assets.person_id`;
bonds valued here attach to a child through it. Either can land first — a bond
with no person is still a correctly valued bond.

## Data model

**Reserved migration: `00039_asset_revaluation.sql`.**

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

    -- Bond. Enough to value the thing from first principles; nothing derived.
    -- `bond_series`: i_savings | ee_savings | treasury | other
    bond_series     TEXT CHECK (bond_series IN ('i_savings','ee_savings','treasury','other')),
    issue_date      DATE,
    -- What was paid. For electronic savings bonds this equals face value; paper
    -- EE bonds were sold at HALF face, which is the single most common way a
    -- savings bond gets entered at twice its real cost.
    purchase_price  NUMERIC(20,4),
    face_value      NUMERIC(20,4),
    -- Marketable Treasuries only. Stored as a percentage (4.25 = 4.25%),
    -- matching `liabilities.apr`.
    coupon_rate     NUMERIC(9,4),
    maturity_date   DATE,
    -- Municipal interest is federally tax-exempt. Recorded so a future tax view
    -- has it; nothing in this doc computes tax.
    tax_exempt      BOOLEAN,

    condition       TEXT CHECK (condition IN ('excellent','good','fair','poor')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Published Treasury savings-bond rates, one row per series per six-month
-- period. This is reference data, and its provenance is the whole point: each
-- row carries the URL it came from so any figure the app produces can be walked
-- back to treasurydirect.gov by hand.
CREATE TABLE savings_bond_rates (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    series       TEXT NOT NULL CHECK (series IN ('i_savings','ee_savings')),
    -- The six-month period this rate was announced for (a May 1 or Nov 1 date).
    period_start DATE NOT NULL,
    -- I bonds: both. EE bonds: fixed only, inflation_rate NULL.
    fixed_rate     NUMERIC(9,6) NOT NULL,
    inflation_rate NUMERIC(9,6),
    source_url     TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (series, period_start)
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

**`manual_assets.kind` needs no migration to gain `'bond'`.** The column is
`TEXT NOT NULL DEFAULT 'other'` with the allowed values written only as a comment
(`00004:115`) — there is no CHECK constraint. Add `bond` to that comment and to
the app-level validation, and leave the column alone.

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

### Bond valuation

A `backend/internal/networth/bonds.go` beside the depreciation curve. Pure
functions over `decimal.Decimal`, taking `now` as a parameter the way
`ProjectRetirement` does.

**Series I.** Walk the six-month periods from `issue_date` forward. For each,
composite rate = `fixed + 2×inflation + (fixed × inflation)`, floored at zero —
the deflation floor is part of the instrument, not a safety check, and an I bond
never loses nominal value. Compound semiannually, accrue monthly within a period.

**Series EE.** Compound the fixed rate. Then, at the 20-year mark and after, the
value is `max(compounded, 2 × purchase_price)` — the doubling guarantee. Test the
month before and the month of; a model that misses this is visibly wrong on the
most common EE bond people actually hold.

**Redemption value is what enters net worth, not accrued value.** Under five
years from issue, the last three months' interest is forfeit on redemption. Net
worth reports what the bond could actually be turned into today; accrued value is
shown beside it, labelled, so the difference is visible rather than swallowed.
Reporting the accrued figure would overstate every bond younger than five years.

**Marketable Treasuries are held at par plus accrued coupon interest, and the UI
says so.** Marking one to market needs a live price the app has no source for.
State the basis rather than implying a market value; a Treasury held to maturity
is worth par, which is what most households holding one intend.

**A missing rate period is refused, not extrapolated.** If the rate table has no
row covering a period the bond spans, return the same `ok=false` shape
`AnnualLimitFor` uses for an unconfigured tax year (`networth/limits.go`), and
surface "valued through <date>; add the rate for <period> to go further."
Carrying the last known rate forward silently invents a return.

### On seeding the rate table

Doc 15 refused to bundle a historical return series on the grounds that it would
be "a transcribed table of numbers nobody can verify." That precedent is real and
this table has to answer it.

It answers it differently, and the difference is why seeding is acceptable here:
each row is a single number, announced on one date by one issuer, checkable
against one page, and `source_url` carries that page per row. A historical equity
return series has none of those properties. Ship a seed migration for the periods
that exist at implementation time, with the URL on every row, and expose the
table for editing so a user who distrusts the seed can verify or replace it.

A twice-yearly job could fetch new rates. It is optional, off by default, behind
a flag, and subject to the README's "phones home to nothing but Plaid" line —
same treatment as auto-valuation below. Manual entry must remain sufficient.

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

**Bonds are the exception to "estimates are proposals."** The rule above exists
because a depreciation curve is a guess and net worth must not move on a guess. A
savings bond's value is not a guess — it is arithmetic over published rates, and
the same inputs give the same answer to the cent every time. So a bond
revaluation is written automatically, monthly, with `source = 'estimated'` and a
note naming the rate periods used, and no confirmation prompt.

Do not blur the two. What licenses the automatic write is *determinism*, not
convenience, and the moment a valuation depends on a judgement it goes back to
being a proposal. A bond spanning a period with no rate row is refused rather
than written, per above.

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

- Class-specific forms (real estate / vehicle / bond / other) driven by `kind`.
  The bond form takes series, issue date, denomination and what was paid — and
  warns on paper EE bonds that the purchase price is half the face value.
- **Per-asset value history chart** — a home that appreciated $80k over five
  years should be visible as a trend, not a single number.
- Revaluation flow: current value, suggestion with its basis, accept or enter
  your own.
- Equity display when a loan is linked, with a payoff-progress bar.
- Staleness indicator on assets with an old `as_of`.
- For a bond: redemption value as the headline, accrued value beside it when the
  two differ, the rate periods that produced them, and — for an EE bond — how far
  it is from the 20-year doubling. A "valued through" date whenever the rate
  table runs out before today.

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
- **Bond valuation, against known-good fixtures with exact decimals.** Every case
  below is a real behaviour of the instrument, not an edge case:
  - an I bond held more than five years, valued to the cent against a
    TreasuryDirect calculator figure recorded in the fixture;
  - the same bond at four years — redemption value is three months of interest
    below accrued value, and it is the redemption figure that reaches net worth;
  - an I bond spanning a deflationary period: composite floors at zero and the
    bond never loses nominal value;
  - an EE bond one month before and one month after its 20th anniversary — the
    doubling guarantee applies on the second and not the first;
  - a paper EE bond entered at half face value produces the right result.
- **A missing rate period refuses.** A bond spanning an unseeded period returns
  `ok=false` and writes no valuation; assert `manual_assets.value` is unchanged.
- Bond valuation is deterministic: the same asset and the same `now` produce
  byte-identical output across runs. Pass `now` explicitly; no test reads the
  clock.
- Every seeded `savings_bond_rates` row has a non-empty `source_url`.
- Frontend `tsc -b`, `vite build`, `oxlint`.

## Out of scope

- Automated property-tax or insurance tracking. Those are recurring obligations
  (doc 13).
- Cost-basis and capital-gains tracking on real estate sale.
- Depreciation for tax purposes. Different rules entirely; do not conflate.
- Collectibles valuation. No usable data source.
- Anything about brokerage-held bonds. They already work through Plaid holdings;
  this doc covers only what Plaid cannot see.
- Marking a Treasury to market, yield-to-maturity, duration, and credit ratings.
  All need a price or ratings feed the app has no source for.
- Tax on bond interest — federal deferral until redemption, the education
  exclusion, state exemption. `tax_exempt` is recorded and nothing reads it yet.
- Tracking redemptions as income. A redeemed bond is edited or deleted by hand.
