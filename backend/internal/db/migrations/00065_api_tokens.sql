-- +goose Up
-- Personal API tokens: a long-lived bearer credential a user mints for a
-- THIRD-PARTY client, so the REST API is reachable by something other than the
-- SPA the user happens to be logged into.
--
-- Why this is a second table rather than a longer-lived row in `sessions`. A
-- session is a browser's, is created by typing a password, dies of idleness,
-- and is a thing the user wants gone the moment they sign out. A token is a
-- program's, is created deliberately from Settings, survives every sign-out on
-- purpose, and is only ever removed by an explicit revoke. Sharing one table
-- would mean every predicate on either concept had to carry a discriminator,
-- and the security-sweep job would have to learn not to collect the rows the
-- user is relying on. `sessions` is also classified Ephemeral for backups —
-- restoring a live login would be wrong — where a token IS worth restoring, so
-- they do not even belong to the same continuity class.
--
-- Numbering: 00063/00064 are taken by the unmerged `tags` branch, so this one
-- takes 00065 to stay above everything either branch can apply. Goose refuses a
-- migration numbered below the current version.
CREATE TABLE api_tokens (
    id      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- What the user called it ("Home Assistant", "export script"). This is the
    -- only handle they have on a credential whose plaintext they can never see
    -- again, so it is required rather than nullable: an unnamed row in a revoke
    -- list is a credential nobody dares delete.
    name    TEXT NOT NULL,
    -- HMAC-SHA256(SESSION_SECRET, token), hex — the same derivation
    -- sessions.token_hash uses, so a leaked database alone cannot be used to
    -- forge or recognise a live token. The plaintext is shown once at creation
    -- and is not recoverable from this row.
    token_hash TEXT NOT NULL UNIQUE,
    -- What the token may do. 'read' is always present and 'write' is what
    -- separates a read-only token from a read-write one; the check keeps a typo
    -- from silently minting a token with no meaning. Stored as an array rather
    -- than a boolean because the interesting future additions here are narrower
    -- scopes ('transactions:read'), not a second flag.
    scopes TEXT[] NOT NULL DEFAULT ARRAY['read'],
    -- Best-effort activity, coalesced by the middleware so an integration
    -- polling every minute does not turn every read into a write. NULL means
    -- the token has never been used, which is a different fact from "used at
    -- creation time" and is worth showing as such.
    last_used_at TIMESTAMPTZ,
    -- Nullable on purpose: a token with no expiry is the normal case for a
    -- self-hosted integration that is meant to keep working. An expiry is
    -- offered for the ones handed to something temporary.
    expires_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT api_tokens_name_not_blank CHECK (btrim(name) <> ''),
    CONSTRAINT api_tokens_scopes_known CHECK (
        scopes <@ ARRAY['read', 'write']::TEXT[] AND 'read' = ANY (scopes)
    )
);

-- The revoke list is "my tokens, newest first", which is the only read that is
-- not by hash. The hash lookup is served by the UNIQUE constraint above.
CREATE INDEX api_tokens_user_id_idx ON api_tokens (user_id, created_at DESC);

-- +goose Down
DROP TABLE IF EXISTS api_tokens;
