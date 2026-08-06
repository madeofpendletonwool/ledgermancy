-- +goose Up
--
-- The likelihood layer (doc 33): Monte Carlo over allocation plans, the
-- documented guardrail rule, and plan-vs-actual tracking.
--
-- Numbered 00056, which is what docs/plans/README.md's reservation table
-- assigns. The doc's own text still says 00054; that number was consumed by
-- 00054_advisor_surface.sql when doc 30 renumbered up from its reserved 00047
-- and pushed every wave-6 row up by one, and doc 32 landed at 00055 for the
-- same reason. goose runs in strict-ordering mode (db.go), so a number below
-- the highest applied migration would refuse to start every instance that has
-- run them.
--
-- THIS MIGRATION IS SMALL ON PURPOSE. The simulation is pure and its results
-- are NEVER persisted: a distribution is recomputed from the plan plus a
-- deterministic seed every time it is asked for. A stored success rate is a
-- figure that quietly stops being true and keeps being displayed — the same
-- rule doc 32 applied to projections, for the same reason.

-- --------------------------------------------------------------------------
-- plan_trackings — plan-vs-actual reconciliation
-- --------------------------------------------------------------------------
--
-- One row per (plan, period). "Period" is the snapshot date.
--
-- ACTUALS ARE NOT STORED HERE. They are read live from
-- investment_transactions, account_balance_history and transactions at read
-- time, so editing a past contribution corrects the drift without a migration
-- and without a backfill. What IS stored is the plan's EXPECTED position at
-- as_of, because recomputing that requires replaying the plan's inputs against
-- the assumptions it was built with — which is exactly the sort of replay that
-- silently changes answer when the household edits an assumption.
--
-- MONEY INSIDE snapshot_inputs IS A STRING, NEVER A JSON NUMBER. The
-- continuity rule says money is cast to text in SQL and never travels as a JSON
-- number, and export.go enforces it for `numeric` COLUMNS — but it cannot reach
-- inside a jsonb value, which `normalise` passes through as json.RawMessage. So
-- {"lump": 30000.50} would leave the portable export as a JSON number and be
-- parsed by whatever reads it as a float64. Doc 32's "Money in JSONB" section
-- owns the rule; this table follows it. expected_lump and expected_total are
-- ordinary numeric COLUMNS, so export.go's cast covers them.

CREATE TABLE plan_trackings (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    plan_id     UUID NOT NULL REFERENCES allocation_plans (id) ON DELETE CASCADE,
    as_of       DATE NOT NULL,
    -- The plan's projected position at as_of: deterministic, from doc 32's
    -- allocator. expected_lump is what the plan said would have gone in up
    -- front; expected_total is what it said would have gone in by as_of.
    expected_lump   NUMERIC(20, 4) NOT NULL,
    expected_total  NUMERIC(20, 4) NOT NULL,
    -- The live baseline captured at this as_of, for the record. Money inside is
    -- a StringFixed(2) STRING — see the note above.
    snapshot_inputs JSONB NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- One snapshot per plan per date. Re-tracking the same day overwrites
    -- rather than accumulating: two rows for one date would make "drift since
    -- the last check" ambiguous about which check.
    UNIQUE (plan_id, as_of)
);

COMMENT ON COLUMN plan_trackings.snapshot_inputs IS
    'Baseline captured at as_of. Every money field is a decimal STRING, never a JSON number.';

CREATE INDEX plan_trackings_plan_idx ON plan_trackings (plan_id, as_of DESC);

-- +goose Down

DROP TABLE IF EXISTS plan_trackings;
