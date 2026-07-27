# Transactions

![Transactions](../screenshots/transactions.png)
<em>Everything Ledgermancy has pulled in, newest first.</em>

Transactions is the ledger: everything the app has pulled in from Plaid plus
anything you've added by hand, newest first.

## Filtering

Filters live in the **URL**, so the Dashboard and Spending charts can deep-link
into a filtered view (one day, one category) and the browser back button
restores it. There is no separate local filter state to keep in sync.

| Filter | Notes |
| --- | --- |
| **From / To** | Defaults to a rolling year |
| **Accounts** | Multi-select, grouped by institution. Empty = all accounts. |
| **Category** | Single-select, includes all categories |
| **Needs a category** | Show only uncategorised rows — handy for a cleanup pass |

Changing any filter (except an explicit page move) resets to page 0, so a new
filter never lands you past the end of a now-shorter result set. Paging keeps
the previous page on screen while the next loads.

## Recategorising inline

Click a row's category chip to recategorise it inline. The dropdown shows the
app's resolved category (falling back to "Uncategorised"), not Plaid's raw
guess.

Tick **All from {merchant}** to both remember the choice for future syncs **and**
retroactively fix every existing charge from that merchant — handled
server-side. This is the fastest way to clean up a mis-categorised merchant.

!!! info "Manual choices are sticky"
    A choice you make is marked `category_source = 'manual'` and is preserved by
    the sync upsert — Plaid can never overwrite it. See
    [Concepts → Categorisation order](../concepts.md#categorisation-order).

## Add transaction

Use **Add transaction** to reconcile a charge your bank feed missed. A modal
collects account, date, amount, an Expense / Income-refund toggle (which sets
the sign), merchant/description, category, and notes.

> A manual transaction corrects your **spending totals only** — it never changes
> an account balance.

Manual rows are badged `MANUAL` and can be edited or deleted; Plaid rows stay
read-only except for category. If a manual row matches a synced charge that
later arrives, a **Possible duplicate** notice appears with a one-click delete.

## Importing a CSV

![Import CSV](../screenshots/transactions.png)

**Import CSV** opens a generic importer for backfilling history older than
Plaid's window, or for institutions that cap history. It:

- Lets you **map your bank's columns** to the fields the app needs.
- Accepts a **single signed amount** column or **separate debit/credit**
  columns.
- **De-duplicates** against synced data so you don't double-count overlapping
  ranges.
- Runs imported rows through the **same categoriser** as synced transactions.

Pick the account the CSV belongs to, map columns, preview, and confirm.
