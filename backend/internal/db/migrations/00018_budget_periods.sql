-- +goose Up
-- Non-monthly budgets: allow a weekly period alongside monthly and yearly, and
-- make a category carry ONE household budget whose period can change, rather than
-- one budget per (category, period). The old unique key included period, so
-- switching a budget from monthly to weekly would have silently created a second
-- row; dropping period from the key makes the upsert update in place.
DROP INDEX IF EXISTS budgets_household_category_key;
CREATE UNIQUE INDEX budgets_household_category_key
    ON budgets (household_id, category_id, owner_scope)
    WHERE user_id IS NULL;

ALTER TABLE budgets DROP CONSTRAINT budgets_period_check;
ALTER TABLE budgets ADD CONSTRAINT budgets_period_check
    CHECK (period IN ('weekly', 'monthly', 'yearly'));

-- +goose Down
ALTER TABLE budgets DROP CONSTRAINT budgets_period_check;
ALTER TABLE budgets ADD CONSTRAINT budgets_period_check
    CHECK (period IN ('monthly', 'yearly'));

DROP INDEX IF EXISTS budgets_household_category_key;
CREATE UNIQUE INDEX budgets_household_category_key
    ON budgets (household_id, category_id, owner_scope, period)
    WHERE user_id IS NULL;
