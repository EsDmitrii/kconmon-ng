-- name: CreateIncident :one
-- id is caller-supplied, same as CreateTarget and CreateAnnotation: the column
-- has a DEFAULT, but minting the UUID in Go keeps the package's one id story
-- and makes a retried create identifiable rather than a second incident.
INSERT INTO incidents (id, title, scope, from_at, to_at, status, notes, pinned, created_by, resolved_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING id, title, scope, from_at, to_at, status, notes, pinned, created_by, created_at, resolved_at;

-- name: GetIncident :one
SELECT id, title, scope, from_at, to_at, status, notes, pinned, created_by, created_at, resolved_at
FROM incidents
WHERE id = $1;

-- name: ListIncidents :many
-- The incidents listing, newest-created first. status and scope are exact
-- matches; from/to bound the window an incident's OWN RANGE must overlap, not
-- the window it was created in -- an incident that began before the window and
-- is still open is exactly the one an operator looking at that window needs.
--
-- to_at NULL is an OPEN-ENDED range, so it coalesces to 'infinity' rather than
-- back onto from_at (which is what annotations does, because there NULL means
-- an instant mark -- the two columns look alike and mean opposite things).
--
-- scope takes the annotations NULL/'' treatment: '' is the GLOBAL scope, a real
-- value a caller must be able to ask for, so "no filter" is a SQL NULL
-- argument and an empty-string argument selects exactly the global incidents.
--
-- (created_at DESC, id DESC) is both index's trailing key order, so a listing
-- filtered by status rides incidents_status_created_idx and one filtered by
-- scope rides incidents_scope_idx, neither with a sort.
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
-- One of THREE narrow updates, deliberately not one full replace. An incident
-- evolves while several people look at it: a full-replace PUT would let a
-- collaborator's stale copy of `notes` silently overwrite an edit made a second
-- earlier, and the three fields that actually change (status, notes, pinned)
-- change for three unrelated reasons. PATCH semantics are assembled from these
-- in httpapi; the store's surface stays this narrow on purpose.
--
-- resolved_at travels WITH status, never separately: it is status' witness, and
-- reopening (status back to 'open' with a NULL resolved_at) has to clear it in
-- the same statement or a reopened incident keeps a resolution time.
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
-- Nothing references an incident, so deleting one removes the incident and
-- nothing else. Its permalink (/investigate?incident={id}) simply stops
-- resolving, which is the honest outcome for a link to something deleted.
DELETE FROM incidents WHERE id = $1;

-- name: DeleteIncidentsBefore :execrows
-- Retention by resolved_at, and note what that does NOT match: an OPEN
-- incident has a NULL resolved_at, and `NULL < $1` is NULL, never true, so an
-- open incident can never be selected here however old it is. That is the
-- point -- an investigation nobody closed is not stale data, it is unfinished
-- work, and the retention sweep must not close it by deleting it.
--
-- i alias on the subquery's own FROM for the sqlc v1.31.1 analyzer quirk
-- documented on DeleteRunsBefore (checks.sql).
DELETE FROM incidents
WHERE id IN (SELECT i.id FROM incidents i WHERE i.resolved_at < $1 ORDER BY i.resolved_at LIMIT $2);
