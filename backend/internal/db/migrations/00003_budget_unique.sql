-- +goose Up
-- One household budget per category per period, so setting a budget twice
-- updates it instead of silently creating a second one that double-counts.
-- Partial (user_id IS NULL) because personal budgets are a separate concept.
CREATE UNIQUE INDEX budgets_household_category_key
    ON budgets (household_id, category_id, owner_scope, period)
    WHERE user_id IS NULL;

-- +goose Down
DROP INDEX IF EXISTS budgets_household_category_key;
