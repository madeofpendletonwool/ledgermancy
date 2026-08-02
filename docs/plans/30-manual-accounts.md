# 30 — Manual accounts, manual investments, scheduled transactions

## Context

Some institutions either cannot be linked through Plaid at all (TreasuryDirect,
private equity, manual bookkeeping for a small business) or fail every link/sync
attempt on the Plaid side and are not going to be fixed — Voya retirement
accounts are the live example. Today that means the account is invisible: it has
no `plaid_item_id` and the `accounts` table requires one (`00001:99`), so it
cannot exist, and neither can its holdings, investment transactions, or
contribution activity.

Doc 12 closed this gap for **transactions** (a manual row attaches to an
existing Plaid-linked account) and explicitly deferred manual accounts. Doc 13
built a forward-looking **bill calendar** (`recurring_obligations`) but
deliberately never posts transactions. Doc 26 added **history and curves** to
the flat `manual_assets` table but stayed inside the net-worth surface — no
transaction feed, no holdings.

This doc closes the remaining gap end-to-end: a user can create an account
without Plaid, post transactions to it, model per-holding investment positions,
and have a fixed monthly contribution (the Voya case) auto-post as a transaction
**and** auto-adjust the manual balance. The goal is full feature parity with a
Plaid-linked investment account on the Investments page — TWR/MWR, allocation,
dividends, fee drag, snapshots, performance vs benchmark — so a manual Voya
account is indistinguishable from a working Plaid one in every report.

## AI vs deterministic split

**No AI.** This is schema + CRUD + a periodic worker + decimal arithmetic. The
TWR/IRR/allocation engines already exist and consume the same tables; manual
rows simply feed them. (Existing AI features — insights, digest — see manual
rows as ordinary data, same as doc 12.)

## Prerequisites

- **Doc 12** (manual transactions) — shipped. The `transactions.source='manual'`
  path, the duplicate guard, and the household-scoped manual-CRUD queries are
  reused verbatim.
- **Doc 14** (investments page) — shipped. Performance, allocation, dividends,
  fee drag, snapshots, and tax-treatment tagging are reused unchanged; the
  engines are designed to read tables, not Plaid.
- **Doc 13** (recurring obligations) — shipped. The
  `(interval_count, interval_unit, anchor_date, end_date)` cadence model and
  the visibility predicate pattern are reused; this doc *adds* an auto-post
  capability 13 deliberately omitted.
- **Doc 26** (real-asset revaluation) — not yet implemented but pattern-aligned.
  The "authoritative current column + append-only history written atomically"
  shape is reused here for manual balances. The two docs touch disjoint columns
  of `manual_assets`/`accounts`; no migration collision.

Migration **`00047_manual_accounts.sql`** is reserved (latest applied is
`00046`). Confirm it is still above the highest applied version before writing.

## Data model

The defining decision is to **reuse the existing tables** by relaxing Plaid-only
constraints, rather than create parallel manual-account/manual-holding tables.
This mirrors doc 12's approach (`source` column, NULL Plaid IDs) and is what lets
every existing report query "just work" — it already filters on visibility and
`is_active`, not on Plaid identity.

### `accounts` — relax Plaid linkage, add ownership for manual rows

```sql
ALTER TABLE accounts ALTER COLUMN plaid_item_id     DROP NOT NULL;
ALTER TABLE accounts ALTER COLUMN plaid_account_id  DROP NOT NULL;
DROP INDEX IF EXISTS accounts_plaid_account_id_key;
CREATE UNIQUE INDEX accounts_plaid_account_id_key
    ON accounts (plaid_account_id) WHERE plaid_account_id IS NOT NULL;

ALTER TABLE accounts
    ADD COLUMN source     TEXT    NOT NULL DEFAULT 'plaid'
        CHECK (source IN ('plaid','manual')),
    ADD COLUMN user_id    UUID    REFERENCES users (id) ON DELETE SET NULL,
    ADD COLUMN is_shared  BOOLEAN NOT NULL DEFAULT TRUE,
    ADD COLUMN household_id UUID  REFERENCES households (id) ON DELETE CASCADE;

ALTER TABLE accounts
    ADD CONSTRAINT accounts_manual_owner
        CHECK (source <> 'manual' OR (plaid_item_id IS NULL AND user_id IS NOT NULL)),
    ADD CONSTRAINT accounts_plaid_owner
        CHECK (source <> 'plaid' OR plaid_item_id IS NOT NULL);

CREATE INDEX accounts_user_id_idx      ON accounts (user_id) WHERE source = 'manual';
CREATE INDEX accounts_household_id_idx ON accounts (household_id);

UPDATE accounts a
   SET household_id = u.household_id,
       user_id     = i.user_id,
       is_shared   = i.is_shared
  FROM plaid_items i JOIN users u ON u.id = i.user_id
 WHERE a.plaid_item_id = i.id;
```

**Why `household_id` on `accounts`?** Today every read query scopes via
`JOIN plaid_items i ON i.id = a.plaid_item_id JOIN users u ON u.id = i.user_id`.
Manual accounts have no `plaid_items` row. Rather than rewrite every query to
LEFT JOIN through a manual-items table, we denormalise the (constant-per-instance)
household and the owning user onto `accounts` itself. Backfill populates Plaid
accounts from their item, so existing queries can be migrated gradually; mixed
visibility becomes `COALESCE(a.user_id, i.user_id)` over a LEFT JOIN.

**Balance independence still holds** for *Plaid-linked* accounts
(`accounts.current_balance` stays Plaid-owned — see doc 12). For *manual*
accounts, this doc establishes the **first user-owned balance-write path**
(see `account_balance_history` below).

### `account_balance_history` — append-only manual balance trail

Mirrors doc 26's `asset_valuations` shape exactly: authoritative current column
plus an append-only history, written in one transaction.

```sql
CREATE TABLE account_balance_history (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id  UUID        NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    as_of       DATE        NOT NULL,
    balance     NUMERIC(20,4) NOT NULL,
    -- manual | scheduled | holding_revalue | fee | dividend
    -- "scheduled" is the traceability link for auto-posted contributions.
    reason      TEXT        NOT NULL DEFAULT 'manual',
    note        TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (account_id, as_of)
);
CREATE INDEX account_balance_history_account_idx
    ON account_balance_history (account_id, as_of DESC);
```

A balance write is `UPDATE accounts.current_balance` + `INSERT account_balance_history`
in one transaction. Re-running the same `as_of` updates in place (matches
`UpsertInvestmentSnapshot`'s idempotency pattern).

### `securities` — relax Plaid-only identity

```sql
ALTER TABLE securities ALTER COLUMN plaid_security_id DROP NOT NULL;
DROP INDEX IF EXISTS securities_plaid_security_id_key;
CREATE UNIQUE INDEX securities_plaid_security_id_key
    ON securities (plaid_security_id) WHERE plaid_security_id IS NOT NULL;
ALTER TABLE securities
    ADD COLUMN source TEXT NOT NULL DEFAULT 'plaid'
        CHECK (source IN ('plaid','manual')),
    ADD COLUMN ticker_key TEXT;  -- lowercased ticker, for manual price match
CREATE UNIQUE INDEX securities_manual_ticker_key
    ON securities (ticker_key) WHERE source = 'manual';
```

Manual securities key on `ticker_key` (lowercased ticker) instead of
`plaid_security_id`. A partial unique index prevents duplicate manual entries of
the same ticker — two "VTI" rows would be wrong. Plaid securities keep their
existing unique constraint where the ID is non-null.

### `holdings` — already manual-ready

`holdings` (`00004:32-48`) has no Plaid-specific NOT NULL columns beyond
`account_id` and `security_id`. Both are satisfied by the relaxed `accounts`
and `securities` above. No schema change. `UpsertHolding` keys on
`UNIQUE (account_id, security_id)`, which works identically for manual rows.

### `investment_transactions` — relax Plaid-only identity

```sql
ALTER TABLE investment_transactions ALTER COLUMN plaid_investment_transaction_id
    DROP NOT NULL;
DROP INDEX IF EXISTS investment_transactions_plaid_investment_transaction_id_key;
CREATE UNIQUE INDEX investment_transactions_plaid_investment_transaction_id_key
    ON investment_transactions (plaid_investment_transaction_id)
    WHERE plaid_investment_transaction_id IS NOT NULL;
ALTER TABLE investment_transactions
    ADD COLUMN source TEXT NOT NULL DEFAULT 'plaid'
        CHECK (source IN ('plaid','manual','scheduled'));
```

`scheduled` distinguishes an auto-posted contribution from a hand-entered one.
`reporting.IsExternalFlow` (`returns.go:55`) already classifies by `type`/
`subtype`, not by source, so manual rows feed TWR/MWR unchanged.

### `recurring_obligations` — add auto-post capability (doc 13 + 1 column)

Doc 13's table is *almost* what we need. Add three columns:

```sql
ALTER TABLE recurring_obligations
    ADD COLUMN auto_post           BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN last_posted_date    DATE,
    ADD COLUMN posting_account_id  UUID REFERENCES accounts (id) ON DELETE SET NULL;
```

- `auto_post` gates whether the new worker materialises occurrences into
  `transactions`.
- `last_posted_date` is the idempotency cursor — the worker only posts
  occurrences strictly after this date. (The cadence model stays derived —
  `anchor_date + interval` is still the source of truth for *when*; this just
  records *what has been posted*.) Pattern matches
  `plaid_items.last_transaction_cursor`.
- `posting_account_id` is independent of `account_id` (`00019:42`) — the latter
  is the bill's *source* account (for projected-balance in doc 13); the former
  is the account a scheduled contribution should credit. NULL means "use
  `account_id`" (the default for bill payment).

### No new table for scheduled transactions

A scheduled post is a row in `transactions` with `source='scheduled'`
(or `source='manual'` if you prefer to fold them — see Backend). The obligation
*template* lives in `recurring_obligations`; the *materialised rows* are
ordinary transactions. This avoids a parallel transaction-shaped table and
gives the investments/spending/budgets engines the data for free.

## Continuity

Every new table classified in `backend/internal/continuity/coverage.go`:

| Table | Category | Rationale |
|---|---|---|
| `account_balance_history` | `InExport` | User-entered balance trail; not re-derivable |
| `account_balance_history` money column | (invariant) | Cast to text in export SQL |
| `recurring_obligations.auto_post` etc. | (existing table) | `recurring_obligations` is already `InExport` |
| New `accounts.*` columns | (existing table) | `accounts` is already `InExport` |
| New `securities`/`holdings`/`investment_transactions` columns | (existing) | All already `InExport` |

No new blob stores or volumes. Existing `account_balance_history` rows include
money and must survive the export's text-cast invariant — write the export
update alongside the table.

## Backend

### Queries — new file `db/queries/manual_accounts.sql`

All household-scoped. For mixed visibility (manual + Plaid in one query),
use the LEFT JOIN + COALESCE pattern documented at the top of the file:

```sql
-- The visibility predicate that works for both source='plaid' and source='manual':
-- COALESCE(a.user_id, i.user_id) is the owner; the household is constant per
-- instance but stored redundantly on a for manual rows.
```

- `CreateManualAccount` — insert with `source='manual'`, caller-supplied
  `user_id`/`is_shared`/`household_id`, `name`, `type`, `subtype`, `mask`,
  `currency`, `tax_treatment`, `current_balance` (nullable). Verify
  `household_id` matches the caller's.
- `UpdateManualAccount` — `WHERE id=$ AND source='manual' AND <household>`
  so a Plaid account can never be edited through this path.
- `SetManualAccountBalance(account_id, as_of, balance, reason, note)` —
  `UPDATE accounts.current_balance` + `INSERT account_balance_history` in a
  `pgx.BeginTx` block. Unique on `(account_id, as_of)` → re-run updates in place.
- `DeleteManualAccount` — `WHERE source='manual'` only; Plaid ones still go
  through `/api/plaid/items/{id}`.
- `CreateManualSecurity` — insert with `source='manual'`, lowercased `ticker_key`,
  caller-supplied name/type/cusip/isin/close_price.
- `UpsertManualHolding` — reuses the existing `UNIQUE (account_id, security_id)`
  conflict target.
- `CreateManualInvestmentTransaction` — for buys/sells/cash/fee/transfer;
  caller supplies `type`/`subtype` from the same enum Plaid uses
  (`reporting.externalFlowSubtypes`, `returns.go:39-47`).
- `ListInvestmentAccountValues` (`investments.sql:134-158`) — **fix the existing
  household-scoping gap** (it currently snapshots every investment account
  globally). Add the same LEFT JOIN + COALESCE so manual accounts are included
  *and* visibility is enforced. Without this fix, a manual Voya account would
  either be invisible to the snapshot worker or leak across households.
- `ListInvestmentAccounts`, `ListVisibleHoldingsDetailed`,
  `ListInvestmentSnapshots*`, `GetEarliestInvestmentSnapshot`,
  `ListInvestmentTransactionsInRange` — change `JOIN plaid_items` to
  `LEFT JOIN plaid_items` and use `COALESCE(a.user_id, i.user_id)`. The column
  sets don't change; existing callers see the same shape.

### Handlers — new file `internal/api/manual_account_handlers.go`

Routes (extend the existing `/accounts` group, `server.go:319`):
```
POST   /api/accounts                   s.handleCreateManualAccount
PUT    /api/accounts/{accountID}       s.handleUpdateManualAccount       // manual only
DELETE /api/accounts/{accountID}       s.handleDeleteManualAccount       // manual only
PUT    /api/accounts/{accountID}/balance  s.handleSetManualBalance       // manual only

POST   /api/securities                 s.handleCreateManualSecurity
GET    /api/securities                 s.handleListSecurities            // for picker

POST   /api/accounts/{accountID}/holdings        s.handleUpsertManualHolding
DELETE /api/holdings/{holdingID}                  s.handleDeleteManualHolding  // manual only

POST   /api/investment-transactions              s.handleCreateManualInvestmentTx
DELETE /api/investment-transactions/{id}         s.handleDeleteManualInvestmentTx
```

All mounted on the `authenticate` + `RequireAdult` group, mirroring the
investment routes at `server.go:478-489`. Existing investment read endpoints
(`/api/investments/*`) need **no handler change** — they call the same queries,
which now return manual rows too.

### Tax-treatment validation — fix the latent 9-vs-13 bug

`validTaxTreatments` at `investment_handlers.go:607-611` lists 9 values; the
DB CHECK (`00034`) and frontend (`Investments.tsx:24-38`) both have 13. Manual
account creation will surface this immediately (a user creating a UTMA cannot
save the right tag). Replace the map with one generated from the same source
as `networth.IsCustodial`/`SuggestTaxTreatment`, so the three locations cannot
drift again.

### Job — new `internal/jobs/scheduled_transactions.go`

`PostScheduledTransactionsWorker`, registered as a periodic job at 15-minute
interval (matches `post_allowances` cadence). Pseudocode:

```
for each obligation where auto_post AND is_active
    AND (last_posted_date IS NULL OR last_posted_date < today)
    AND (end_date IS NULL OR end_date >= today):
    occurrences = dates_between(max(anchor_date, last_posted_date+1),
                                today, interval_count, interval_unit)
    if empty: continue
    begin tx
        for each occurrence_date in occurrences:
            insert into transactions (
                account_id = posting_account_id ?? account_id,
                date       = occurrence_date,
                amount     = amount,  // positive = money out (Plaid convention)
                name       = label,
                category_id, source = 'scheduled',
                category_source = 'manual',
                merchant_key = MerchantKey(label, label)
            )
            if posting_account is type IN ('investment','brokerage'):
                // contribution = external flow into the portfolio; the balance
                // grows by the contribution. Sign follows doc 26's atomic-write
                // rule: same inputs → same result.
                insert into investment_transactions (
                    account_id, type='cash',
                    subtype='contribution',  // IsExternalFlow returns true
                    amount = -amount,        // sign convention: into portfolio
                    date   = occurrence_date,
                    source = 'scheduled'
                )
                insert into account_balance_history (
                    account_id, as_of=occurrence_date,
                    balance = current_balance + amount,  // atomic with the UPDATE
                    reason  = 'scheduled',
                    note    = label
                )
                update accounts set current_balance = current_balance + amount
                    where id = posting_account_id
        update recurring_obligations set last_posted_date = today
            where id = obligation.id
    commit tx
```

**Critical rules:**

- **All occurrences for an obligation post in one transaction.** A failure
  leaves neither the transactions nor the cursor advance — same atomicity rule
  as doc 26's `asset_valuations`.
- **`last_posted_date` advances only after the commit.** A crash mid-batch
  replays from the same cursor (matches Plaid's transactions-sync cursor
  pattern at `plaid/sync.go:142-147`).
- **Month arithmetic in SQL**, not Go (`time.AddDate` doesn't clamp Jan 31 →
  Feb 28 — see `00019:23-25`). Reuse `ListUpcomingObligations`'s
  interval-addition SQL.
- **Idempotency on re-run.** If the worker runs twice in a day, the second run
  computes an empty occurrence set because `last_posted_date = today`. Belt-
  and-braces: add a partial unique index
  `(transaction_id can be derived from obligation_id + occurrence_date)` — or
  accept the cursor as the sole guard (Plaid sync does; doc 13's detector
  does). Recommend the cursor-only path for parity with existing patterns.
- **Sign conventions** (the easy thing to get wrong):
  - `transactions.amount` is **positive = money out** (Plaid convention). A
    contribution from checking is positive there.
  - `investment_transactions.amount` follows `reporting.ExternalFlows`'s flip:
    a contribution is **negative** there (money into the portfolio), per
    `reporting/investments.go:177-188`.
  - `accounts.current_balance` for the investment account **grows** by the
    contribution amount.
  - For the *source* (checking) account, only a `transactions` row is inserted;
    its balance is unaffected (Plaid owns it, or it's another manual account
    whose balance is updated through its own schedule).

### Snapshot worker — already correct once the query is fixed

`SnapshotInvestmentsWorker` (`jobs/investments.go:36-71`) reads stored data and
is registered unconditionally. Once `ListInvestmentAccountValues` includes
manual accounts (above), the worker snapshots them with no scheduling change.
**The day's `account_balance_history` row is the source of truth** for a manual
account's market value (preferred over `SUM(holdings.institution_value)`, which
only covers positions, not uninvested cash — matches the existing preference at
`investments.go:49-55`).

### Performance reporting — no engine change

`BuildPortfolioPerformance` (`reporting/investments.go:93`) reads
`investment_snapshots` and `investment_transactions`. Both now contain manual
rows. TWR/MWR work without modification. **Backfill is impossible** (no
historical holdings exist before the user starts entering them — same constraint
as Plaid, called out at `Investments.tsx:117-127`); the UI's "based on N days
of history" disclosure covers this.

### Dividends, fees, allocation — no change

`GetDividendIncome` filters by subtype; manual rows with subtype='dividend'
flow through. `ComputeFeeDrag` still has no expense-ratio source (unchanged
from doc 14 — manual holdings face the same disclosure-only fee panel).

## Frontend

### Accounts page (`frontend/src/routes/Accounts.tsx`)

- New **"+ Add account"** button next to `ConnectAccount`. Opens
  `CreateAccountModal` (new, mirrors `ManualTransactionModal` at
  `Transactions.tsx:859`): name, type picker (`depository`/`investment`/
  `brokerage`/`credit`/`loan`/`other`), subtype (free text or picklist by type),
  optional mask, opening balance, currency, `is_shared` toggle.
- Manual-account badge alongside the existing Plaid-status badge. For manual
  accounts, the per-row menu shows **Edit**, **Update balance**, **Delete**
  instead of **Sync now** / **Reconnect**.
- **Update balance** opens a modal: as_of date (default today), new balance,
  reason picker (`manual`/`holding_revalue`/`fee`/`dividend`), optional note.
  The history is read-only in a collapse below.

### Transactions page (`frontend/src/routes/Transactions.tsx`)

- The account picker in `ManualTransactionModal` already lists visible accounts;
  manual accounts appear once the backend returns them. No structural change.
- New **"Scheduled"** badge for `source==='scheduled'` rows; these are
  read-only like Plaid rows (edit by editing the obligation).

### Investments page (`frontend/src/routes/Investments.tsx`)

- The tagging prompt (`AccountTagging` at `:649-796`) currently leans on
  `SuggestTaxTreatment`'s Plaid-subtype inference. For manual investment
  accounts there is no Plaid subtype, so the prompt shows the picker without a
  pre-selected suggestion (label: "Choose a tax treatment").
- **New: Holdings editor.** Per-account "Add holding" affordance: security
  picker (search by ticker over `GET /api/securities`, with "Create new
  security" inline), quantity, cost basis (optional), institution price
  (optional — falls back to `securities.close_price`). Mirrors `DebtTerms`'s
  inline-edit shape.
- **New: Investment transaction editor.** Buy/sell/cash/fee/transfer picker,
  security (optional — cash/fee don't need one), quantity, price, fees, amount,
  date. Use cases: recording a Voya contribution (cash/contribution), a dividend
  (cash/dividend), a buy (buy/with security).

### Schedule page (`frontend/src/routes/Schedule.tsx` from doc 13)

- Add **"Auto-post as transaction"** toggle to the obligation editor. When on,
  reveal a **posting account** picker (the Voya account for contributions;
  defaults to the obligation's existing `account_id`).
- Existing occurrences view gains a small "Scheduled" badge on auto-posted
  dates, with the materialised transaction linked.

### API client (`frontend/src/lib/api.ts`)

New methods following existing conventions:
- `createManualAccount`, `updateManualAccount`, `deleteManualAccount`,
  `setManualAccountBalance`
- `listSecurities`, `createManualSecurity`
- `upsertManualHolding`, `deleteManualHolding`
- `createManualInvestmentTransaction`, `deleteManualInvestmentTransaction`
- `setObligationAutoPost(obligationID, { auto_post, posting_account_id })`

Add to the service-worker cache allowlist (`frontend/src/sw.ts:72`) only if you
want these offline-readable — they're write paths, so default behavior (online
only) is correct.

## AI notes

None. Fully deterministic.

## Verification

Decimal-exact, table-driven. Mirror the invariants in
`backend/internal/db/split_invariants_test.go` (doc 21) and the cadence
edge-cases called out in doc 13.

- **Manual account CRUD:** create a manual `investment` account "Voya"; confirm
  it appears in `/api/accounts`, in `ComputeNetWorth`'s investments bucket
  (`networth.sql:343-352`), and on the Investments page. Edit/delete; confirm
  household scoping (a valid id from another household 404s) and that the
  manual endpoints **refuse a `source='plaid'` id**.
- **Balance atomicity:** call `setManualAccountBalance` twice for the same
  `as_of`; confirm the second updates in place (one `account_balance_history`
  row) and `accounts.current_balance` matches. Confirm a mid-write failure
  leaves neither row (use a test that injects an error between the two
  statements — pattern from doc 26).
- **Manual holding + security:** create VTI manually, add 50 shares @ $100;
  confirm `GET /api/investments/holdings` returns it and the Investments page
  renders the row identically to a Plaid-sourced holding.
- **Investment transaction + performance:** post a `cash`/`contribution` for
  $500; confirm `ExternalFlows` counts it (sign-correct), and that TWR/MWR
  over a window including the contribution match a hand-computed value to the
  cent. Post a `cash`/`dividend` for $10; confirm `GetDividendIncome` includes
  it in the right month.
- **Snapshot inclusion:** after the daily `investment_snapshot` job runs, confirm
  a row exists for the manual account in `investment_snapshots` and that
  `GET /api/investments/performance?period=ytd` returns `computable=true` once
  ≥2 days of history exist.
- **Scheduled worker:**
  - Seed an obligation with `auto_post=true`, `anchor_date` 30 days ago,
    monthly cadence, `posting_account_id` = the Voya account. Run the worker.
    Confirm: one `transactions` row (`source='scheduled'`), one
    `investment_transactions` row (`type='cash'`, `subtype='contribution'`),
    one `account_balance_history` row (`reason='scheduled'`), the Voya
    `current_balance` increased by exactly the amount, and
    `recurring_obligations.last_posted_date` = today. All four writes happened
    or none did (kill the worker mid-test and re-run; confirm idempotency).
  - Month-end cadence (`anchor_date` = Jan 31): confirm Feb gets the 28th, not
    Mar 3 (the SQL clamp, not Go's `time.AddDate`).
  - `end_date` in the past with `auto_post=true`: confirm no posts, no error.
  - A second run on the same day: zero new rows.
- **Visibility scoping:** a manual account private to user A is invisible to
  user B (same household) in `/api/accounts`, `/api/investments/*`, and the
  snapshot worker.
- **Tax-treatment fix:** create a manual account with `tax_treatment='utma_ugma'`
  via the API (previously 400'd); confirm it persists and appears in the
  custodial bucket of `AllocationByTaxTreatment`.
- **Continuity:** `go test ./internal/continuity/...` passes (the build fails
  otherwise — the registry test requires no DB). Confirm
  `account_balance_history` appears in the portable export with `balance` cast
  to text.
- **Build:** `cd backend && go build ./... && go vet ./... && go test -p 1 ./...`
  with `TEST_DATABASE_URL`. Frontend: `tsc -b`, `npm run build`, `npm run lint`.

## Out of scope

- **Auto-fetching prices for manual holdings.** The `asset_prices` table is
  ticker-keyed and `FetchBenchmarksWorker` already pulls from Stooq, but
  extending that to user-held tickers is a *different privacy contract* than
  the operator-configured benchmark set (the README's "phones home to nothing
  but Plaid" line depends on user-driven outbound traffic being opt-in).
  Manual price entry is the default; an `ASSET_PRICES_ENABLED`-style switch for
  user tickers is a separate doc.
- **CSV import of holdings.** A bulk-import path for "I have 30 positions"
  is real work (column mapping, security matching) and lives in a follow-up.
  One-at-a-time entry covers the Voya case.
- **Manual account ↔ Plaid reconciliation.** If Voya ever starts working
  through Plaid, there is no merge path. The user deletes the manual account
  and links the Plaid one; transaction history is preserved (manual rows
  stay), balance history is not.
- **Tax-lot tracking and realised gains.** Doc 14 explicitly deferred this;
  manual holdings inherit the same limitation.
- **Manual liabilities product.** A manual *credit* or *loan* account works
  through this schema (debt terms at `account_terms` already key on
  `account_id`), but no `liabilities`-mirror equivalent is added — the user
  enters balance and APR directly. Documented as a known gap.
- **Cancelling or skipping a scheduled occurrence.** The first cut posts every
  due occurrence. A "skip this one" affordance is small but deferred — the
  user can delete the resulting transaction row.
