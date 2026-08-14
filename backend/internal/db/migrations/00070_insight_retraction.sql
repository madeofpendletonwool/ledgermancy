-- +goose Up
-- Retraction: the feed's answer to an insight whose fact stopped being true.
--
-- The generation engine only ever upserts, so until now "no longer detected" and
-- "never detected" were indistinguishable to storage and an insight outlived its
-- cause. An overdue_bill raised at 6am survived the payment landing at noon; it
-- sat in the feed, saying "we can't find a payment for this", next to the
-- transaction that paid it, until somebody dismissed it by hand.
--
-- retracted_at is deliberately NOT a reuse of dismissed_at, even though both
-- hide a row. They differ in the one way that matters — whether a HUMAN made the
-- judgement:
--
--   dismissed_at  a member said "I don't want to see this". Sticky forever:
--                 UpsertInsight preserves it precisely so a detector that keeps
--                 re-detecting the same fact cannot resurrect what was waved
--                 away.
--   retracted_at  the app withdrew its own claim. The member may never have seen
--                 the row. It MUST clear on re-raise: if the fact becomes true
--                 again (the matching transaction is deleted, a merchant merge is
--                 undone), the insight has to come back, and folding this into
--                 dismissed_at would have buried it permanently — a silent
--                 failure exactly where the feature's credibility lives.
--
-- Both columns hide a row from the default feed; only the human one survives an
-- upsert. See UpsertInsight and RetractStaleInsights in queries/insights.sql.
ALTER TABLE insights ADD COLUMN retracted_at TIMESTAMPTZ;

COMMENT ON COLUMN insights.retracted_at IS
    'Set when the producer stopped detecting this insight''s fact, withdrawing '
    'the claim. Cleared on re-raise, unlike dismissed_at, which records a '
    'member''s own decision and is preserved across upserts.';

-- The feed index covers (household_id, dismissed_at, priority, created_at); the
-- default feed now filters on retracted_at as well, so it joins the key. Rebuilt
-- rather than added alongside: two partially-overlapping indexes on one hot read
-- path is a planner coin-flip, not a speedup.
DROP INDEX IF EXISTS insights_feed_idx;
CREATE INDEX insights_feed_idx
    ON insights (household_id, dismissed_at, retracted_at, priority DESC, created_at DESC);

-- +goose Down
DROP INDEX IF EXISTS insights_feed_idx;
CREATE INDEX insights_feed_idx
    ON insights (household_id, dismissed_at, priority DESC, created_at DESC);
ALTER TABLE insights DROP COLUMN IF EXISTS retracted_at;
