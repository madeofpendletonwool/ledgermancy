# Accounts & Plaid

![Accounts](../screenshots/accounts.png)
<em>Connected institutions and the balances they report.</em>

Accounts is where you connect banks through Plaid, see what came in, and control
sharing. It is also where you add accounts Plaid **cannot** link — see
[Manual accounts](#accounts-without-plaid) below.

## Connecting an account

**Connect an account** opens Plaid Link. On success, Ledgermancy:

1. Exchanges the public token for an access token (encrypted at rest with
   AES-GCM, never returned to the browser).
2. Requests the **730-day maximum** transaction history at link time.
3. Backfills until `has_more` is false, then enters the recurring sync loop
   (driven by webhooks and a worker sweep).

The first account you register creates the household; after that, registration
is invite-only. See [Households](households.md).

## Per institution

Each institution card shows:

- Name and **last synced** time.
- Whether a **backfill is still importing**.
- **Days of history** that landed — with a note if it's under a year (some
  institutions, like Capital One, cap at 90 days and that's expected).
- A **status badge** (`Reconnect required`, etc.) when the item needs attention.

### Controls

- **Shared with household** — toggle per institution. Sharing is per-institution
  so one spouse can keep an account private while everything else rolls up to
  the household. See [Households](households.md).
- **Sync now** — refresh immediately (routine syncs run in the worker).
- **Unlink** — deletes the institution's accounts and transactions (a database
  cascade). A sync summary appears after a manual sync: added / updated /
  removed across N accounts.

!!! danger "Unlinking deletes history"
    Because the cascade removes transactions, unlink is only for when you truly
    want the history gone. There is no undo. See
    [Deployment → What happens after 730 days](../deployment.md#what-happens-after-730-days).

### Reconnect without losing history

When an item's status is `login_required` or `revoked` (credentials changed, MFA
re-prompt, the institution revoked access), **Reconnect** opens Plaid Link in
**update mode**. Update mode repairs the *existing* item in place rather than
creating a new one, so its accounts, its transactions, and the 730-day history
window it was linked with all stay put.

This is the right action whenever an item needs attention — relinking from
scratch instead would orphan the history tied to the old `plaid_item_id`, and
*cannot* re-widen the history window (Plaid fixes that at link time). See
[Deployment → What happens after 730 days](../deployment.md#what-happens-after-730-days).

## Products

Two settings, and the difference between them matters.

| Setting | Meaning |
| --- | --- |
| `PLAID_PRODUCTS` | Products an institution **must** support to appear in Link at all |
| `PLAID_OPTIONAL_PRODUCTS` | Products pulled **where supported**, ignored where not |

!!! danger "Never add `investments` or `liabilities` to `PLAID_PRODUCTS`"
    Plaid narrows the institution list to banks supporting *every* required
    product. Asking for `liabilities` there hides every bank without loan
    products from a user who only wanted their chequing account. Keep
    `PLAID_PRODUCTS=transactions` and put the rest in
    `PLAID_OPTIONAL_PRODUCTS`, which never filters the institution list.

### Enabling a product on accounts you already linked

`PLAID_OPTIONAL_PRODUCTS` is also the switch that turns the Investments and
Liabilities sync modules on, and it applies to **existing** connections. Add a
product, restart, and the next sync pulls it — no relink, no lost history.

This is deliberate. An access token serves whatever the institution supports,
regardless of what was requested when the item was linked, so there is no reason
to make you relink. Relinking would orphan the transaction history tied to the
old item and cannot re-widen the 730-day window.

Each sync module additionally skips any item with no accounts it applies to, so
a chequing-only bank is never asked about its mortgages, and Plaid never bills
for a product that doesn't apply.

## Accounts without Plaid

Some accounts cannot be linked at all — TreasuryDirect, a Voya retirement plan
whose every sync attempt fails, a private holding, the books for a small
business. Until they existed here, they were simply invisible: a manual asset
is a number, not an account, and an account with no institution had nowhere to
live. **Add account** beside *Connect an account* creates one without Plaid.

A manual account has the same shape as a linked one — name, type
(depository / investment / brokerage / credit / loan / other), subtype, optional
mask, currency, opening balance, and a sharing toggle. Once it exists it behaves
like any other account: transactions post to it, it appears in net worth, and a
manual **investment** or **brokerage** account gets full
[Investments](investments.md)-page parity — per-holding positions, buys / sells /
dividends / contributions entered by hand, and TWR/MWR computed over them exactly
as for a linked account.

Two things are different, by design:

- **You own the balance.** A linked account's balance belongs to the institution
  and is overwritten on every sync; a manual account's balance is yours to set,
  and every change is written alongside a dated history row (the figure, the
  date, a reason, an optional note) in one transaction. **Update balance** is on
  the account's menu where *Sync now* sits for a linked one, and the history is
  the audit trail.
- **The menu edits instead of syncing.** A manual account's row offers **Edit**,
  **Update balance**, and **Delete** — there is no *Reconnect*, because there is
  no link to repair. Deleting removes the account and its transactions; the
  manual write endpoints refuse a linked account's id outright, so a Plaid
  account can never be edited or deleted through this path.

### Auto-posting scheduled transactions

A [scheduled obligation](schedule.md) can be told to **auto-post as a
transaction** — the Voya case, where a fixed monthly contribution should land on
the account and grow its balance without anyone entering it each month. Toggle
*Auto-post* on the obligation and pick the account it should credit. A worker
materialises each due occurrence as an ordinary transaction (badged
**Scheduled**), and for an investment account it also records the contribution
and adjusts the balance in the same write — so the figure and its explanation
move together.

### What there is deliberately no path for

- **Auto-fetching prices for manual holdings.** Manual price entry is the
  default; pulling live quotes for tickers you hold would be a different privacy
  contract than the operator-configured benchmark set, and it is not added here.
- **Merging with a later Plaid link.** If an institution starts working through
  Plaid, the manual account is deleted and the Plaid one linked. Transaction
  history is preserved (manual rows stay); balance history is not.

A manual account private to one member is invisible to the other, under the same
visibility scoping every other account uses.

## Sandbox credentials

In sandbox, Plaid Link accepts the test credentials `user_good` / `pass_good`.
Sandbox institutions only generate about 90 days of transactions — a fixture
limit, not a backfill limit. See [Getting started](../getting-started.md).
