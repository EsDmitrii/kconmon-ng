-- name: CreateMaintenanceWindow :one
-- id is caller-supplied, same as CreateAnnotation: see that query's comment.
INSERT INTO maintenance_windows (id, scope, start_at, end_at, reason, created_by)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, scope, start_at, end_at, reason, created_by, created_at;

-- name: ListMaintenanceWindows :many
-- Every window whose interval OVERLAPS the requested range, newest first -- the chart-markArea
-- query.
SELECT id, scope, start_at, end_at, reason, created_by, created_at
FROM maintenance_windows
WHERE (sqlc.narg('scope')::text IS NULL OR scope = sqlc.narg('scope')::text)
  AND (sqlc.narg('from_time')::timestamptz IS NULL OR end_at >= sqlc.narg('from_time')::timestamptz)
  AND (sqlc.narg('to_time')::timestamptz IS NULL OR start_at < sqlc.narg('to_time')::timestamptz)
  AND (sqlc.narg('cur_time')::timestamptz IS NULL OR
       (start_at, id) < (sqlc.narg('cur_time')::timestamptz, sqlc.narg('cur_id')::uuid))
ORDER BY start_at DESC, id DESC
LIMIT sqlc.arg('lim');

-- name: DeleteMaintenanceWindow :execrows
-- There is no edit by design: a window is two timestamps and a reason.
DELETE FROM maintenance_windows WHERE id = $1;

-- name: DeleteMaintenanceWindowsBefore :execrows
-- Retention by end_at, not start_at: a window that is still open is still current however long ago
-- it began.
DELETE FROM maintenance_windows
WHERE id IN (SELECT m.id FROM maintenance_windows m WHERE m.end_at < $1 ORDER BY m.end_at LIMIT $2);
