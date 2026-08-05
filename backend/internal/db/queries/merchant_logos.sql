-- Merchant logo cache (00050). Read by the api to serve an avatar, written by
-- the fetch job. Every key here is a RESOLVED merchant key, per the resolution
-- rule documented at the top of merchants.sql.

-- name: ListMerchantsNeedingLogos :many
-- Merchants the fetcher has never considered, busiest first.
--
-- "Never considered" rather than "has no logo": a row with state 'none' is a
-- decision, not a gap, so it must exclude the merchant from this list exactly
-- as a 'found' row does. Ordering by transaction count means a household whose
-- ledger outruns the per-pass limit gets logos where it will notice them.
--
-- Visibility follows the same rule as every other household-wide job query: the
-- caller passes uuid.Nil as user_id so only shared items are considered, and a
-- private institution's merchants are never resolved on the household's behalf.
SELECT
    COALESCE(ma.entity_id::text, t.merchant_key)::text                      AS merchant_key,
    (array_agg(COALESCE(me.canonical_name, t.merchant_name, t.name) ORDER BY t.date DESC))[1]::text AS merchant_name,
    COUNT(*)::bigint                                                        AS transaction_count
FROM transactions t
JOIN accounts a    ON a.id = t.account_id
JOIN account_access v ON v.account_id = a.id
LEFT JOIN merchant_aliases ma
       ON ma.household_id = @household_id
      AND ma.merchant_key = t.merchant_key
      AND ma.source <> 'suggested'
LEFT JOIN merchant_entities me ON me.id = ma.entity_id
WHERE v.household_id = @household_id
  AND (v.user_id = @user_id OR v.is_shared)
  AND a.is_active
  AND NOT t.pending
  AND t.merchant_key IS NOT NULL
  AND NOT EXISTS (
      SELECT 1 FROM merchant_logos ml
      WHERE ml.household_id = @household_id
        AND ml.merchant_key = COALESCE(ma.entity_id::text, t.merchant_key)
  )
GROUP BY COALESCE(ma.entity_id::text, t.merchant_key)
ORDER BY COUNT(*) DESC
LIMIT @row_limit;

-- name: UpsertMerchantLogo :exec
-- Record one outcome, found or not. Re-running a pass overwrites rather than
-- conflicting, so a future "rescan" only has to delete rows to make them
-- eligible again.
INSERT INTO merchant_logos (
    household_id, merchant_key, merchant_name, domain, content_type, image, state, checked_at
) VALUES (
    @household_id, @merchant_key, @merchant_name, @domain, @content_type, @image, @state, now()
)
ON CONFLICT (household_id, merchant_key) DO UPDATE SET
    merchant_name = EXCLUDED.merchant_name,
    domain        = EXCLUDED.domain,
    content_type  = EXCLUDED.content_type,
    image         = EXCLUDED.image,
    state         = EXCLUDED.state,
    checked_at    = EXCLUDED.checked_at;

-- name: GetMerchantLogo :one
-- The bytes for one merchant. Household-scoped in the query rather than in the
-- handler: a merchant key is a guessable string, so it must never be on its own
-- sufficient to read a row.
SELECT content_type, image
FROM merchant_logos
WHERE household_id = $1 AND merchant_key = $2 AND state = 'found';

-- name: DeleteMerchantLogos :exec
-- Forget everything fetched for a household. Used when the household switches
-- the feature off: the cache is derived data about where they shop, and keeping
-- it after they said no would be keeping the part they objected to.
DELETE FROM merchant_logos WHERE household_id = $1;
