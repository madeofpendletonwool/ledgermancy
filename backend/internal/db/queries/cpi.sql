-- name: ListCPISeries :many
-- The whole published series, oldest first.
--
-- Read whole rather than paged, for the same reason ListSavingsBondRates is:
-- ~200 rows growing by twelve a year, and deflating a multi-year chart needs
-- most of them anyway. Paging this would only add round trips.
SELECT period, index_value FROM cpi_series ORDER BY period;

-- name: UpsertCPIPoint :one
-- Record one month's index.
--
-- Upsert rather than insert-only because BLS REVISES: an index published in
-- mid-month can be corrected later, and an insert-only job would keep serving
-- the superseded figure forever. The correction wins.
INSERT INTO cpi_series (period, index_value)
VALUES ($1, $2)
ON CONFLICT (period) DO UPDATE SET index_value = EXCLUDED.index_value
RETURNING *;
