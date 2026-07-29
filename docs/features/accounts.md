# Accounts & Plaid

![Accounts](../screenshots/accounts.png)
<em>Connected institutions and the balances they report.</em>

Accounts is where you connect banks through Plaid, see what came in, and control
sharing.

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

## Sandbox credentials

In sandbox, Plaid Link accepts the test credentials `user_good` / `pass_good`.
Sandbox institutions only generate about 90 days of transactions — a fixture
limit, not a backfill limit. See [Getting started](../getting-started.md).
