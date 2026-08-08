-- name: CreateAnnotation :one
-- id is caller-supplied, same as CreateTarget (targets.sql): the column has a
-- DEFAULT, but minting the UUID in Go keeps the package's one id story and
-- makes a retried create identifiable rather than a second mark on the chart.
INSERT INTO annotations (id, start_at, end_at, scope, text, created_by)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, start_at, end_at, scope, text, created_by, created_at;

-- name: ListAnnotations :many
-- The chart-marker query: every annotation whose interval OVERLAPS the
-- requested window, newest first. An instant mark (end_at NULL) is treated as
-- the zero-length interval [start_at, start_at], which coalesce spells out --
-- without it a NULL end_at would fail the lower-bound test and every instant
-- mark, i.e. most of them, would vanish from a bounded window.
--
-- The window is half-open: start_at < 'to' and the mark's end >= 'from'. A
-- NULL bound is unbounded on that side, the same "narg means no filter"
-- convention every other listing in this package uses.
--
-- scope's filter is the SQL NULL / '' distinction, not the empty-string one
-- every other listing here uses: '' is the GLOBAL scope, a real value a
-- caller must be able to ask for, so "no filter" is spelled as a NULL
-- argument and an empty-string argument selects exactly the global marks.
--
-- (start_at DESC, id DESC) is annotations_time_idx's own order, so the
-- listing pages without a sort.
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
-- No cascade and nothing references an annotation: deleting a mark removes
-- the mark and nothing else. M5 has no edit (Decision 10), so delete-and-
-- recreate is the only correction path and it has to be clean.
DELETE FROM annotations WHERE id = $1;

-- name: DeleteAnnotationsBefore :execrows
-- Retention by start_at: an annotation is pinned to the moment it describes,
-- so it ages out with the data it annotates rather than with when it was
-- typed. a alias on the subquery's own FROM for the sqlc v1.31.1 analyzer
-- quirk documented on DeleteRunsBefore (checks.sql).
DELETE FROM annotations
WHERE id IN (SELECT a.id FROM annotations a WHERE a.start_at < $1 ORDER BY a.start_at LIMIT $2);
