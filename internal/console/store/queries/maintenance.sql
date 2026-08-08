-- name: CreateMaintenanceWindow :one
-- id is caller-supplied, same as CreateAnnotation: see that query's comment.
INSERT INTO maintenance_windows (id, scope, start_at, end_at, reason, created_by)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, scope, start_at, end_at, reason, created_by, created_at;

-- name: ListMaintenanceWindows :many
-- Every window whose interval OVERLAPS the requested range, newest first --
-- the chart-markArea query, so containment would be the wrong test: a window
-- that opened before the range and is still running inside it is exactly the
-- one that explains what the operator is looking at.
--
-- Both ends are closed here (the table's own CHECK guarantees end_at >
-- start_at), so no coalesce is needed: the overlap is `end_at >= from AND
-- start_at < to`, half-open on the upper bound like every other window in this
-- package.
--
-- scope takes the annotations NULL/'' treatment -- '' is the global scope, a
-- real value, so "no filter" is a SQL NULL argument.
--
-- (start_at DESC, id DESC) is maintenance_time_idx's own order, so the listing
-- pages without a sort.
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
-- There is no edit by design (M6 Task 4): a window is two timestamps and a
-- reason, so delete-and-recreate is both the correction path and the whole of
-- it. Nothing references a window, so this removes it and nothing else.
DELETE FROM maintenance_windows WHERE id = $1;

-- name: DeleteMaintenanceWindowsBefore :execrows
-- Retention by end_at, not start_at: a window that is still open is still
-- current however long ago it began -- the same reasoning
-- DeletePathSnapshotsBefore applies to last_seen. m alias on the subquery's
-- own FROM for the sqlc v1.31.1 analyzer quirk documented on DeleteRunsBefore
-- (checks.sql).
DELETE FROM maintenance_windows
WHERE id IN (SELECT m.id FROM maintenance_windows m WHERE m.end_at < $1 ORDER BY m.end_at LIMIT $2);
