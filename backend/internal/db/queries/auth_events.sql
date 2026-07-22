-- name: RecordAuthEvent :exec
INSERT INTO auth_events (user_id, email_attempted, event_type, client_ip, user_agent, metadata)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: ListUserAuthEvents :many
SELECT event_type, client_ip, user_agent, metadata, created_at
FROM auth_events
WHERE user_id = $1
ORDER BY created_at DESC
LIMIT $2;

-- name: DeleteOldAuthEvents :execrows
-- Retention sweep. An audit log nobody prunes is just an ever-growing table,
-- and the security value of a three-year-old login record is nil.
DELETE FROM auth_events WHERE created_at < now() - sqlc.arg(max_age)::interval;
