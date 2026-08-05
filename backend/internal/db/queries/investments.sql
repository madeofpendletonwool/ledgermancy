-- Investment analysis: holdings detail, value history, cash flows, benchmarks
-- and account tax-treatment tagging.
--
-- Every read here carries the same visibility scoping as the rest of the
-- reporting layer, resolved through the account_access view (00053): household
-- membership plus `v.user_id = $2 OR v.is_shared`, so a member's private
-- institution never leaks into a household view. Reaching through plaid_items
-- directly is now wrong as well as duplicated — a manual account has no item,
-- and an inner join through one drops it silently from every figure here.

-- name: ListInvestmentAccounts :many
-- Investment accounts the caller can see, with the tags the Investments page
-- groups by. tax_treatment is NULL until confirmed; the page groups those under
-- "Untagged" rather than guessing.
SELECT
    a.id,
    a.name,
    a.mask,
    a.type,
    a.subtype,
    a.current_balance,
    a.currency,
    a.tax_treatment,
    a.is_managed,
    a.source,
    v.institution_name
FROM accounts a
JOIN account_access v ON v.account_id = a.id
WHERE v.household_id = $1
  AND (v.user_id = $2 OR v.is_shared)
  AND a.is_active
  AND a.type IN ('investment', 'brokerage')
ORDER BY v.institution_name, a.name;

-- name: SetAccountTaxTreatment :one
-- Confirms a classification. Scoped by household through the item's owner so a
-- caller cannot tag an account belonging to somebody else's household, and by
-- the same visibility rule as the read: a private item is not tag-able by
-- another member.
UPDATE accounts a
SET tax_treatment = sqlc.narg('tax_treatment'),
    is_managed    = sqlc.narg('is_managed')
FROM account_access v
WHERE a.id = $1
  AND v.account_id = a.id
  AND v.household_id = $2
  AND (v.user_id = $3 OR v.is_shared)
RETURNING a.id, a.name, a.tax_treatment, a.is_managed;

-- name: ListVisibleHoldingsDetailed :many
-- Every position with the fields the holdings table shows. Unlike
-- ListVisibleHoldings (used by the Net Worth page) this carries the account id
-- and close price so the page can group by account and show a last price.
SELECT
    h.id,
    h.account_id,
    h.quantity,
    h.cost_basis,
    h.institution_price,
    h.institution_value,
    h.currency,
    s.name              AS security_name,
    s.ticker,
    s.type              AS security_type,
    s.close_price,
    s.close_price_as_of,
    s.is_cash_equivalent,
    a.name              AS account_name,
    a.tax_treatment,
    v.institution_name
FROM holdings h
JOIN securities s  ON s.id = h.security_id
JOIN accounts a    ON a.id = h.account_id
JOIN account_access v ON v.account_id = a.id
WHERE v.household_id = $1
  AND (v.user_id = $2 OR v.is_shared)
  AND a.is_active
ORDER BY h.institution_value DESC NULLS LAST;

-- name: UpsertInvestmentTransaction :exec
-- Plaid is authoritative for investment transactions, so a re-fetch of an
-- overlapping window refreshes the row rather than duplicating it.
--
-- The WHERE on the conflict target is not optional. 00053 made the unique index
-- PARTIAL so manual rows, which have no Plaid id, are simply not in it — and
-- Postgres only accepts a partial index as an arbiter when the statement
-- repeats its predicate exactly. Without it this fails outright with "no unique
-- or exclusion constraint matching the ON CONFLICT specification", which is the
-- good outcome: the alternative would have been a silent duplicate per sync.
INSERT INTO investment_transactions (
    account_id, security_id, plaid_investment_transaction_id,
    type, subtype, amount, quantity, price, fees, date, name, currency, raw
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
ON CONFLICT (plaid_investment_transaction_id)
    WHERE plaid_investment_transaction_id IS NOT NULL
DO UPDATE SET
    account_id  = EXCLUDED.account_id,
    security_id = EXCLUDED.security_id,
    type        = EXCLUDED.type,
    subtype     = EXCLUDED.subtype,
    amount      = EXCLUDED.amount,
    quantity    = EXCLUDED.quantity,
    price       = EXCLUDED.price,
    fees        = EXCLUDED.fees,
    date        = EXCLUDED.date,
    name        = EXCLUDED.name,
    currency    = EXCLUDED.currency,
    raw         = EXCLUDED.raw;

-- name: ListInvestmentTransactionsInRange :many
-- Cash-flow input for the return calculations. Deliberately unfiltered by
-- type/subtype: which of these count as an *external* flow is a judgement made
-- once in Go (reporting.IsExternalFlow) and tested there, rather than encoded
-- twice — in a WHERE clause and in a comment — where the two can drift.
SELECT
    t.id,
    t.account_id,
    t.date,
    t.type,
    t.subtype,
    t.amount,
    t.name
FROM investment_transactions t
JOIN accounts a    ON a.id = t.account_id
JOIN account_access v ON v.account_id = a.id
WHERE v.household_id = $1
  AND (v.user_id = $2 OR v.is_shared)
  AND a.is_active
  AND t.date >= $3
  AND t.date <= $4
ORDER BY t.date, t.id;

-- name: UpsertInvestmentSnapshot :exec
-- One row per account per day. Re-running the job on the same day replaces that
-- day's value rather than adding a second point, which is what makes the daily
-- job safe to run on start and on every sync.
INSERT INTO investment_snapshots (account_id, as_of, market_value, cost_basis)
VALUES ($1, COALESCE(sqlc.narg('as_of')::date, CURRENT_DATE), $2, $3)
ON CONFLICT (account_id, as_of) DO UPDATE SET
    market_value = EXCLUDED.market_value,
    cost_basis   = EXCLUDED.cost_basis;

-- name: ListInvestmentAccountValues :many
-- What each visible investment account is worth right now, plus the cost basis
-- its holdings report. Feeds the snapshot job.
--
-- market_value prefers the account balance because it includes uninvested cash
-- that summing holdings would miss — the same choice ComputeNetWorth makes.
-- cost_basis sums only holdings that report one, so it is NULL when none does.
SELECT
    a.id AS account_id,
    COALESCE(
        a.current_balance,
        (SELECT SUM(h.institution_value) FROM holdings h WHERE h.account_id = a.id),
        0
    )::numeric AS market_value,
    COALESCE(
        (SELECT SUM(h.cost_basis) FROM holdings h WHERE h.account_id = a.id), 0
    )::numeric AS cost_basis,
    -- How many holdings actually reported a basis. Zero means the sum above is
    -- an artefact of COALESCE and must be stored as NULL, not as $0.00 —
    -- "unknown basis" and "zero basis" are very different claims.
    (SELECT COUNT(h.cost_basis) FROM holdings h WHERE h.account_id = a.id)::bigint
        AS basis_holdings
FROM accounts a
WHERE a.is_active
  AND a.type IN ('investment', 'brokerage');

-- name: ListInvestmentSnapshots :many
-- The household's investment value history, summed across visible accounts per
-- day. Chain-linking a return needs one series, and summing in SQL keeps the
-- arithmetic out of Go.
--
-- Only days on which EVERY account that has any history reported a value would
-- be strictly correct; in practice the snapshot job writes all accounts in one
-- pass, so a day is either complete or absent. A partial day (an account linked
-- mid-history) is handled by the caller skipping sub-periods whose base value is
-- not positive.
SELECT
    s.as_of,
    SUM(s.market_value)::numeric               AS market_value,
    COALESCE(SUM(s.cost_basis), 0)::numeric    AS cost_basis,
    COUNT(s.cost_basis)::bigint                AS basis_accounts,
    COUNT(*)::bigint                           AS account_count
FROM investment_snapshots s
JOIN accounts a    ON a.id = s.account_id
JOIN account_access v ON v.account_id = a.id
WHERE v.household_id = $1
  AND (v.user_id = $2 OR v.is_shared)
  AND a.is_active
  AND s.as_of >= $3
  AND s.as_of <= $4
GROUP BY s.as_of
ORDER BY s.as_of;

-- name: ListInvestmentSnapshotsForAccount :many
SELECT s.as_of, s.market_value, s.cost_basis
FROM investment_snapshots s
JOIN accounts a    ON a.id = s.account_id
JOIN account_access v ON v.account_id = a.id
WHERE v.household_id = $1
  AND (v.user_id = $2 OR v.is_shared)
  AND s.account_id = $3
  AND s.as_of >= $4
  AND s.as_of <= $5
ORDER BY s.as_of;

-- name: GetEarliestInvestmentSnapshot :one
-- The day the app started watching. Used to clamp a requested period to the
-- history that actually exists, so a "5 year" request on a two-week-old install
-- reports two weeks and says so.
SELECT MIN(s.as_of)::date AS as_of
FROM investment_snapshots s
JOIN accounts a    ON a.id = s.account_id
JOIN account_access v ON v.account_id = a.id
WHERE v.household_id = $1
  AND (v.user_id = $2 OR v.is_shared)
  AND a.is_active
-- MIN over an empty set is one row containing NULL, which cannot be scanned
-- into a date. The HAVING turns "no history" into no rows, so the caller sees
-- pgx.ErrNoRows — a condition it already has to handle.
HAVING MIN(s.as_of) IS NOT NULL;

-- name: UpsertAssetPrice :exec
INSERT INTO asset_prices (ticker, as_of, close)
VALUES ($1, $2, $3)
ON CONFLICT (ticker, as_of) DO UPDATE SET close = EXCLUDED.close;

-- name: ListAssetPrices :many
SELECT ticker, as_of, close
FROM asset_prices
WHERE ticker = ANY($1::text[])
  AND as_of >= $2
  AND as_of <= $3
ORDER BY ticker, as_of;

-- name: GetDividendIncome :many
-- Dividends as an income stream, by month.
--
-- Source is investment_transactions, NOT the ordinary categorised feed: a
-- dividend is credited inside the brokerage account, so it never appears as a
-- bank transaction unless it is swept out, and there is no 'dividends' category
-- in the seed to catch it if it did. Plaid's sign convention has cash credits
-- negative, so the sum is negated to read as positive income.
--
-- Reinvested dividends are included: the money was earned either way, and
-- excluding them would understate income for anyone with DRIP on.
SELECT
    date_trunc('month', t.date)::date AS month,
    (-SUM(t.amount))::numeric         AS total
FROM investment_transactions t
JOIN accounts a    ON a.id = t.account_id
JOIN account_access v ON v.account_id = a.id
WHERE v.household_id = $1
  AND (v.user_id = $2 OR v.is_shared)
  AND a.is_active
  AND t.subtype IN (
      'dividend', 'qualified dividend', 'non-qualified dividend',
      'dividend reinvestment'
  )
  AND t.date >= $3
  AND t.date <= $4
GROUP BY 1
ORDER BY 1;
