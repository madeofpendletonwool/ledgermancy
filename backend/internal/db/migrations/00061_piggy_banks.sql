-- +goose Up
-- Piggy banks: lightweight, period-independent savings envelopes ("Car Repair
-- Fund", "Christmas 2026") that sit on an asset account and track a running
-- balance via an append-only event log (deposit / withdraw). Not tied to a
-- month or a budget period — the casual version of a goal.
--
-- A piggy bank is an ANNOTATION on part of an account balance, not a separate
-- balance. The money is already in the account (it arrived via a real
-- transaction); a piggy bank just earmarks a slice of it. Net worth must not
-- move when a piggy bank does, and computing available-for-piggy as
-- (account balance − sum of piggy banks on it) is what stops a household
-- over-allocating the same dollars twice.
--
-- current_amount is NOT stored here — it is DERIVED from the event log (deposits
-- minus withdrawals, summed in SQL). This is the same "derived, never stored"
-- rule goals use for progress: a stored running balance would be a second
-- source of truth that drifts from the events the moment a code path forgets to
-- update it. The event log is the only authority.
CREATE TABLE piggy_banks (
    id            UUID           PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id  UUID           NOT NULL REFERENCES households (id) ON DELETE CASCADE,
    -- The asset account this jar draws from. NOT NULL: a piggy bank always sits
    -- on an account, because available-for-piggy is "what's left of this
    -- account's balance", which has no meaning without one.
    account_id    UUID           NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    name          TEXT           NOT NULL,
    -- Nullable: an open-ended jar with no finish line ("the car fund"). A set
    -- target drives a progress bar; a NULL one just accumulates.
    target_amount NUMERIC(20, 4),
    created_at    TIMESTAMPTZ    NOT NULL DEFAULT now()
);

CREATE INDEX piggy_banks_household_idx ON piggy_banks (household_id);
CREATE INDEX piggy_banks_account_idx   ON piggy_banks (account_id);

-- The append-only event log behind every piggy bank's running balance. Each
-- row is one deposit or one withdrawal; the balance is always the SQL sum of
-- these. amount is stored positive with the direction in `type`, so a
-- withdrawal is never a negative number that a careless SUM could misread.
--
-- transaction_id optionally links an event to a real transaction (a transfer to
-- savings, a paycheck deposit) — an annotation, never a second source of truth.
-- ON DELETE SET NULL: deleting the transaction leaves the event behind, because
-- the piggy bank's balance is the event log's sum, not the transaction's.
CREATE TABLE piggy_bank_events (
    id            UUID           PRIMARY KEY DEFAULT gen_random_uuid(),
    piggy_bank_id UUID           NOT NULL REFERENCES piggy_banks (id) ON DELETE CASCADE,
    type          TEXT           NOT NULL CHECK (type IN ('deposit', 'withdraw')),
    amount        NUMERIC(20, 4) NOT NULL CHECK (amount > 0),
    transaction_id UUID          REFERENCES transactions (id) ON DELETE SET NULL,
    created_at    TIMESTAMPTZ    NOT NULL DEFAULT now()
);

CREATE INDEX piggy_bank_events_piggy_idx ON piggy_bank_events (piggy_bank_id, created_at DESC);

-- +goose Down
DROP TABLE IF EXISTS piggy_bank_events;
DROP TABLE IF EXISTS piggy_banks;
