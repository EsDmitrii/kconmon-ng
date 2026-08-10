-- name: CreateAnnotation :one
-- id is caller-supplied, same as CreateTarget (targets.sql): the column has a DEFAULT.
INSERT INTO annotations (id, start_at, end_at, scope, text, created_by)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, start_at, end_at, scope, text, created_by, created_at;

-- name: ListAnnotations :many
-- The chart-marker query: every annotation whose interval OVERLAPS the requested window.
SELECT id, start_at, end_at, scope, text, created_by, created_at
FROM annotations
WHERE (sqlc.narg('scope')::text IS NULL OR scope = sqlc.narg('scope')::text)
  AND (sqlc.narg('from_time')::timestamptz IS NULL OR
       coalesce(end_at, start_at) >= sqlc.narg('from_time')::timestamptz)
  AND (sqlc.narg('to_time')::timestamptz IS NULL OR
       start_at < sqlc.narg('to_time')::timestamptz)
  AND (sqlc.narg('cur_time')::timestamptz IS NULL OR
       (start_at, id) < (sqlc.narg('cur_time')::timestamptz, sqlc.narg('cur_id')::uuid))
ORDER BY start_at DESC, id DESC
LIMIT sqlc.arg('lim');

-- name: GetAnnotation :one
SELECT id, start_at, end_at, scope, text, created_by, created_at
FROM annotations
WHERE id = $1;

-- name: DeleteAnnotation :execrows
-- has no edit, so delete-and-recreate is the only correction path and it has to be clean.
DELETE FROM annotations WHERE id = $1;

-- name: DeleteAnnotationsBefore :execrows
-- Retention by start_at: an annotation is pinned to the moment it describes.
DELETE FROM annotations
WHERE id IN (SELECT a.id FROM annotations a WHERE a.start_at < $1 ORDER BY a.start_at LIMIT $2);
