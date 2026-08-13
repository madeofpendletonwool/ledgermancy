-- Manual accounts, manual securities and holdings, and the balance trail
-- behind a manually-maintained balance (doc 30).
--
-- Two rules hold throughout this file and are worth stating once:
--
--   1. Every mutation carries `source = 'manual'` in its WHERE clause. That is
--      what makes it impossible to reach a Plaid-linked account through these
--      endpoints even with a valid id — a Plaid account's name, balance and
--      lifecycle belong to the institution, and the only reason these queries
--      can write accounts.current_balance at all is that manual accounts have
--      no institution to disagree with.
--
--   2. Scoping goes through account_access (00053), never through plaid_items.
--      A manual account has no item; reaching for one returns nothing.

-- --------------------------------------------------------------------------
-- Accounts
-- --------------------------------------------------------------------------

-- name: CreateManualAccount :one
-- The household and owner are taken from the authenticated caller, never from
-- the request body, so there is no id here a caller could point at somebody
-- else's household.
INSERT INTO accounts (
    source, user_id, household_id, is_shared,
    name, type, subtype, mask, currency, tax_treatment, current_balance
) VALUES (
    'manual',
    sqlc.arg('user_id')::uuid, sqlc.arg('household_id')::uuid, sqlc.arg('is_shared')::boolean,
    sqlc.arg('name'), sqlc.arg('type'), sqlc.narg('subtype'), sqlc.narg('mask'),
    sqlc.arg('currency'), sqlc.narg('tax_treatment'), sqlc.narg('current_balance')
)
RETURNING *;

-- name: UpdateManualAccount :one
-- Balance is deliberately absent: it moves only through SetManualAccountBalance,
-- which also writes the history row explaining the move.
UPDATE accounts a
SET name          = sqlc.arg('name'),
    type          = sqlc.arg('type'),
    subtype       = sqlc.narg('subtype'),
    mask          = sqlc.narg('mask'),
    currency      = sqlc.arg('currency'),
    tax_treatment = sqlc.narg('tax_treatment'),
    is_shared     = sqlc.arg('is_shared')::boolean,
    updated_at    = now()
FROM account_access v
WHERE a.id = sqlc.arg('id')
  AND v.account_id = a.id
  AND a.source = 'manual'
  AND v.household_id = sqlc.arg('household_id')
  AND (v.user_id = sqlc.arg('user_id') OR v.is_shared)
RETURNING a.*;

-- name: DeleteManualAccount :execrows
-- Plaid accounts are removed by deleting their item, not here. Cascades take
-- the holdings, investment transactions and balance history with it; the
-- ordinary transactions posted to it go too, which is the honest outcome —
-- they described money in an account that no longer exists.
DELETE FROM accounts a
USING account_access v
WHERE a.id = sqlc.arg('id')
  AND v.account_id = a.id
  AND a.source = 'manual'
  AND v.household_id = sqlc.arg('household_id')
  AND (v.user_id = sqlc.arg('user_id') OR v.is_shared);

-- name: GetVisibleAccount :one
-- One account of either source, resolved through the same predicate as the
-- list. Handlers use it to check an id before acting on it.
SELECT a.*, v.institution_name, v.user_id AS owner_id, v.is_shared AS shared
FROM accounts a
JOIN account_access v ON v.account_id = a.id
WHERE a.id = sqlc.arg('id')
  AND v.household_id = sqlc.arg('household_id')
  AND (v.user_id = sqlc.arg('user_id') OR v.is_shared);

-- name: SetManualAccountBalance :one
-- Half of a balance write. The caller MUST run this in the same transaction as
-- InsertAccountBalanceHistory: a failure has to leave neither, or the balance
-- every net-worth query reads and the history that is supposed to explain it
-- disagree. Same rule as SetManualAssetValue/InsertAssetValuation (00051).
UPDATE accounts a
SET current_balance = sqlc.arg('balance'),
    updated_at      = now()
FROM account_access v
WHERE a.id = sqlc.arg('id')
  AND v.account_id = a.id
  AND a.source = 'manual'
  AND v.household_id = sqlc.arg('household_id')
  AND (v.user_id = sqlc.arg('user_id') OR v.is_shared)
RETURNING a.*;

-- name: InsertAccountBalanceHistory :one
-- The other half. One row per account per day; a second write for the same day
-- replaces it rather than failing, so re-running the scheduled worker and a
-- user correcting a figure they entered an hour ago behave the same way.
INSERT INTO account_balance_history (account_id, as_of, balance, reason, note)
VALUES ($1, $2, $3, $4, sqlc.narg('note'))
ON CONFLICT (account_id, as_of) DO UPDATE SET
    balance = EXCLUDED.balance,
    reason  = EXCLUDED.reason,
    note    = EXCLUDED.note
RETURNING *;

-- name: ListAccountBalanceHistory :many
-- The trend behind one account's current balance. Ascending, because it is
-- drawn as a line. Source-agnostic by design: a manual account's rows are the
-- user's writes, a Plaid account's are the snapshot path's (MAD-119), and the
-- account_access join resolves either — a manual account reaches its owner
-- through accounts.user_id and a Plaid one through its item.
--
-- from / to are OPTIONAL range bounds. Left NULL they return the whole trail
-- (the manual balance editor's existing behaviour, which shows every entry); a
-- caller drawing a chart passes a window so a year of daily Plaid snapshots
-- does not pull the full history every time.
SELECT h.*
FROM account_balance_history h
JOIN account_access v ON v.account_id = h.account_id
WHERE h.account_id = sqlc.arg('account_id')
  AND v.household_id = sqlc.arg('household_id')
  AND (v.user_id = sqlc.arg('user_id') OR v.is_shared)
  AND (sqlc.narg('from')::date IS NULL OR h.as_of >= sqlc.narg('from'))
  AND (sqlc.narg('to')::date IS NULL OR h.as_of <= sqlc.narg('to'))
ORDER BY h.as_of;

-- --------------------------------------------------------------------------
-- Securities
-- --------------------------------------------------------------------------

-- name: ListSecurities :many
-- The security picker's source. Deliberately NOT household-scoped: securities
-- are reference data, not household data — a row says "VTI is an ETF called
-- Vanguard Total Stock Market", which is true for everyone and reveals nothing
-- about who holds it. Holdings are what carry ownership, and those are scoped.
SELECT id, plaid_security_id, name, ticker, type, cusip, isin,
       close_price, close_price_as_of, currency, is_cash_equivalent, source
FROM securities
WHERE sqlc.narg('search')::text IS NULL
   OR ticker ILIKE '%' || sqlc.narg('search') || '%'
   OR name   ILIKE '%' || sqlc.narg('search') || '%'
ORDER BY (ticker IS NULL), ticker, name
LIMIT sqlc.arg('limit');

-- name: CreateManualSecurity :one
-- ticker_key is the lowercased ticker and the manual identity key. The partial
-- unique index on it means a second attempt at the same ticker conflicts rather
-- than creating a duplicate; DO UPDATE refreshes the descriptive fields, which
-- is what a user re-entering "VTI" with a better name means.
INSERT INTO securities (
    source, ticker_key, ticker, name, type, cusip, isin,
    close_price, close_price_as_of, currency, is_cash_equivalent
) VALUES (
    'manual', lower(sqlc.arg('ticker')::text), sqlc.arg('ticker')::text, sqlc.narg('name'),
    sqlc.narg('type'), sqlc.narg('cusip'), sqlc.narg('isin'),
    sqlc.narg('close_price'), sqlc.narg('close_price_as_of'),
    sqlc.arg('currency'), sqlc.arg('is_cash_equivalent')
)
ON CONFLICT (ticker_key) WHERE source = 'manual' DO UPDATE SET
    name              = EXCLUDED.name,
    type              = EXCLUDED.type,
    cusip             = EXCLUDED.cusip,
    isin              = EXCLUDED.isin,
    close_price       = EXCLUDED.close_price,
    close_price_as_of = EXCLUDED.close_price_as_of,
    currency          = EXCLUDED.currency,
    updated_at        = now()
RETURNING *;

-- --------------------------------------------------------------------------
-- Holdings
-- --------------------------------------------------------------------------

-- name: UpsertManualHolding :one
-- Reuses holdings' existing UNIQUE (account_id, security_id), so entering a
-- position twice corrects it rather than double-counting it — the same
-- behaviour a re-sync has for a Plaid holding.
--
-- institution_value is computed here rather than accepted from the caller:
-- quantity × price is arithmetic, and a caller-supplied total that disagreed
-- with its own factors would be a silent error in every allocation figure.
INSERT INTO holdings (
    account_id, security_id, quantity, cost_basis,
    institution_price, institution_value, currency, as_of
)
SELECT
    a.id,
    sqlc.arg('security_id'),
    sqlc.arg('quantity'),
    sqlc.narg('cost_basis'),
    sqlc.narg('institution_price'),
    sqlc.arg('quantity') * COALESCE(
        sqlc.narg('institution_price')::numeric,
        (SELECT s.close_price FROM securities s WHERE s.id = sqlc.arg('security_id')),
        0
    ),
    a.currency,
    sqlc.arg('as_of')
FROM accounts a
JOIN account_access v ON v.account_id = a.id
WHERE a.id = sqlc.arg('account_id')
  AND a.source = 'manual'
  AND v.household_id = sqlc.arg('household_id')
  AND (v.user_id = sqlc.arg('user_id') OR v.is_shared)
ON CONFLICT (account_id, security_id) DO UPDATE SET
    quantity          = EXCLUDED.quantity,
    cost_basis        = EXCLUDED.cost_basis,
    institution_price = EXCLUDED.institution_price,
    institution_value = EXCLUDED.institution_value,
    as_of             = EXCLUDED.as_of,
    updated_at        = now()
RETURNING *;

-- name: DeleteManualHolding :execrows
-- Only positions in a manual account. A Plaid holding deleted here would
-- reappear on the next sync anyway, so refusing is the honest answer.
DELETE FROM holdings h
USING accounts a, account_access v
WHERE h.id = sqlc.arg('id')
  AND a.id = h.account_id
  AND v.account_id = a.id
  AND a.source = 'manual'
  AND v.household_id = sqlc.arg('household_id')
  AND (v.user_id = sqlc.arg('user_id') OR v.is_shared);

-- --------------------------------------------------------------------------
-- Investment transactions
-- --------------------------------------------------------------------------

-- name: CreateManualInvestmentTransaction :one
-- type/subtype come from the same vocabulary Plaid uses, because
-- reporting.IsExternalFlow classifies on them and a value outside that
-- vocabulary would be silently excluded from every return figure rather than
-- rejected. The handler validates against that list before calling this.
INSERT INTO investment_transactions (
    account_id, security_id, source, type, subtype,
    amount, quantity, price, fees, date, name, currency
)
SELECT
    a.id,
    sqlc.narg('security_id'),
    sqlc.arg('source'),
    sqlc.arg('type'),
    sqlc.narg('subtype'),
    sqlc.arg('amount'),
    sqlc.narg('quantity'),
    sqlc.narg('price'),
    sqlc.narg('fees'),
    sqlc.arg('date'),
    sqlc.narg('name'),
    a.currency
FROM accounts a
JOIN account_access v ON v.account_id = a.id
WHERE a.id = sqlc.arg('account_id')
  AND a.source = 'manual'
  AND v.household_id = sqlc.arg('household_id')
  AND (v.user_id = sqlc.arg('user_id') OR v.is_shared)
RETURNING *;

-- name: DeleteManualInvestmentTransaction :execrows
-- source <> 'plaid' rather than = 'manual': a scheduled contribution the worker
-- posted is also the user's to remove, and the obligation that produced it is
-- edited separately.
DELETE FROM investment_transactions t
USING accounts a, account_access v
WHERE t.id = sqlc.arg('id')
  AND a.id = t.account_id
  AND v.account_id = a.id
  AND t.source <> 'plaid'
  AND v.household_id = sqlc.arg('household_id')
  AND (v.user_id = sqlc.arg('user_id') OR v.is_shared);

-- name: ListInvestmentTransactionsForAccount :many
-- The editor's list view: what has been recorded against one account.
SELECT t.id, t.account_id, t.security_id, t.source, t.type, t.subtype,
       t.amount, t.quantity, t.price, t.fees, t.date, t.name, t.currency,
       s.ticker, s.name AS security_name
FROM investment_transactions t
JOIN accounts a       ON a.id = t.account_id
JOIN account_access v ON v.account_id = a.id
LEFT JOIN securities s ON s.id = t.security_id
WHERE t.account_id = sqlc.arg('account_id')
  AND v.household_id = sqlc.arg('household_id')
  AND (v.user_id = sqlc.arg('user_id') OR v.is_shared)
ORDER BY t.date DESC, t.id;

-- name: AddToManualAccountBalance :one
-- Moves a manual account's balance by a delta and reports the result, so the
-- caller can write the matching account_balance_history row with the exact
-- figure the account now holds rather than recomputing it and risking a
-- disagreement.
--
-- A delta, not an absolute value, because two contributions posted in the same
-- batch must compose. COALESCE treats a never-set balance as zero, which is
-- what "I have just made my first contribution" means.
--
-- Manual accounts only. The whole point of the Plaid path is that the
-- institution reports the balance; adding to it here would produce a figure the
-- next sync silently overwrites.
UPDATE accounts
SET current_balance = COALESCE(current_balance, 0) + sqlc.arg('delta'),
    updated_at      = now()
WHERE id = sqlc.arg('id')
  AND source = 'manual'
RETURNING current_balance;

-- name: CreateManualInvestmentTransactionUnscoped :one
-- The scheduled-posting worker's variant. It carries no user identity — it is a
-- background sweep across every household, like the snapshot jobs — so it has
-- no caller to scope against. Safe because the account id does not come from a
-- request: it is read from the obligation the worker is already processing,
-- which was itself household-scoped when the user set it.
--
-- Kept deliberately separate from the scoped version rather than made
-- optionally-scoped, so that no request-handling path can ever reach an
-- unscoped insert by passing a NULL user.
INSERT INTO investment_transactions (
    account_id, security_id, source, type, subtype,
    amount, quantity, price, fees, date, name, currency
)
SELECT
    a.id, sqlc.narg('security_id'), sqlc.arg('source'), sqlc.arg('type'),
    sqlc.narg('subtype'), sqlc.arg('amount'), sqlc.narg('quantity'),
    sqlc.narg('price'), sqlc.narg('fees'), sqlc.arg('date'), sqlc.narg('name'),
    a.currency
FROM accounts a
WHERE a.id = sqlc.arg('account_id')
  AND a.source = 'manual'
RETURNING *;
