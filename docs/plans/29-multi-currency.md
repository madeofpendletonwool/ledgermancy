# 29 — Multi-currency

*(TODO.md "Next major initiatives" #13. Explicitly far-future — read the
"should you be doing this" section below before starting.)*

## Context

The app hardcodes US dollars. The assistant system prompt says so outright
(`backend/internal/api/chat_handlers.go:43`), and while `accounts.currency` exists
with a `DEFAULT 'USD'` (`00001_core_schema.sql:110`), **nothing reads it**. There
is no currency column on transactions at all, and every aggregate silently
assumes one currency.

For a non-US household this means the numbers are not merely unsupported — they
are *wrong*, because a EUR account and a USD account sum as if they were the same
unit.

The plumbing is already currency-agnostic in the ways that matter: money is
`NUMERIC(20,4)` and decimal strings end to end, never a float. What is missing is
the unit, the conversion, and the presentation.

## Should you be doing this?

TODO #13 marks this far-future for good reason and it is worth restating honestly
before anyone starts: **this touches every aggregation query in the app.** Every
reporting query, the summary, safe-to-spend, net worth, goals, budgets, the
report exports, the chat tools, and every doc in waves 3–5 that added a query.

It is the highest-blast-radius item in the backlog and delivers nothing to a
US-only user. Do it when non-US adoption is a real, present concern — not
speculatively. If it is being done, do it **after** the wave 3–5 docs have
landed, or every one of them adds queries that then need retrofitting.

## AI vs deterministic split

**Deterministic:** every conversion. Rate lookup and multiplication in exact
decimal.

**AI:** the assistant prompt must stop asserting USD and start reasoning about
the household's base currency. It still never computes a conversion — the tools
return converted figures with both amounts present.

## Prerequisites

None technically. Practically: land after waves 3–5, per above.

## Data model

**Reserved migration: `00032_multi_currency.sql`.**

```sql
-- Household base currency: the unit every rollup is expressed in.
ALTER TABLE households ADD COLUMN base_currency TEXT NOT NULL DEFAULT 'USD';

-- Transactions carry their own currency. Existing rows are USD by definition —
-- that is what they were recorded as, even if implicitly.
ALTER TABLE transactions   ADD COLUMN currency TEXT NOT NULL DEFAULT 'USD';
ALTER TABLE manual_assets  ADD COLUMN currency TEXT NOT NULL DEFAULT 'USD';
ALTER TABLE liabilities    ADD COLUMN currency TEXT NOT NULL DEFAULT 'USD';
ALTER TABLE budgets        ADD COLUMN currency TEXT NOT NULL DEFAULT 'USD';
ALTER TABLE goals          ADD COLUMN currency TEXT NOT NULL DEFAULT 'USD';

-- Daily FX rates. Same shape as asset_prices (doc 14) and cpi_series (doc 27).
CREATE TABLE fx_rates (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    base_currency TEXT NOT NULL,
    quote_currency TEXT NOT NULL,
    as_of         DATE NOT NULL,
    rate          NUMERIC(20,10) NOT NULL,   -- wider scale: FX needs the precision
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (base_currency, quote_currency, as_of)
);
CREATE INDEX fx_rates_lookup_idx ON fx_rates (base_currency, quote_currency, as_of DESC);
```

**`NUMERIC(20,10)` for rates, not `(20,4)`.** A JPY or IDR rate needs the scale,
and rounding the *rate* before multiplying compounds error across every figure.

**Backfilling existing rows as USD is a defensible assumption, not a fact** — a
European user running the app today has been entering EUR into a USD-labelled
column. Note it in the migration and surface a one-time prompt to confirm or
correct the base currency after upgrade.

## Backend

### Rate ingestion

A daily job pulling from a free source — `frankfurter.app` (ECB data, no key) or
`exchangerate.host`. Same treatment as every other outbound call in the backlog:
config flag, and the README's "phones home to nothing but Plaid" line must
already be accurate by now.

Gaps are guaranteed — weekends, holidays, a failed fetch. **Carry forward the
most recent prior rate and label the figure as using a rate from that date.**
Never interpolate, and never silently substitute today's date.

### Conversion at aggregation time

The core rule, and the one to write down prominently:

> **Convert at aggregation, never at write.** A transaction is stored in the
> currency it occurred in, forever. Converted figures are computed at read time
> using the rate for the transaction's own date.

Storing a converted amount bakes in one day's rate permanently and makes history
change meaning when rates move. It is the mistake this design exists to avoid.

Responses carry both: the original amount + currency, and the converted amount +
base currency + the rate date used.

**Historical vs. current rate** is a genuine judgement call, so make it
explicitly and document it: a *spending* figure should use the rate at
transaction date (what it actually cost you then); a *balance sheet* figure
should use the current rate (what it is worth now). Mixing them without saying
which is which is how a multi-currency report becomes untrustworthy.

### The sweep

Every aggregation query needs revisiting. Enumerate them — `reports.sql`,
`networth.sql`, `alerts.sql`, plus everything waves 3–5 added — and work the list
rather than fixing surfaces as they are noticed. A partially-converted report is
worse than an unconverted one, because it looks right.

### Assistant

Update the system prompt at `chat_handlers.go:43` to reason about the base
currency. Tool outputs include both amounts so the model never converts.

## Frontend

- Per-account currency badge where accounts are listed.
- Converted totals labelled with base currency and rate date.
- Base-currency setting, with the post-upgrade confirmation prompt.
- Original-currency display on transaction detail — a user who spent €40 wants to
  see €40, not only its converted value.
- `lib/money.ts` gains currency-aware formatting (symbol, placement, decimal
  places — JPY has none).

## Verification

- `go test -p 1 ./...`.
- **The no-op assertion:** a USD-only household's every endpoint returns
  byte-identical output after this lands. Non-negotiable — it is what makes the
  change safe for existing users.
- Conversion against fixture rates, including a zero-decimal currency (JPY) and a
  high-magnitude rate (IDR).
- Rate carry-forward across a weekend gap reports the correct earlier rate date.
- Mixed-currency household: net worth and spending totals against hand-computed
  expected values.
- Rate precision: assert a `(20,10)` rate is not truncated to `(20,4)` anywhere
  in the path.
- Historical-vs-current rate rule holds — a spending figure uses transaction-date
  rates, a balance uses current.
- Frontend `tsc -b`, `vite build`, `oxlint`.

## Out of scope

- Multi-currency Plaid ingestion beyond what Plaid reports per account.
- Crypto as a currency. Different volatility and rate-source problem.
- Hedging, FX gain/loss reporting, or currency-exposure analysis.
- Per-user (rather than per-household) base currency.
