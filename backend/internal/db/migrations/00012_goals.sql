-- +goose Up
-- Savings/target goals: where a household (or one member) wants its money to go.
-- Feasibility (required monthly contribution, on-track/behind) is computed
-- deterministically from the same projection engine the forecast uses — nothing
-- here is an AI figure. Progress is DERIVED, never stored, so a current_amount
-- column can't drift from reality: for an account-linked goal it's that
-- account's balance; unlinked, it's accumulated surplus since created_at.
CREATE TABLE goals (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id  UUID          NOT NULL REFERENCES households (id) ON DELETE CASCADE,
    -- Scope mirrors budgets/preferences: a shared household goal or a personal
    -- one. user_id is set iff scope = 'user'.
    scope         TEXT          NOT NULL CHECK (scope IN ('user', 'household')),
    user_id       UUID          REFERENCES users (id) ON DELETE CASCADE,
    kind          TEXT          NOT NULL,   -- 'savings' | 'debt_payoff' (start with 'savings')
    name          TEXT          NOT NULL,   -- "Trip to Japan"
    target_amount NUMERIC(20, 4) NOT NULL,  -- a positive target
    target_date   DATE,                     -- nullable: an open-ended goal
    -- Optional links that make progress measurable without manual updates.
    account_id    UUID          REFERENCES accounts (id)   ON DELETE SET NULL,
    category_id   UUID          REFERENCES categories (id) ON DELETE SET NULL,
    created_at    TIMESTAMPTZ   NOT NULL DEFAULT now(),
    achieved_at   TIMESTAMPTZ,              -- set when progress first reaches target
    archived_at   TIMESTAMPTZ,
    -- user_id is present exactly when the goal is personal.
    CONSTRAINT goals_user_scope CHECK (
        (scope = 'user' AND user_id IS NOT NULL)
        OR (scope = 'household' AND user_id IS NULL)
    )
);
CREATE INDEX goals_household_idx ON goals (household_id, archived_at);

-- +goose Down
DROP TABLE IF EXISTS goals;
