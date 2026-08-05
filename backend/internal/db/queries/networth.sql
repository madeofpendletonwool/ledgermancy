-- name: UpsertSecurity :one
--
-- The WHERE on the conflict target is load-bearing: 00053 made this index
-- PARTIAL (manual rows have no Plaid id and are not in it), and Postgres only
-- accepts a partial index as an ON CONFLICT arbiter when the statement repeats
-- its predicate.
INSERT INTO securities (
    plaid_security_id, name, ticker, type, cusip, isin,
    close_price, close_price_as_of, currency, is_cash_equivalent
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (plaid_security_id) WHERE plaid_security_id IS NOT NULL DO UPDATE SET
    name              = EXCLUDED.name,
    ticker            = EXCLUDED.ticker,
    type              = EXCLUDED.type,
    close_price       = EXCLUDED.close_price,
    close_price_as_of = EXCLUDED.close_price_as_of,
    currency          = EXCLUDED.currency,
    is_cash_equivalent = EXCLUDED.is_cash_equivalent
RETURNING *;

-- name: UpsertHolding :exec
INSERT INTO holdings (
    account_id, security_id, quantity, cost_basis,
    institution_price, institution_value, currency, as_of
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (account_id, security_id) DO UPDATE SET
    quantity          = EXCLUDED.quantity,
    cost_basis        = EXCLUDED.cost_basis,
    institution_price = EXCLUDED.institution_price,
    institution_value = EXCLUDED.institution_value,
    currency          = EXCLUDED.currency,
    as_of             = EXCLUDED.as_of;

-- name: DeleteHoldingsNotIn :exec
-- Removes positions the institution no longer reports, so a fully sold holding
-- disappears instead of lingering at its last known value.
DELETE FROM holdings
WHERE account_id = $1 AND NOT (security_id = ANY($2::uuid[]));

-- name: ListVisibleHoldings :many
SELECT
    h.id,
    h.quantity,
    h.cost_basis,
    h.institution_value,
    h.currency,
    s.name    AS security_name,
    s.ticker,
    s.type    AS security_type,
    s.is_cash_equivalent,
    a.name    AS account_name,
    v.institution_name
FROM holdings h
JOIN securities s  ON s.id = h.security_id
JOIN accounts a    ON a.id = h.account_id
JOIN account_access v ON v.account_id = a.id
WHERE v.household_id = $1
  AND (v.user_id = $2 OR v.is_shared)
  AND a.is_active
ORDER BY h.institution_value DESC NULLS LAST;

-- name: UpsertLiability :exec
INSERT INTO liabilities (
    account_id, kind, apr, apr_type, balance, minimum_payment,
    last_payment_amount, last_payment_date, next_payment_due_date,
    origination_date, origination_principal, interest_rate_percentage,
    is_overdue, raw
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
ON CONFLICT (account_id) DO UPDATE SET
    kind                     = EXCLUDED.kind,
    apr                      = EXCLUDED.apr,
    apr_type                 = EXCLUDED.apr_type,
    balance                  = EXCLUDED.balance,
    minimum_payment          = EXCLUDED.minimum_payment,
    last_payment_amount      = EXCLUDED.last_payment_amount,
    last_payment_date        = EXCLUDED.last_payment_date,
    next_payment_due_date    = EXCLUDED.next_payment_due_date,
    origination_date         = EXCLUDED.origination_date,
    origination_principal    = EXCLUDED.origination_principal,
    interest_rate_percentage = EXCLUDED.interest_rate_percentage,
    is_overdue               = EXCLUDED.is_overdue,
    raw                      = EXCLUDED.raw;

-- name: ListVisibleLiabilities :many
-- Every debt account the caller can see, with whatever terms are known for it.
--
-- Account TYPE decides what is a debt — not the presence of a liabilities row.
-- This used to select FROM liabilities, which asked "did Plaid serve loan terms
-- for this?" when the question is "is this a debt?". Plaid serves the Liabilities
-- product for a minority of institutions, so for most households the answer to
-- the first question is no for every debt they have, and this list came back
-- empty: no rows in the Net Worth debt table, none in the printed report, none
-- in the AI goal parser's debt list, and an empty payoff-goal picker.
--
-- The rule here is the same one ComputeNetWorth uses to decide which side of the
-- ledger a balance falls on, and the same one frontend/src/lib/money.ts
-- isLiability() uses. All three must stay in step; a household seeing a debt
-- total above a table that lists no debts is the bug this replaced.
--
-- BALANCE is the ACCOUNT's current balance, never liabilities.balance. The
-- latter is a card's last STATEMENT balance, which disagrees with the Accounts
-- page and with the Liabilities tile sitting directly above the table this
-- feeds. Two balances on one screen are two answers.
--
-- l.kind is deliberately NOT selected. It is NOT NULL on its own table, so under
-- a LEFT JOIN sqlc would generate a non-pointer field and the scan would fail on
-- every account without a liabilities row — which is most of them. The label is
-- derived in Go from subtype/type instead (see debtKindLabel), which also gets
-- 'auto' right: a value liabilities.kind's CHECK does not permit.
SELECT
    a.id   AS account_id,
    a.name AS account_name,
    a.mask,
    a.type,
    a.subtype,
    a.current_balance,
    v.institution_name,
    l.apr,
    l.interest_rate_percentage,
    l.minimum_payment,
    l.next_payment_due_date,
    l.is_overdue,
    t.apr             AS manual_apr,
    t.minimum_payment AS manual_minimum_payment,
    -- The scheduled bill's amount, when one is linked. It outranks
    -- account_terms.minimum_payment so the payment has ONE source of truth:
    -- editing the bill on the schedule page must not leave the payoff maths
    -- quoting a figure the calendar disagrees with.
    o.amount          AS obligation_amount
FROM accounts a
JOIN account_access v ON v.account_id = a.id
LEFT JOIN liabilities l   ON l.account_id = a.id
LEFT JOIN account_terms t ON t.account_id = a.id
LEFT JOIN recurring_obligations o
       ON o.id = t.payment_obligation_id AND o.is_active
WHERE v.household_id = $1
  AND (v.user_id = $2 OR v.is_shared)
  AND a.is_active
  AND a.type IN ('credit', 'loan')
ORDER BY a.current_balance DESC NULLS LAST;

-- name: GetVisibleLiability :one
-- One debt account with its merged terms: the ListVisibleLiabilities projection
-- narrowed to a single account, so a write can answer with the merged result
-- rather than making the client refetch the whole list to see its own edit.
SELECT
    a.id   AS account_id,
    a.name AS account_name,
    a.mask,
    a.type,
    a.subtype,
    a.current_balance,
    v.institution_name,
    l.apr,
    l.interest_rate_percentage,
    l.minimum_payment,
    l.next_payment_due_date,
    l.is_overdue,
    t.apr             AS manual_apr,
    t.minimum_payment AS manual_minimum_payment,
    -- The scheduled bill's amount, when one is linked. It outranks
    -- account_terms.minimum_payment so the payment has ONE source of truth:
    -- editing the bill on the schedule page must not leave the payoff maths
    -- quoting a figure the calendar disagrees with.
    o.amount          AS obligation_amount
FROM accounts a
JOIN account_access v ON v.account_id = a.id
LEFT JOIN liabilities l   ON l.account_id = a.id
LEFT JOIN account_terms t ON t.account_id = a.id
LEFT JOIN recurring_obligations o
       ON o.id = t.payment_obligation_id AND o.is_active
WHERE v.household_id = $1
  AND (v.user_id = $2 OR v.is_shared)
  AND a.is_active
  AND a.type IN ('credit', 'loan')
  AND a.id = $3;

-- name: UpsertAccountTerms :one
-- Records what the household says a debt costs, for the majority of institutions
-- Plaid reports no terms for.
--
-- INSERT ... SELECT with the guard inside the SELECT, so an account in another
-- household, a private item another member linked, an inactive account, or one
-- that is not a debt at all simply produces zero rows and pgx.ErrNoRows. It can
-- never produce a write. Same shape as CreateGoalContribution.
--
-- The visibility rule here is the STRICTER one (mine, or explicitly shared),
-- matching SetAccountTaxTreatment. Reads are household-wide; setting the rate
-- every payoff goal in the household is computed from is not.
INSERT INTO account_terms (account_id, apr, minimum_payment, payment_obligation_id, updated_by)
SELECT a.id,
       sqlc.narg('apr')::numeric,
       sqlc.narg('minimum_payment')::numeric,
       sqlc.narg('payment_obligation_id')::uuid,
       sqlc.arg('updated_by')
FROM accounts a
JOIN account_access v ON v.account_id = a.id
WHERE a.id = sqlc.arg('account_id')
  AND v.household_id = sqlc.arg('household_id')
  AND (v.user_id = sqlc.arg('updated_by') OR v.is_shared)
  AND a.is_active
  AND a.type IN ('credit', 'loan')
ON CONFLICT (account_id) DO UPDATE SET
    apr                   = EXCLUDED.apr,
    minimum_payment       = EXCLUDED.minimum_payment,
    payment_obligation_id = EXCLUDED.payment_obligation_id,
    updated_by            = EXCLUDED.updated_by,
    updated_at            = now()
RETURNING *;

-- name: GetAccountTerms :one
-- The stored terms for one account, used to find an existing payment obligation
-- before deciding whether to create one or edit it in place. Scoped like the
-- write path, not the read path: this answers a question only a writer asks.
SELECT t.*
FROM account_terms t
JOIN accounts a    ON a.id = t.account_id
JOIN account_access v ON v.account_id = a.id
WHERE t.account_id = sqlc.arg('account_id')
  AND v.household_id = sqlc.arg('household_id')
  AND (v.user_id = sqlc.arg('user_id') OR v.is_shared);

-- name: ListNextPaymentDueDates :many
-- The next payment date for every debt whose bill the household has scheduled.
--
-- The occurrence arithmetic is the same as ListUpcomingObligations and is here
-- for the same reason: Postgres interval addition clamps month ends the way a
-- person reads them (2025-01-31 + interval '1 month' = 2025-02-28) and Go's
-- time.AddDate does not. Each occurrence is anchor_date + n whole periods rather
-- than the previous occurrence plus one, so a payment due on the 31st clamps in
-- February and returns to the 31st in March instead of drifting off it forever.
--
-- The 400-day bound covers one period of every unit the cadence permits — the
-- longest is a year — so the earliest occurrence at or after $3 is always inside
-- it. DISTINCT ON then takes exactly that one.
WITH linked AS (
    SELECT t.account_id, o.anchor_date, o.interval_count, o.interval_unit, o.end_date
    FROM account_terms t
    JOIN recurring_obligations o ON o.id = t.payment_obligation_id
    JOIN accounts a    ON a.id = t.account_id
    JOIN account_access v ON v.account_id = a.id
    WHERE v.household_id = $1
      AND (v.user_id = $2 OR v.is_shared)
      AND a.is_active
      AND o.is_active
),
bounded AS (
    SELECT
        linked.*,
        CASE interval_unit
            WHEN 'day'   THEN (($3::date + 400) - anchor_date) / interval_count
            WHEN 'week'  THEN (($3::date + 400) - anchor_date) / (7 * interval_count)
            WHEN 'month' THEN (12 * (EXTRACT(YEAR  FROM $3::date + 400)::int - EXTRACT(YEAR  FROM anchor_date)::int)
                                  + (EXTRACT(MONTH FROM $3::date + 400)::int - EXTRACT(MONTH FROM anchor_date)::int))
                              / interval_count
            WHEN 'year'  THEN (EXTRACT(YEAR FROM $3::date + 400)::int - EXTRACT(YEAR FROM anchor_date)::int)
                              / interval_count
        END AS n_max
    FROM linked
)
SELECT DISTINCT ON (b.account_id)
    b.account_id,
    d.due_date::date AS due_date
FROM bounded b
CROSS JOIN LATERAL generate_series(0, GREATEST(b.n_max, 0)) AS g(n)
CROSS JOIN LATERAL (
    SELECT b.anchor_date + make_interval(
        days   => CASE b.interval_unit
                      WHEN 'day'  THEN g.n * b.interval_count
                      WHEN 'week' THEN g.n * b.interval_count * 7
                      ELSE 0 END,
        months => CASE b.interval_unit WHEN 'month' THEN g.n * b.interval_count ELSE 0 END,
        years  => CASE b.interval_unit WHEN 'year'  THEN g.n * b.interval_count ELSE 0 END
    ) AS due_date
) d
WHERE d.due_date >= $3::date
  AND (b.end_date IS NULL OR d.due_date <= b.end_date)
ORDER BY b.account_id, d.due_date;

-- name: DeleteAccountTerms :exec
-- Clearing both fields removes the row rather than storing an all-NULL one, so
-- "the household has said nothing about this debt" stays a single state rather
-- than two that have to be told apart everywhere downstream. The table's
-- account_terms_not_empty CHECK makes that an invariant rather than a habit.
--
-- Same strict visibility rule as UpsertAccountTerms: clearing someone's terms is
-- a write.
DELETE FROM account_terms t
USING accounts a, account_access v
WHERE t.account_id = a.id
  AND v.account_id = a.id
  AND a.id = sqlc.arg('account_id')
  AND v.household_id = sqlc.arg('household_id')
  AND (v.user_id = sqlc.arg('user_id') OR v.is_shared);

-- name: ListManualAssets :many
SELECT * FROM manual_assets WHERE household_id = $1 ORDER BY is_liability, value DESC;

-- name: CreateManualAsset :one
-- person_id attributes the asset to the person it is held for — savings bonds
-- in a child's name are the case this exists for. NULL for household assets,
-- which is most of them.
INSERT INTO manual_assets (
    household_id, created_by, name, kind, value, is_liability, as_of, notes, person_id
)
VALUES ($1, $2, $3, $4, $5, $6, COALESCE(sqlc.narg('as_of')::date, CURRENT_DATE), $7,
        sqlc.narg('person_id'))
RETURNING *;

-- name: UpdateManualAsset :one
UPDATE manual_assets
SET name = $3, kind = $4, value = $5, is_liability = $6, notes = $7,
    person_id = sqlc.narg('person_id'), as_of = CURRENT_DATE
WHERE id = $1 AND household_id = $2
RETURNING *;

-- name: DeleteManualAsset :exec
DELETE FROM manual_assets WHERE id = $1 AND household_id = $2;

-- name: ComputeNetWorth :one
-- The current whole picture, in one pass.
--
-- Account *type* decides which side of the ledger a balance falls on: credit
-- and loan balances are amounts owed, everything else is held. Investment
-- value comes from the account balance rather than summing holdings, because
-- the balance includes uninvested cash that holdings alone would miss.
WITH visible_accounts AS (
    SELECT a.type, a.current_balance
    FROM accounts a
    JOIN account_access v ON v.account_id = a.id
    WHERE v.household_id = $1
      AND a.is_active
      AND a.current_balance IS NOT NULL
),
manual AS (
    SELECT is_liability, value FROM manual_assets WHERE household_id = $1
)
SELECT
    COALESCE((SELECT SUM(current_balance) FROM visible_accounts
              WHERE type = 'depository'), 0)::numeric AS cash,
    COALESCE((SELECT SUM(current_balance) FROM visible_accounts
              WHERE type IN ('investment', 'brokerage')), 0)::numeric AS investments,
    COALESCE((SELECT SUM(current_balance) FROM visible_accounts
              WHERE type NOT IN ('depository', 'investment', 'brokerage', 'credit', 'loan')), 0)::numeric AS other_assets,
    COALESCE((SELECT SUM(value) FROM manual WHERE NOT is_liability), 0)::numeric AS manual_assets,
    COALESCE((SELECT SUM(current_balance) FROM visible_accounts
              WHERE type = 'credit'), 0)::numeric AS credit_debt,
    COALESCE((SELECT SUM(current_balance) FROM visible_accounts
              WHERE type = 'loan'), 0)::numeric AS loan_debt,
    COALESCE((SELECT SUM(value) FROM manual WHERE is_liability), 0)::numeric AS manual_debt;

-- name: UpsertNetWorthSnapshot :one
INSERT INTO net_worth_snapshots (
    household_id, as_of, assets_total, liabilities_total, net_worth, breakdown
) VALUES ($1, COALESCE(sqlc.narg('as_of')::date, CURRENT_DATE), $2, $3, $4, $5)
ON CONFLICT (household_id, as_of) DO UPDATE SET
    assets_total      = EXCLUDED.assets_total,
    liabilities_total = EXCLUDED.liabilities_total,
    net_worth         = EXCLUDED.net_worth,
    breakdown         = EXCLUDED.breakdown
RETURNING *;

-- name: ListNetWorthSnapshots :many
SELECT * FROM net_worth_snapshots
WHERE household_id = $1 AND as_of >= $2 AND as_of <= $3
ORDER BY as_of;

-- name: GetLatestNetWorthSnapshot :one
SELECT * FROM net_worth_snapshots
WHERE household_id = $1
ORDER BY as_of DESC
LIMIT 1;

-- name: SumManualAssetsByPerson :many
-- Manual assets attributed to a person: savings bonds in a child's name, cash
-- in a birthday envelope. Liabilities are netted off, matching the sign
-- convention ComputeNetWorth uses, so a person's manual total is what they own
-- minus what they owe rather than a gross figure.
-- The whole expression carries one explicit ::numeric cast. Casting each
-- COALESCE separately and subtracting leaves sqlc unable to infer the result
-- type, and it silently emits int32 — which would truncate every asset value
-- to a whole dollar.
SELECT
    m.person_id,
    (COALESCE(SUM(m.value) FILTER (WHERE NOT m.is_liability), 0)
   - COALESCE(SUM(m.value) FILTER (WHERE m.is_liability), 0))::numeric AS total
FROM manual_assets m
WHERE m.household_id = $1 AND m.person_id IS NOT NULL
GROUP BY m.person_id;
