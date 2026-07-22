-- +goose Up
-- Optional TOTP multi-factor auth, plus the tables the security hardening pass
-- needs: recovery codes, half-authenticated login challenges, and an audit log.

-- --------------------------------------------------------------------------
-- Users: TOTP enrolment and login throttling state
-- --------------------------------------------------------------------------

ALTER TABLE users
    -- The shared TOTP secret, encrypted with the same AES-256-GCM key that
    -- protects Plaid access tokens. It is a bearer credential: anyone holding
    -- it can mint valid codes forever, so it never goes to disk in the clear.
    ADD COLUMN totp_secret_encrypted BYTEA,
    -- False while a secret exists but the user has not yet proved they can
    -- generate a code from it. Only a confirmed enrolment gates login.
    ADD COLUMN totp_enabled          BOOLEAN     NOT NULL DEFAULT FALSE,
    ADD COLUMN totp_confirmed_at     TIMESTAMPTZ,
    -- The last TOTP time-step accepted for this user. Codes are valid for a
    -- 90-second window (one step either side of now), which without this would
    -- make an observed code replayable for that whole window. Any step at or
    -- below this value is refused.
    ADD COLUMN totp_last_step        BIGINT,
    -- Durable failed-login state. In-process rate limiting is the first line of
    -- defence, but it resets when the container restarts; these two survive.
    ADD COLUMN failed_login_count    INTEGER     NOT NULL DEFAULT 0,
    ADD COLUMN locked_until          TIMESTAMPTZ,
    -- Enforce the invariant in the database rather than trusting every future
    -- caller to keep the flag and the secret in step.
    ADD CONSTRAINT users_totp_enabled_requires_secret
        CHECK (NOT totp_enabled OR totp_secret_encrypted IS NOT NULL);

-- --------------------------------------------------------------------------
-- Recovery codes
-- --------------------------------------------------------------------------

-- There is no SMTP anywhere in this application, so a lost authenticator would
-- otherwise mean permanent lockout. These are the self-service way back in.
--
-- code_hash is HMAC-SHA256 keyed with SESSION_SECRET, not argon2id. The same
-- reasoning as session tokens applies (see auth/session.go): these are
-- high-entropy random values, so a single fast keyed hash is right. Argon2
-- would also force up to ten 64 MiB verifications per login attempt, which is
-- a denial of service against ourselves.
CREATE TABLE user_recovery_codes (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    code_hash  TEXT        NOT NULL UNIQUE,
    used_at    TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX user_recovery_codes_user_id_idx ON user_recovery_codes (user_id);

-- --------------------------------------------------------------------------
-- MFA challenges
-- --------------------------------------------------------------------------

-- A login that has passed the password but not yet the second factor.
--
-- This is deliberately NOT a flag on the sessions table. Keeping the
-- half-authenticated state in a separate table means a pending challenge can
-- never satisfy the auth middleware, no matter what it does — a row in
-- `sessions` continues to mean exactly one thing: fully authenticated.
CREATE TABLE mfa_challenges (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash TEXT        NOT NULL UNIQUE,
    attempts   INTEGER     NOT NULL DEFAULT 0,
    user_agent TEXT,
    client_ip  TEXT,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX mfa_challenges_expires_at_idx ON mfa_challenges (expires_at);

-- --------------------------------------------------------------------------
-- Auth audit log
-- --------------------------------------------------------------------------

-- Every security-relevant event, so "was that login me?" has an answer.
--
-- user_id is nullable and ON DELETE SET NULL: a failed login for an address
-- that does not exist has no user to point at, and removing a user should not
-- erase the record that something happened.
CREATE TABLE auth_events (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         UUID REFERENCES users (id) ON DELETE SET NULL,
    -- Recorded only for failures where no user resolves, so the log can show
    -- which addresses are being probed.
    email_attempted TEXT,
    event_type      TEXT        NOT NULL,
    client_ip       TEXT,
    user_agent      TEXT,
    -- Event-specific detail. JSONB so new event kinds need no migration.
    metadata        JSONB       NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX auth_events_user_created_idx ON auth_events (user_id, created_at DESC);
CREATE INDEX auth_events_created_idx      ON auth_events (created_at DESC);

-- --------------------------------------------------------------------------
-- Sessions: idle expiry and device attribution
-- --------------------------------------------------------------------------

ALTER TABLE sessions
    -- expires_at is an absolute cap. This adds an idle cap on top, so a
    -- forgotten session on a borrowed machine dies in days rather than a month.
    ADD COLUMN last_used_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ADD COLUMN client_ip    TEXT;

CREATE INDEX sessions_last_used_at_idx ON sessions (last_used_at);

-- +goose Down
DROP TABLE IF EXISTS auth_events, mfa_challenges, user_recovery_codes CASCADE;

ALTER TABLE sessions
    DROP COLUMN IF EXISTS last_used_at,
    DROP COLUMN IF EXISTS client_ip;

ALTER TABLE users
    DROP CONSTRAINT IF EXISTS users_totp_enabled_requires_secret,
    DROP COLUMN IF EXISTS totp_secret_encrypted,
    DROP COLUMN IF EXISTS totp_enabled,
    DROP COLUMN IF EXISTS totp_confirmed_at,
    DROP COLUMN IF EXISTS totp_last_step,
    DROP COLUMN IF EXISTS failed_login_count,
    DROP COLUMN IF EXISTS locked_until;
