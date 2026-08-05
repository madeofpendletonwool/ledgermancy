-- Retirement projection inputs: household assumptions and the per-account
-- contribution plan.
--
-- Reads carry the same visibility scoping as the rest of the reporting layer,
-- resolved through the account_access view (00053): household membership plus
-- `v.user_id = $2 OR v.is_shared`, so a member's private institution never
-- appears in a household projection.

-- name: GetProjectionAssumptions :one
SELECT * FROM projection_assumptions WHERE household_id = $1;

-- name: UpsertProjectionAssumptions :one
-- One row per household; saving again edits it in place. Every field is sent
-- on every save, so the UI's form is the whole state and a cleared field
-- genuinely clears (NULL means "not decided", which is a real answer for the
-- age and Social Security columns).
INSERT INTO projection_assumptions (
    household_id, real_return_rate, inflation_rate, withdrawal_rate,
    target_retirement_age, current_age, annual_ss_income, ss_start_age,
    target_annual_spending
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (household_id) DO UPDATE SET
    real_return_rate       = EXCLUDED.real_return_rate,
    inflation_rate         = EXCLUDED.inflation_rate,
    withdrawal_rate        = EXCLUDED.withdrawal_rate,
    target_retirement_age  = EXCLUDED.target_retirement_age,
    current_age            = EXCLUDED.current_age,
    annual_ss_income       = EXCLUDED.annual_ss_income,
    ss_start_age           = EXCLUDED.ss_start_age,
    target_annual_spending = EXCLUDED.target_annual_spending,
    updated_at             = now()
RETURNING *;

-- name: ListProjectableAccounts :many
-- Every visible investment account with whatever contribution plan it has.
--
-- tax_treatment comes through as-is, NULL included: an untagged account cannot
-- be projected (its limit, its horizon and its withdrawal treatment are all
-- unknown), and the caller reports it as an excluded gap rather than defaulting
-- it to something flattering. The LEFT JOIN is why an account with no plan yet
-- still appears — it is a real account with a real balance.
SELECT
    a.id,
    a.name,
    a.mask,
    a.subtype,
    a.current_balance,
    a.tax_treatment,
    v.institution_name,
    c.monthly_contribution,
    c.employer_match_pct,
    c.annual_salary,
    c.employer_match_limit,
    c.beneficiary_current_age,
    c.beneficiary_target_age,
    -- The beneficiary's birthdate, when the account is tagged with the person
    -- it is held for. Preferred over beneficiary_current_age, which is a stored
    -- integer that decays — see networth.ResolveAge for the order. NULL here
    -- means fall back, which is what an upgraded instance does until somebody
    -- enters a birthdate.
    bp.birthdate AS beneficiary_birthdate
FROM accounts a
JOIN account_access v ON v.account_id = a.id
LEFT JOIN account_contributions c ON c.account_id = a.id
LEFT JOIN household_people bp     ON bp.id = a.beneficiary_person_id
WHERE v.household_id = $1
  AND (v.user_id = $2 OR v.is_shared)
  AND a.is_active
  AND a.type IN ('investment', 'brokerage')
ORDER BY v.institution_name, a.name;

-- name: UpsertAccountContribution :one
-- Sets the plan for one account. Ownership is enforced the same way the
-- tax-treatment write is: through the account's item owner, so a caller can
-- neither plan contributions into another household's account nor into a
-- private item belonging to another member of their own.
--
-- The SELECT in the VALUES clause is the guard — it yields no row when the
-- caller cannot see the account, so the INSERT writes nothing and the handler
-- sees pgx.ErrNoRows (a 404), rather than the write silently succeeding.
INSERT INTO account_contributions (
    account_id, monthly_contribution, employer_match_pct, annual_salary,
    employer_match_limit, beneficiary_current_age, beneficiary_target_age
)
SELECT
    a.id,
    sqlc.arg('monthly_contribution')::numeric,
    sqlc.narg('employer_match_pct')::numeric,
    sqlc.narg('annual_salary')::numeric,
    sqlc.narg('employer_match_limit')::numeric,
    sqlc.narg('beneficiary_current_age')::int,
    sqlc.narg('beneficiary_target_age')::int
FROM accounts a
JOIN account_access v ON v.account_id = a.id
WHERE a.id = sqlc.arg('account_id')
  AND v.household_id = sqlc.arg('household_id')
  AND (v.user_id = sqlc.arg('user_id') OR v.is_shared)
ON CONFLICT (account_id) DO UPDATE SET
    monthly_contribution    = EXCLUDED.monthly_contribution,
    employer_match_pct      = EXCLUDED.employer_match_pct,
    annual_salary           = EXCLUDED.annual_salary,
    employer_match_limit    = EXCLUDED.employer_match_limit,
    beneficiary_current_age = EXCLUDED.beneficiary_current_age,
    beneficiary_target_age  = EXCLUDED.beneficiary_target_age,
    updated_at              = now()
RETURNING *;

-- name: DeleteAccountContribution :exec
-- Clears a plan rather than storing a row of zeroes, so "no plan set" and
-- "planning to contribute nothing" stay distinguishable in the UI.
DELETE FROM account_contributions c
USING accounts a, account_access v
WHERE c.account_id = $1
  AND v.account_id = a.id
  AND a.id = c.account_id
  AND v.household_id = $2
  AND (v.user_id = $3 OR v.is_shared);
