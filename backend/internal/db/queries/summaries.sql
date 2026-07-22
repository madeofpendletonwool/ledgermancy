-- name: GetMonthlySummary :one
SELECT * FROM monthly_summaries
WHERE household_id = $1 AND month = $2;

-- name: UpsertMonthlySummary :one
-- Regenerating overwrites the cached text and stamps a fresh created_at.
INSERT INTO monthly_summaries (household_id, month, summary, model)
VALUES ($1, $2, $3, $4)
ON CONFLICT (household_id, month) DO UPDATE SET
    summary    = EXCLUDED.summary,
    model      = EXCLUDED.model,
    created_at = now()
RETURNING *;
