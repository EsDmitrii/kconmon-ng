-- name: CreateIncident :one
-- id is caller-supplied, same as CreateTarget and CreateAnnotation: the column has a DEFAULT.
INSERT INTO incidents (id, title, scope, from_at, to_at, status, notes, pinned, created_by, resolved_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING id, title, scope, from_at, to_at, status, notes, pinned, created_by, created_at, resolved_at;

-- name: GetIncident :one
SELECT id, title, scope, from_at, to_at, status, notes, pinned, created_by, created_at, resolved_at
FROM incidents
WHERE id = $1;

-- name: ListIncidents :many
-- The incidents listing, newest-created first.
SELECT id, title, scope, from_at, to_at, status, notes, pinned, created_by, created_at, resolved_at
FROM incidents
WHERE (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status')::text)
  AND (sqlc.narg('scope')::text IS NULL OR scope = sqlc.narg('scope')::text)
  AND (sqlc.narg('from_time')::timestamptz IS NULL OR
       coalesce(to_at, 'infinity'::timestamptz) >= sqlc.narg('from_time')::timestamptz)
  AND (sqlc.narg('to_time')::timestamptz IS NULL OR
       from_at < sqlc.narg('to_time')::timestamptz)
  AND (sqlc.narg('cur_time')::timestamptz IS NULL OR
       (created_at, id) < (sqlc.narg('cur_time')::timestamptz, sqlc.narg('cur_id')::uuid))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('lim');

-- name: UpdateIncidentStatus :one
-- One of THREE narrow updates, deliberately not one full replace; an incident evolves while several
-- people look.
UPDATE incidents
SET status = $2, resolved_at = $3
WHERE id = $1
RETURNING id, title, scope, from_at, to_at, status, notes, pinned, created_by, created_at, resolved_at;

-- name: UpdateIncidentNotes :one
UPDATE incidents
SET notes = $2
WHERE id = $1
RETURNING id, title, scope, from_at, to_at, status, notes, pinned, created_by, created_at, resolved_at;

-- name: UpdateIncidentPinned :one
UPDATE incidents
SET pinned = $2
WHERE id = $1
RETURNING id, title, scope, from_at, to_at, status, notes, pinned, created_by, created_at, resolved_at;

-- name: DeleteIncident :execrows
-- Nothing references an incident, so deleting one removes the incident and nothing else.
DELETE FROM incidents WHERE id = $1;

-- name: DeleteIncidentsBefore :execrows
-- Retention by resolved_at, and note what that does NOT match: an OPEN incident has a NULL
-- resolved_at.
DELETE FROM incidents
WHERE id IN (SELECT i.id FROM incidents i WHERE i.resolved_at < $1 ORDER BY i.resolved_at LIMIT $2);
