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

## What it is made of

A breakdown of:

| Assets | Liabilities |
| --- | --- |
| Cash & deposits | Credit cards |
| Investments | Loans |
| Other | Manual debt |
| Manual assets | |

## Manual assets

Things Plaid cannot see — home equity, vehicles, a private loan. Add a name,
kind (home / vehicle / cash / collectible / other), value, and whether it's a
debt. Sent as a decimal **string** so it never goes through a float.

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
