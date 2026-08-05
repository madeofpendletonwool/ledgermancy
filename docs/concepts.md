# Concepts

These rules decide every figure the app reports. Getting them wrong is how
finance apps quietly lie — so they're documented here in full. Each one is a
load-bearing decision, several of which were bugs already found and fixed.

---

## Money is never a float

| Layer | Representation |
| --- | --- |
| Database | `NUMERIC(20,4)` |
| Go | `shopspring/decimal` |
| Wire | decimal **strings**, never JSON numbers |
| Browser | display only |

Plaid returns JSON numbers; they are parsed straight into decimal, never through
`float64`. The single float boundary is Plaid's own SDK, isolated in
`amountToDecimal` with a test pinning it.

The consequence: **every total is computed server-side in Postgres**, never in
JavaScript. Money crosses the wire as a decimal string so the backend's exact
`NUMERIC` value isn't dragged through a float on the way out. Formatting one
value for display is fine; summing them in JavaScript is not.

---

## Spending

How the headline numbers are defined:

- **Plaid signs amounts positive = money out.** Spending sums positive rows;
  income is the negation of rows in income categories.
- **Transfers count as neither income nor spending.** Moving money between your
  own accounts is not either one. (See [below](#transfers-credit-card-payments).)
- **Monthly averages divide by elapsed months**, not months touched. An
  inclusive "+1" turns a trailing year into 13 months and understates every
  average by ~8%.
- **All arithmetic happens in Postgres in `NUMERIC`** — never in Go, never in JS.

The Dashboard's pace hint ("$X more/less than last month by day N") is the one
exception: it sums a handful of already-exact daily totals for a secondary
comparison, never as a headline figure.

---

## Transfers & credit-card payments

This is the rule most apps get wrong, and it double-counts a lot of money:

- **A transfer is money between your own accounts.** It is excluded from income
  and spending entirely.
- **Credit-card payments are transfers, not spending.** The purchases were
  already counted when they hit the card; counting the payment too would double
  every dollar spent on credit.

`LOAN_PAYMENTS_CREDIT_CARD_PAYMENT` maps to a transfer category specifically to
prevent this. There's also a **transfer/card-payment detection heuristic** at
ingest for the cases Plaid returns as `OTHER_OTHER`.

You control this via [category typing](features/categories.md#category-types):
set a category to **Transfer (not spending)** to take it out of the spend total
— the fix for a category that was wrongly inflating your spend.

---

## Categorisation order

Categorisation tries, in order:

1. **Manual choice** — `category_source = 'manual'`, preserved by the sync
   upsert so Plaid can never overwrite it.
2. **Household rule.**
3. **Merchant cache** (`merchant_category_map`).
4. **Plaid's own category.**
5. **(optional, AI) an LLM**, whose answer is then cached in the merchant map so
   a merchant is never sent twice.

Steps 1–4 are **deterministic and free**, which is why the AI layer stays
optional. The merchant map's `UpsertMerchantCategory` refuses to overwrite a
`manual` entry, so a cache hit never clobbers a deliberate choice.

---

## CSV exports

The [Financial Summary](features/report.md) CSVs are designed to be summed in a
spreadsheet without a formula:

- Transaction amounts are **sign-flipped** from Plaid's convention so **negative
  = money out**. Summing the column gives the right answer directly.
- The `is_transfer` and `is_income` flags are included so a spreadsheet can
  reproduce the app's totals rather than guess which rows to exclude.

---

## Why net worth is snapshotted

Balances are a **point-in-time** value. Plaid reports what an account holds
today and keeps no history, so yesterday's net worth is unrecoverable once a
balance moves.

The figure is therefore written down daily — after each sync, and on a schedule
so quiet households still get points. The trend can only start from the day the
app did, which the UI says plainly rather than implying older data exists.

This is also why **losing the database loses the net-worth trend**: Plaid cannot
re-supply it. See [Deployment → Back up the database](deployment.md#back-up-the-database).

---

## Projections are arithmetic, not forecasts

`internal/networth/project.go` is a plain compounding model:

- Growth applies to the invested balance *before* each month's contribution.
- Debt paydown floors at zero.
- A negative surplus draws down assets.

Every assumption is visible, adjustable, and echoed back in the response
alongside an `estimate: true` flag. **Nothing here predicts markets.** The
Financial Summary's Outlook section uses the same engine and carries the caveat
on the page itself.

---

## Real dollars and nominal dollars

Every figure the app stores is **nominal**: the dollars of the day it was
recorded. That is correct for storage and wrong for comparison, because a 2019
dollar and a 2026 dollar are not the same thing. "Net worth up 8% this year" in
a 6% inflation year is 2% real growth, and an app that cannot say so is not
telling you the truth.

Any long-horizon chart can therefore be switched into **real (inflation-adjusted)
dollars**:

```
real = nominal × index[base] / index[recorded]
```

Five rules keep that honest.

**Nominal stays the default.** Real is opt-in, per user, and remembered.
Quietly changing what an existing figure means would break every comparison you
carry in your head.

**Deflation is a view, applied at read time.** Nothing stored is ever rewritten.
The stored number is what it was, in the dollars of its own day, permanently.

**The base month is always stated.** Real figures are labelled "in June 2026
dollars", not "in today's dollars" — the current month's index does not exist
yet, so the base is the newest month BLS has published. A real number without
the month it is denominated in is not a number you can use.

**A month with no index is a gap, never a nominal figure in disguise.** The
series has one permanent hole: **October 2025 was never published**, because
that year's lapse in appropriations stopped collection, and BLS will not
estimate it after the fact. Points dated there are dropped from a real chart and
counted underneath it. Interpolating between September and November would have
been within 0.2% and would still have been an invented number.

**Returns are deflated by division, not subtraction.** Real return is
`(1 + nominal) / (1 + inflation) − 1`. At 20% nominal and 6% inflation,
subtraction says 14% and the truth is 13.2% — an error that always runs in the
flattering direction, and compounds.

The index is **CPI-U, U.S. city average, all items, not seasonally adjusted**
(BLS `CUUR0000SA0`). Not seasonally adjusted on purpose: the SA series is
revised annually for five years running, which would mean a figure you saw last
month quietly changing.

The series **ships bundled**, from January 2010, so this works with no outbound
access at all. `CPI_FETCH_ENABLED` only adds new months as they are published;
with it off, the app says the series is behind rather than pretending otherwise.
See [Configuration → CPI refresh](configuration.md#cpi-refresh-inflation-adjusted-views).

Short windows do not offer the toggle. Deflating one month by one month's price
change moves the figure by a couple of tenths of a percent — noise dressed as
precision.
