-- +goose Up
-- Plaid's taxonomy has a seventeenth primary category, OTHER, which the initial
-- seed missed. Unmapped values fall through to the "uncategorised" fallback, so
-- nothing was mis-filed — but relying on a fallback for a value Plaid does
-- report is accidental rather than intentional. Mapping it explicitly means the
-- fallback now only ever catches genuinely unknown values.
INSERT INTO pfc_category_map (pfc_primary, pfc_detailed, category_slug)
VALUES ('OTHER', NULL, 'uncategorised')
ON CONFLICT (pfc_primary, pfc_detailed) DO NOTHING;

-- +goose Down
DELETE FROM pfc_category_map WHERE pfc_primary = 'OTHER';
