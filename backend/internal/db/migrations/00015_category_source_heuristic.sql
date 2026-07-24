-- +goose Up
-- The transfer/card-payment detection step records its decisions with
-- category_source = 'heuristic'; the original CHECK predates that source and
-- would reject the write, failing categorization the moment the heuristic
-- matched a real transaction. Widen the allowed set.
ALTER TABLE transactions DROP CONSTRAINT IF EXISTS transactions_category_source_check;
ALTER TABLE transactions ADD CONSTRAINT transactions_category_source_check
    CHECK (category_source IS NULL
           OR category_source IN ('manual', 'rule', 'cache', 'plaid', 'llm', 'heuristic'));

-- +goose Down
ALTER TABLE transactions DROP CONSTRAINT IF EXISTS transactions_category_source_check;
ALTER TABLE transactions ADD CONSTRAINT transactions_category_source_check
    CHECK (category_source IS NULL
           OR category_source IN ('manual', 'rule', 'cache', 'plaid', 'llm'));
