-- +goose Up
-- Seeds the system category taxonomy from Plaid's Personal Finance Category
-- (PFC) primary categories, and the mapping used to resolve a Plaid category
-- onto one of ours.
--
-- Primary-level granularity is deliberate: sixteen categories is what a
-- spending breakdown is actually readable at, and the finer PFC value is still
-- kept on every transaction (plaid_pfc_detailed) for when more detail is
-- wanted. Households can add their own categories on top.

-- Maps a Plaid PFC value onto one of our categories. `pfc_detailed` matches
-- first so specific cases (notably credit-card payments) can override the
-- broader primary mapping.
CREATE TABLE pfc_category_map (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    pfc_primary  TEXT NOT NULL,
    pfc_detailed TEXT,
    category_slug TEXT NOT NULL,
    UNIQUE (pfc_primary, pfc_detailed)
);

CREATE INDEX pfc_category_map_lookup_idx ON pfc_category_map (pfc_primary, pfc_detailed);

-- System categories (household_id NULL).
--
-- is_transfer marks movements between the household's own accounts, which are
-- neither income nor spending. is_fixed marks obligations that recur at a
-- predictable amount, which is what separates committed from discretionary
-- spending in the reports.
INSERT INTO categories (household_id, name, slug, is_income, is_transfer, is_fixed, sort_order, color) VALUES
    (NULL, 'Income',                  'income',                    TRUE,  FALSE, FALSE,  1, '#4ade80'),
    (NULL, 'Transfer In',             'transfer-in',               FALSE, TRUE,  FALSE,  2, '#7b749c'),
    (NULL, 'Transfer Out',            'transfer-out',              FALSE, TRUE,  FALSE,  3, '#7b749c'),
    (NULL, 'Credit Card Payment',     'credit-card-payment',       FALSE, TRUE,  FALSE,  4, '#7b749c'),
    (NULL, 'Rent & Utilities',        'rent-and-utilities',        FALSE, FALSE, TRUE,   5, '#8b5cf6'),
    (NULL, 'Loan Payments',           'loan-payments',             FALSE, FALSE, TRUE,   6, '#a78bfa'),
    (NULL, 'Food & Drink',            'food-and-drink',            FALSE, FALSE, FALSE,  7, '#e8b962'),
    (NULL, 'Groceries',               'groceries',                 FALSE, FALSE, FALSE,  8, '#f2d492'),
    (NULL, 'Transportation',          'transportation',            FALSE, FALSE, FALSE,  9, '#60a5fa'),
    (NULL, 'Travel',                  'travel',                    FALSE, FALSE, FALSE, 10, '#38bdf8'),
    (NULL, 'General Merchandise',     'general-merchandise',       FALSE, FALSE, FALSE, 11, '#fb7185'),
    (NULL, 'Entertainment',           'entertainment',             FALSE, FALSE, FALSE, 12, '#f472b6'),
    (NULL, 'Personal Care',           'personal-care',             FALSE, FALSE, FALSE, 13, '#c084fc'),
    (NULL, 'Medical',                 'medical',                   FALSE, FALSE, FALSE, 14, '#2dd4bf'),
    (NULL, 'General Services',        'general-services',          FALSE, FALSE, FALSE, 15, '#94a3b8'),
    (NULL, 'Home Improvement',        'home-improvement',          FALSE, FALSE, FALSE, 16, '#fbbf24'),
    (NULL, 'Government & Non-Profit', 'government-and-non-profit', FALSE, FALSE, FALSE, 17, '#a3a3a3'),
    (NULL, 'Bank Fees',               'bank-fees',                 FALSE, FALSE, FALSE, 18, '#f87171'),
    (NULL, 'Uncategorised',           'uncategorised',             FALSE, FALSE, FALSE, 99, '#7b749c');

-- Primary-level mappings.
INSERT INTO pfc_category_map (pfc_primary, pfc_detailed, category_slug) VALUES
    ('INCOME',                    NULL, 'income'),
    ('TRANSFER_IN',               NULL, 'transfer-in'),
    ('TRANSFER_OUT',              NULL, 'transfer-out'),
    ('LOAN_PAYMENTS',             NULL, 'loan-payments'),
    ('BANK_FEES',                 NULL, 'bank-fees'),
    ('ENTERTAINMENT',             NULL, 'entertainment'),
    ('FOOD_AND_DRINK',            NULL, 'food-and-drink'),
    ('GENERAL_MERCHANDISE',       NULL, 'general-merchandise'),
    ('HOME_IMPROVEMENT',          NULL, 'home-improvement'),
    ('MEDICAL',                   NULL, 'medical'),
    ('PERSONAL_CARE',             NULL, 'personal-care'),
    ('GENERAL_SERVICES',          NULL, 'general-services'),
    ('GOVERNMENT_AND_NON_PROFIT', NULL, 'government-and-non-profit'),
    ('TRANSPORTATION',            NULL, 'transportation'),
    ('TRAVEL',                    NULL, 'travel'),
    ('RENT_AND_UTILITIES',        NULL, 'rent-and-utilities');

-- Detailed overrides.
--
-- The credit-card row is the important one. A card payment is money moving
-- between two of the household's own accounts, not new spending: the purchases
-- were already counted when they hit the card. Leaving it under LOAN_PAYMENTS
-- would count every card dollar twice — once as the purchase, once as the
-- payment — and silently inflate every monthly total.
INSERT INTO pfc_category_map (pfc_primary, pfc_detailed, category_slug) VALUES
    ('LOAN_PAYMENTS',  'LOAN_PAYMENTS_CREDIT_CARD_PAYMENT',        'credit-card-payment'),
    ('FOOD_AND_DRINK', 'FOOD_AND_DRINK_GROCERIES',                 'groceries'),
    -- Deposits into the household's own investment accounts are savings, not
    -- spending; they are the leftover being put to work.
    ('TRANSFER_OUT',   'TRANSFER_OUT_INVESTMENT_AND_RETIREMENT_FUNDS', 'transfer-out'),
    ('TRANSFER_OUT',   'TRANSFER_OUT_SAVINGS',                     'transfer-out');

-- +goose Down
DELETE FROM categories WHERE household_id IS NULL;
DROP TABLE IF EXISTS pfc_category_map;
