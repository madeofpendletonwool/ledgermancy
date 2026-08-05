# Net worth

![Net worth](../screenshots/net-worth.png)
<em>Everything you own, minus everything you owe.</em>

Net worth tracks everything you have and everything you owe, snapshotted daily,
with a breakdown and a projection.

## Headline tiles

**Assets**, **Liabilities**, and **Net worth** — the last in large type, green
when positive, red when in debt.

## Over time

A line chart of daily net-worth readings. **Record today** writes a fresh
snapshot on demand (snapshots otherwise happen after each sync and on a daily
schedule).

The line starts the day Ledgermancy did — see [Why net worth is
snapshotted](../concepts.md#why-net-worth-is-snapshotted) for why.

The y-axis fits the data with a margin rather than anchoring to zero. That's
deliberate: a household carrying a mortgage sits well below zero, and anchoring
to zero squashed the trend into the bottom of the plot where no movement was
visible. Zero is pulled in whenever the data comes near it, so a crossing into
positive net worth is never hidden. A dashed zero line appears when relevant.

**Nominal / Real.** The chart can be switched into inflation-adjusted dollars.
Nominal is the default and stays that way; the choice is remembered per user.
When real is on, every point is restated in the base month's dollars and the
chart says which month that is — "in June 2026 dollars", never "today's", since
the current month's index is not published yet.

A reading whose month has no published price index is **dropped and counted
underneath the chart**, not carried over at its nominal value. See
[Real dollars and nominal dollars](../concepts.md#real-dollars-and-nominal-dollars).

## What it is made of

A breakdown of:

| Assets | Liabilities |
| --- | --- |
| Cash & deposits | Credit cards |
| Investments | Loans |
| Other | Manual debt |
| Manual assets | |

## Manual assets

Things Plaid cannot see — home equity, vehicles, a private loan, savings bonds.
Add a name, kind (home / vehicle / cash / collectible / other / debt), value,
and whether it's a debt. Sent as a decimal **string** so it never goes through a
float.

### Value history

Every manual asset keeps its current value alongside an **append-only history**
of every value it has ever had, with the date and a reason. Editing the value
writes the new figure *and* a history row in the same transaction, so the figure
and its explanation can never drift apart. The trend line is drawn from that
history, which means a home revalued a year ago finally moves the curve when it
actually moved, not the day you got around to entering it.

### Revaluation vs. depreciation

The app never silently moves a number on you. There are two ways a non-cash asset
gets revalued, and the distinction is load-bearing:

- **An estimate is always a proposal.** A vehicle can be depreciated along a
  published curve — roughly 20% of value lost in the first year and 15% of the
  remainder each year after, with a tilt for mileage — and the page will show the
  proposed figure, the curve that produced it, and every input, then wait for
  you to accept it or override it. **Nothing is written until you do.** Silently
  depreciating a car would mean net worth moving with no user action in an app
  whose whole pitch is that the numbers are honest and checkable.
- **A savings bond is arithmetic, not an estimate.** A Series I or EE bond's
  value is computed against published Treasury rates — composite rate, semiannual
  compounding, the three-month interest forfeiture before five years, the EE
  guarantee of doubling at 20 years. That is deterministic: anyone computing it
  gets the same answer. Savings bonds are the one asset whose value the app will
  update for you, because there is no judgement in it. The published rate table
  is bundled and seeded, and each row names where it came from so any figure can
  be walked back to treasurydirect.gov by hand.

Bonds held in a brokerage already work through your holdings and are not
duplicated here — this is for the ones held directly at TreasuryDirect, which
Plaid cannot link.

### Linking an asset to a loan

A manual asset can be linked to a loan account so the page can show **equity**
— the asset's value minus the debt against it — without double-counting the
balance either way.

## Holdings

If you have investment accounts linked, a table of positions across them:
security, ticker, account, quantity, value, and gain (green/red).

## Debt

If you have liabilities linked, a list with balance, APR, kind, institution, and
next-payment due date.

## Projections

A plain compounding model — see [Projections are arithmetic, not
forecasts](../concepts.md#projections-are-arithmetic-not-forecasts). Every
assumption is visible, adjustable, and echoed back. The Financial Summary's
[Outlook](report.md) section uses the same engine.
