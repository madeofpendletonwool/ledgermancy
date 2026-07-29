# Dashboard

![Dashboard](../screenshots/dashboard.png)
<em>The Dashboard — your month at a glance.</em>

The Dashboard is the at-a-glance view: this month's spend and pace, where the
money is going, and the latest activity. It links into other pages for the full
breakdown rather than repeating them.

## What you see

- **A proactive feed** of insights — the app noticing things worth a glance
  (spending spikes, budget pace, new recurring charges). It renders nothing
  when there is nothing to flag, so it never leaves an empty box.
- **Due this week** — known bills falling due in the next seven days, with a link
  to the full [Schedule](schedule.md). Nothing renders when there are none.
- **Four stat tiles:** accounts linked, cash & investments, debt, and this
  month's spend (with this month's income as the hint).
- **This month · spend by day** — a bar chart with a pace verdict in the header
  ("$120 more than last month by day 14"). Income, left to invest, and savings
  rate for the month sit underneath, alongside what's **still to come** — the
  known bills left in the month, so the pace reads against what has not cleared
  yet.
- **Top categories** for the month — a teaser into the full Spending breakdown.
- **Top merchants** and **recent transactions** side by side.

If you have no accounts connected yet, the Dashboard shows a **Get started**
panel with a single *Connect an account* button.

## Interactions

- **Click a day bar** to open those transactions filtered to that day.
- **Click a category bar** to open those transactions filtered to that category.
- The pace comparison uses the *previous* calendar month, same day-of-month, so
  "ahead" or "behind" is measured against an equivalent point in time.

## How the pace number works

The headline spend figure is computed server-side in exact decimal. The pace
*hint* (the "$X more/less than last month by day N" line) sums a handful of
already-exact daily totals for a secondary comparison — it is never the basis
for a headline number. See [Concepts → Spending](../concepts.md#spending) for
the full rules.

## Alerts

If a linked institution needs attention (e.g. `login_required`), a banner at the
top tells you to reconnect it. While a backfill is in progress, a separate
banner shows that history is importing.
