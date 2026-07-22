-- Queries for TOTP enrolment, recovery codes, and the half-authenticated
-- login challenge. See migration 00006 for why challenges are their own table.

-- --------------------------------------------------------------------------
-- TOTP enrolment
-- --------------------------------------------------------------------------

-- name: GetUserMFA :one
SELECT
    id,
    totp_secret_encrypted,
    totp_enabled,
    totp_confirmed_at,
    totp_last_step
FROM users
WHERE id = $1;

-- name: SetPendingTOTPSecret :exec
-- Stores a freshly generated secret without enabling it. Enrolment is not
-- complete until the user proves they can generate a code from it, so this
-- deliberately leaves totp_enabled false and clears any previous replay state.
UPDATE users
SET totp_secret_encrypted = $2,
    totp_enabled          = FALSE,
    totp_confirmed_at     = NULL,
    totp_last_step        = NULL
WHERE id = $1;

-- name: ActivateTOTP :exec
UPDATE users
SET totp_enabled      = TRUE,
    totp_confirmed_at = now(),
    totp_last_step    = $2
WHERE id = $1;

-- name: DisableTOTP :exec
UPDATE users
SET totp_secret_encrypted = NULL,
    totp_enabled          = FALSE,
    totp_confirmed_at     = NULL,
    totp_last_step        = NULL
WHERE id = $1;

-- name: SetTOTPLastStep :execrows
-- The replay guard. The predicate is what makes it safe under concurrency:
-- two requests presenting the same code race here, and only the first moves
-- the step forward. The loser updates zero rows — which is why this returns a
-- row count rather than :exec. A caller seeing 0 must reject the code.
UPDATE users
SET totp_last_step = sqlc.arg(step)
WHERE id = sqlc.arg(id)
  AND (totp_last_step IS NULL OR totp_last_step < sqlc.arg(step));

-- --------------------------------------------------------------------------
-- Recovery codes
-- --------------------------------------------------------------------------

-- name: CreateRecoveryCode :exec
INSERT INTO user_recovery_codes (user_id, code_hash) VALUES ($1, $2);

-- name: DeleteUserRecoveryCodes :exec
DELETE FROM user_recovery_codes WHERE user_id = $1;

-- name: CountUnusedRecoveryCodes :one
SELECT count(*) FROM user_recovery_codes
WHERE user_id = $1 AND used_at IS NULL;

-- name: ConsumeRecoveryCode :execrows
-- Marks a code used, matching on the hash. `used_at IS NULL` in the predicate
-- is what makes a code single-use: redeeming the same one twice updates zero
-- rows the second time, so the caller sees the attempt fail.
UPDATE user_recovery_codes
SET used_at = now()
WHERE user_id = $1 AND code_hash = $2 AND used_at IS NULL;

-- --------------------------------------------------------------------------
-- Login challenges
-- --------------------------------------------------------------------------

-- name: CreateMFAChallenge :one
INSERT INTO mfa_challenges (user_id, token_hash, user_agent, client_ip, expires_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetMFAChallenge :one
-- Expiry is filtered here, so an expired challenge simply does not come back.
SELECT c.id, c.user_id, c.attempts, u.household_id, u.email, u.display_name
FROM mfa_challenges c
JOIN users u ON u.id = c.user_id
WHERE c.token_hash = $1 AND c.expires_at > now();

-- name: IncrementMFAChallengeAttempts :one
UPDATE mfa_challenges
SET attempts = attempts + 1
WHERE id = $1
RETURNING attempts;

-- name: DeleteMFAChallenge :exec
DELETE FROM mfa_challenges WHERE id = $1;

-- name: DeleteExpiredMFAChallenges :execrows
DELETE FROM mfa_challenges WHERE expires_at <= now();
