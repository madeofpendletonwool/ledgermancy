# Investments

<em>What you hold, what it has actually returned, and what it costs you to hold it.</em>

Net worth shows your investments as one number. This page opens it up: per-holding
detail, real return calculations, allocation, and dividends.

## Returns are measured from the day Ledgermancy started watching

This is the most important thing on the page, and it is not a limitation that can
be engineered away. Your institution reports what you hold **today** and keeps no
history — there is no endpoint that will tell Ledgermancy what your portfolio was
worth last March. So a value is recorded once a day from the moment you link the
account, and every return figure is measured over that recorded history.

A three-day-old install has three days of history. The page says so, in the
performance block, above the numbers rather than below them. It will not render
an annualised return off a fortnight of data.

## Headline tiles

**Total value**, **Unrealised gain**, and **Recorded history** (how many days of
value readings exist).

The unrealised gain covers only holdings that report a cost basis. When some do
not, the page says how much value the figure covers and how many holdings were
left out — rather than counting a basis-less position as pure profit.

## Performance

Two return figures, over YTD / 1 year / 3 years / 5 years / all history:

| | What it measures |
| --- | --- |
| **Time-weighted (TWR)** | The **portfolio**. Deliberately blind to when you paid money in, so a big deposit does not read as a good year. |
| **Money-weighted (IRR)** | **You**. Accounts for when your money was actually invested, so the same market path gives a different answer depending on your timing. |

Both are computed in exact decimal from recorded values and your institution's
own investment transactions. Deposits, withdrawals and transfers are treated as
external cash flows and removed from the return; buys, sells, dividends, interest
and fees happen *inside* the account and stay in it.

Alongside them: **market gain** (the money the market made, with your deposits
stripped out) and **net paid in**.

### When a figure refuses to appear

- **"Not computable"** on the money-weighted return means the internal-rate-of-
  return solve found no rate that fits your cash flows. That is a real answer for
  some flow patterns, and the page gives the reason rather than a plausible
  number.
- **No annualised figure** below a year of history. A 5% gain over three weeks is
  not an 87% annual return in any sense a reader would understand.

### Real returns

The **Real** toggle restates both return figures after inflation, using the
bundled CPI-U series over the same span. The arithmetic is
`(1 + nominal) / (1 + inflation) - 1` — not subtraction, which is wrong by the
product of the two and always in the flattering direction.

Only the **returns** are deflated. Market gain and net paid in stay nominal, and
the page says so: deflating a period's cash flows correctly needs each one
converted on its own date, and converting them from the period's endpoints would
be a precise-looking guess. The money-weighted figure is deflated by the
*annualised* price change, since it is itself already annualised.

The toggle is not offered on YTD — a partial year deflated by a partial year's
price change says considerably less than it looks like it says. Where a span
reaches outside the published index the page shows the nominal figures and
explains why.

## Against the market

Your portfolio's growth, rebased to 100 at the start of the period, optionally
against index benchmarks.

The line is **growth with your own deposits and withdrawals removed**, not your
balance. Plotting the raw balance here would make a portfolio that received a
large deposit appear to beat every benchmark while having underperformed all of
them — the most convincing wrong thing the app could draw.

Benchmark prices are **off by default**. Fetching them is one of only two
outbound requests Ledgermancy makes to a host that is neither Plaid nor your AI
provider — the other is [merchant logos](merchants.md#merchant-imagery) — so it
is opt-in; see
[Benchmark prices](../configuration.md#benchmark-prices-investments-page). With
it off, your own line still plots.

## Allocation

Two breakdowns: **by asset class** and **by tax treatment**.

Asset class comes from your institution and is not always reported. Whatever it
did not classify appears as its own **Unknown** slice, shown muted and pinned
last. It is never folded into the other slices — a 60%-covered portfolio
presented as 100% would make every slice on the chart wrong while making the
chart look complete.

## Account tax treatment

Taxable brokerage, traditional/Roth 401(k), traditional/Roth IRA, 529, HSA,
trust, or other. This is what makes retirement projections meaningful — a 401(k)
and a taxable account cannot be projected the same way.

Ledgermancy **asks rather than guesses**. Where the reported subtype is
unambiguous (a brokerage account, a Roth IRA) the picker is pre-filled with a
suggestion marked *(likely)*, but nothing is stored until you save. Where it is
ambiguous there is no suggestion at all: Plaid reports a Roth 401(k) and a
traditional one identically, and a wrong tag would silently change every
retirement figure built on it.

Untagged accounts group under "Untagged" everywhere rather than being quietly
defaulted, and the prompt to classify them stays on the page until they are.

## Holdings

Every position: security, ticker, account, tax treatment, quantity, last price,
cost basis, value, and gain in dollars and percent. Sortable by value, gain or
name, and exportable to CSV.

An unknown cost basis shows as an em dash, never `$0.00` — and the CSV exports it
as an empty cell, so a spreadsheet summing the column cannot silently treat
"unknown" as zero.

## Fund fees

Annual cost of fund expense ratios across the portfolio.

Today this reports nothing, and says why: Plaid supplies no expense ratio, so
there is no source to compute from. Ledgermancy will not estimate one. A fee
figure covering part of a portfolio and presented as the total is worse than no
figure at all, so the coverage is always disclosed alongside whatever is shown.

## Dividends

Dividends received by month, taken from your institution's investment
transactions rather than the bank feed — a dividend is credited inside the
brokerage account and usually never appears as a bank transaction at all.

Reinvested dividends are included: the money was earned either way, and leaving
them out would understate income for anyone with automatic reinvestment on.
