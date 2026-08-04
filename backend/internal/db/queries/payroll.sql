-- Payroll: employers, paystubs and paystub lines.
--
-- Two scoping rules run through every query here, and both differ from the rest
-- of the app in ways that are easy to get wrong by copying a neighbouring file.
--
--   * VISIBILITY IS INVERTED. Everywhere else the default is shared and the
--     opt-out is private (plaid_items.is_shared, documents.is_shared, both
--     DEFAULT TRUE). A paystub is the opposite: private unless its owner said
--     otherwise. The predicate is therefore
--         (p.user_id = @user_id OR p.is_shared)
--     with the household established through the owning user's row. There is no
--     "all_person" escape hatch and no adult override — an adult in the
--     household is not thereby entitled to another adult's salary.
--
--   * CONFIRMED GATES EVERY FIGURE. Any query whose output feeds a reported
--     number filters `confirmed_at IS NOT NULL`. Listing queries do not, because
--     the review queue is exactly the list of unconfirmed stubs. The names say
--     which is which: anything named ...ForYear is a roll-up input and is
--     confirmed-only.
--
-- Money stays NUMERIC throughout. The roll-ups deliberately return ROWS rather
-- than pre-aggregated totals: the reconciliation between a printed year-to-date
-- figure and the sum of the periods on file is a judgement (see
-- payroll.reconcile), and burying it in SQL would put it out of reach of the
-- tests that pin it.

-- --------------------------------------------------------------------------
-- Employers
-- --------------------------------------------------------------------------

-- name: CreateEmployer :one
INSERT INTO employers (household_id, name, ein_encrypted, address, pay_frequency)
VALUES (
    sqlc.arg('household_id'),
    sqlc.arg('name'),
    sqlc.narg('ein_encrypted'),
    sqlc.narg('address'),
    sqlc.arg('pay_frequency')
)
RETURNING *;

-- name: ListEmployers :many
-- The count is scoped by the SAME visibility predicate the paystub queries use,
-- not by employer alone. An employer row is household-wide, so an unscoped
-- count would tell one member how many stubs the other has on file — a small
-- leak, but a leak of exactly the thing this feature keeps private by default.
SELECT
    e.*,
    (SELECT COUNT(*) FROM paystubs p
      WHERE p.employer_id = e.id
        AND (p.user_id = sqlc.arg('user_id') OR p.is_shared))::bigint AS paystub_count
FROM employers e
WHERE e.household_id = sqlc.arg('household_id')
ORDER BY e.name;

-- name: GetEmployer :one
SELECT * FROM employers
WHERE id = sqlc.arg('id') AND household_id = sqlc.arg('household_id');

-- name: UpdateEmployer :one
UPDATE employers
SET name          = sqlc.arg('name'),
    -- NULL clears the stored EIN. The API distinguishes "leave it alone" from
    -- "remove it" before it gets here; a sealed value that cannot be read back
    -- for comparison must not be silently re-sealed on every edit.
    ein_encrypted = sqlc.narg('ein_encrypted'),
    address       = sqlc.narg('address'),
    pay_frequency = sqlc.arg('pay_frequency'),
    updated_at    = now()
WHERE id = sqlc.arg('id') AND household_id = sqlc.arg('household_id')
RETURNING *;

-- name: DeleteEmployer :execrows
-- Cascades to that employer's paystubs and their lines. The API refuses this
-- while stubs exist rather than relying on the cascade: deleting a year of
-- payroll history as a side effect of tidying up an employer list is not
-- something anybody means to do.
DELETE FROM employers
WHERE id = sqlc.arg('id') AND household_id = sqlc.arg('household_id');

-- name: CountEmployerPaystubs :one
SELECT COUNT(*)::bigint FROM paystubs WHERE employer_id = sqlc.arg('employer_id');

-- --------------------------------------------------------------------------
-- Paystubs
-- --------------------------------------------------------------------------

-- name: CreatePaystub :one
INSERT INTO paystubs (
    user_id, employer_id, period_start, period_end, pay_date,
    gross, net, ytd_gross, ytd_net, source, confirmed_at, is_shared, document_id
) VALUES (
    sqlc.arg('user_id'), sqlc.arg('employer_id'),
    sqlc.arg('period_start'), sqlc.arg('period_end'), sqlc.arg('pay_date'),
    sqlc.arg('gross'), sqlc.arg('net'),
    sqlc.narg('ytd_gross'), sqlc.narg('ytd_net'),
    sqlc.arg('source'), sqlc.narg('confirmed_at'),
    sqlc.arg('is_shared'), sqlc.narg('document_id')
)
RETURNING *;

-- name: ListPaystubs :many
-- The paystub list and the review queue, in one query. Unconfirmed rows are
-- included on purpose — reviewing them is the point — and every consumer that
-- reports a FIGURE uses one of the ...ForYear queries below instead.
SELECT
    p.*,
    e.name          AS employer_name,
    e.pay_frequency AS pay_frequency,
    -- The linked deposit, so the list can show the reconciliation state without
    -- a second round trip per row.
    t.date          AS deposit_date,
    t.amount        AS deposit_amount
FROM paystubs p
JOIN employers e ON e.id = p.employer_id
JOIN users     u ON u.id = p.user_id
LEFT JOIN transactions t ON t.id = p.transaction_id
WHERE u.household_id = sqlc.arg('household_id')
  AND (p.user_id = sqlc.arg('user_id') OR p.is_shared)
  AND (NOT sqlc.arg('by_year')::boolean
       OR EXTRACT(YEAR FROM p.pay_date) = sqlc.arg('tax_year')::int)
ORDER BY p.pay_date DESC, p.created_at DESC;

-- name: GetPaystub :one
SELECT
    p.*,
    e.name          AS employer_name,
    e.pay_frequency AS pay_frequency,
    t.date          AS deposit_date,
    t.amount        AS deposit_amount
FROM paystubs p
JOIN employers e ON e.id = p.employer_id
JOIN users     u ON u.id = p.user_id
LEFT JOIN transactions t ON t.id = p.transaction_id
WHERE p.id = sqlc.arg('id')
  AND u.household_id = sqlc.arg('household_id')
  AND (p.user_id = sqlc.arg('user_id') OR p.is_shared);

-- name: GetOwnedPaystub :one
-- The write-path lookup. Deliberately NOT the visibility predicate above:
-- seeing a shared stub is not permission to edit, confirm, delete or re-link
-- it. Every mutation resolves the row through this.
SELECT p.* FROM paystubs p
JOIN users u ON u.id = p.user_id
WHERE p.id = sqlc.arg('id')
  AND p.user_id = sqlc.arg('user_id')
  AND u.household_id = sqlc.arg('household_id');

-- name: UpdatePaystub :one
UPDATE paystubs
SET employer_id  = sqlc.arg('employer_id'),
    period_start = sqlc.arg('period_start'),
    period_end   = sqlc.arg('period_end'),
    pay_date     = sqlc.arg('pay_date'),
    gross        = sqlc.arg('gross'),
    net          = sqlc.arg('net'),
    ytd_gross    = sqlc.narg('ytd_gross'),
    ytd_net      = sqlc.narg('ytd_net'),
    updated_at   = now()
WHERE id = sqlc.arg('id') AND user_id = sqlc.arg('user_id')
RETURNING *;

-- name: SetPaystubConfirmed :one
-- Idempotent by design: re-confirming an already-confirmed stub leaves the
-- original timestamp alone, so an editing round trip does not rewrite when the
-- review happened.
UPDATE paystubs
SET confirmed_at = CASE
        WHEN sqlc.arg('confirmed')::boolean THEN COALESCE(confirmed_at, now())
        ELSE NULL
    END,
    updated_at = now()
WHERE id = sqlc.arg('id') AND user_id = sqlc.arg('user_id')
RETURNING *;

-- name: SetPaystubSharing :one
UPDATE paystubs
SET is_shared = sqlc.arg('is_shared'), updated_at = now()
WHERE id = sqlc.arg('id') AND user_id = sqlc.arg('user_id')
RETURNING *;

-- name: SetPaystubTransaction :one
-- Links a stub to the deposit it produced, or clears the link when the
-- transaction id is NULL. Only ever called from a request in which a human
-- picked the transaction; the matcher never writes.
UPDATE paystubs
SET transaction_id = sqlc.narg('transaction_id'), updated_at = now()
WHERE id = sqlc.arg('id') AND user_id = sqlc.arg('user_id')
RETURNING *;

-- name: DeletePaystub :execrows
DELETE FROM paystubs
WHERE id = sqlc.arg('id') AND user_id = sqlc.arg('user_id');

-- --------------------------------------------------------------------------
-- Paystub lines
-- --------------------------------------------------------------------------

-- name: CreatePaystubLine :one
INSERT INTO paystub_lines (
    paystub_id, category, label, amount, ytd_amount, pre_tax, is_employer
) VALUES (
    sqlc.arg('paystub_id'), sqlc.arg('category'), sqlc.arg('label'),
    sqlc.arg('amount'), sqlc.narg('ytd_amount'),
    sqlc.arg('pre_tax'), sqlc.arg('is_employer')
)
RETURNING *;

-- name: ListPaystubLines :many
SELECT * FROM paystub_lines
WHERE paystub_id = sqlc.arg('paystub_id')
ORDER BY created_at, id;

-- name: DeletePaystubLines :exec
-- The line set is replaced wholesale on edit rather than diffed. A paystub's
-- lines are one indivisible statement about one pay period — a partial update
-- that leaves a stale line behind is a stub that no longer balances, which is
-- the failure the whole feature is built to prevent.
DELETE FROM paystub_lines WHERE paystub_id = sqlc.arg('paystub_id');

-- name: ListPaystubLinesForStubs :many
-- Every line for a set of paystubs, for the roll-ups. One round trip rather
-- than one per stub.
SELECT * FROM paystub_lines
WHERE paystub_id = ANY (sqlc.arg('paystub_ids')::uuid[])
ORDER BY paystub_id, created_at, id;

-- --------------------------------------------------------------------------
-- Roll-up inputs
-- --------------------------------------------------------------------------

-- name: ListConfirmedPaystubsForYear :many
-- CONFIRMED ONLY. This is the query behind every reported figure — savings
-- rate, effective tax rate, contribution headroom, the tax summary — so the
-- inertness of an unconfirmed stub is enforced here, once, rather than in each
-- consumer.
SELECT
    p.*,
    e.name          AS employer_name,
    e.pay_frequency AS pay_frequency
FROM paystubs p
JOIN employers e ON e.id = p.employer_id
JOIN users     u ON u.id = p.user_id
WHERE u.household_id = sqlc.arg('household_id')
  AND (p.user_id = sqlc.arg('user_id') OR p.is_shared)
  AND p.confirmed_at IS NOT NULL
  AND EXTRACT(YEAR FROM p.pay_date) = sqlc.arg('tax_year')::int
ORDER BY p.employer_id, p.pay_date;

-- name: GetConfirmedGrossForPeriod :one
-- Gross pay over an arbitrary window, for the gross-based savings rate.
--
-- It sits ALONGSIDE the net-based figure in reporting/summary.go and does not
-- replace it. The existing savings rate is consumed by the recap, the insight
-- feed and the chat; silently redefining it against a different denominator
-- would change every one of those without a single call site changing.
SELECT
    COALESCE(SUM(p.gross), 0)::numeric AS gross,
    COALESCE(SUM(p.net), 0)::numeric   AS net,
    COUNT(*)::bigint                   AS paystub_count
FROM paystubs p
JOIN users u ON u.id = p.user_id
WHERE u.household_id = sqlc.arg('household_id')
  AND (p.user_id = sqlc.arg('user_id') OR p.is_shared)
  AND p.confirmed_at IS NOT NULL
  AND p.pay_date >= sqlc.arg('from_date')
  AND p.pay_date <= sqlc.arg('to_date');

-- name: CountUnconfirmedPaystubs :one
-- The review queue badge. The caller's OWN stubs only: a review queue is a list
-- of things you can act on, and nobody can confirm somebody else's paystub.
SELECT COUNT(*)::bigint
FROM paystubs p
JOIN users u ON u.id = p.user_id
WHERE u.household_id = sqlc.arg('household_id')
  AND p.user_id = sqlc.arg('user_id')
  AND p.confirmed_at IS NULL;

-- name: ListPaystubYears :many
-- Which tax years have confirmed stubs, newest first — the year picker.
SELECT DISTINCT EXTRACT(YEAR FROM p.pay_date)::int AS tax_year
FROM paystubs p
JOIN users u ON u.id = p.user_id
WHERE u.household_id = sqlc.arg('household_id')
  AND (p.user_id = sqlc.arg('user_id') OR p.is_shared)
  AND p.confirmed_at IS NOT NULL
ORDER BY tax_year DESC;

-- --------------------------------------------------------------------------
-- Deposit reconciliation
-- --------------------------------------------------------------------------

-- name: MatchDepositsForPaystub :many
-- Candidate bank deposits for a stub's net pay. A PROPOSAL, always.
--
-- Deliberately not an exact-amount match. A direct deposit split between
-- checking and savings lands as two rows that each fall short of net, and an
-- exact matcher would silently find nothing for a household that does something
-- extremely ordinary. Ranking by closeness and letting a human choose covers
-- that case and the identical-amounts case at once.
--
-- The NOT EXISTS is what stops one deposit being claimed by two stubs — the
-- two-earner household with equal take-home, which doc 23 names as the case
-- that must not auto-match. It cannot even be OFFERED twice.
--
-- Money-in is amount < 0 (Plaid's convention: positive is money leaving), and
-- transfers are excluded because a paycheque is not one.
SELECT
    t.id,
    t.date,
    t.amount,
    COALESCE(NULLIF(t.merchant_name, ''), t.name) AS label,
    a.name                                        AS account_name,
    -- Cast is load-bearing, not decoration: without it sqlc infers ABS() as an
    -- integer and the generated struct silently truncates every cent of the gap
    -- this column exists to report.
    ABS(-t.amount - sqlc.arg('net')::numeric)::numeric AS delta
FROM transactions t
JOIN accounts    a ON a.id = t.account_id
JOIN plaid_items i ON i.id = a.plaid_item_id
JOIN users       u ON u.id = i.user_id
LEFT JOIN categories c ON c.id = t.category_id
WHERE u.household_id = sqlc.arg('household_id')
  AND (i.user_id = sqlc.arg('user_id') OR i.is_shared)
  AND a.is_active
  AND NOT t.pending
  AND NOT t.excluded_from_reports
  AND NOT COALESCE(c.is_transfer, FALSE)
  AND t.amount < 0
  AND t.date >= sqlc.arg('from_date')
  AND t.date <= sqlc.arg('to_date')
  AND NOT EXISTS (
      SELECT 1 FROM paystubs claimed
      WHERE claimed.transaction_id = t.id
        AND claimed.id <> sqlc.arg('paystub_id')
  )
ORDER BY delta, ABS(t.date - sqlc.arg('pay_date')::date), t.date DESC
LIMIT sqlc.arg('max_results');
