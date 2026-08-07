-- name: InsertTopologyEvent :execrows
INSERT INTO topology_events (event_seq, event_time, type, severity, scope, summary, details)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT ON CONSTRAINT topology_events_natural_key DO NOTHING;

-- name: ListTopologyEvents :many
SELECT id, event_seq, event_time, type, severity, scope, summary, details
FROM topology_events
WHERE (sqlc.narg('types')::text[]  IS NULL OR type = ANY(sqlc.narg('types')::text[]))
  AND (sqlc.narg('scope')::text    IS NULL OR scope = sqlc.narg('scope')::text)
  AND (sqlc.narg('from_time')::timestamptz IS NULL OR event_time >= sqlc.narg('from_time')::timestamptz)
  AND (sqlc.narg('to_time')::timestamptz   IS NULL OR event_time <  sqlc.narg('to_time')::timestamptz)
  AND (sqlc.narg('cur_time')::timestamptz  IS NULL OR
       (event_time, id) < (sqlc.narg('cur_time')::timestamptz, sqlc.narg('cur_id')::bigint))
ORDER BY event_time DESC, id DESC
LIMIT sqlc.arg('lim');

-- name: DeleteTopologyEventsBefore :execrows
-- The inner subquery aliases topology_events as te: sqlc v1.31.1's own query
-- analyzer (not real PostgreSQL -- verified this exact self-join resolves
-- unambiguously against a live postgres:17-alpine) reports "column reference
-- event_time is ambiguous" for the unaliased form the brief specifies
-- verbatim, because it does not scope-resolve the DELETE target table
-- against the subquery's own FROM. The alias is a no-op for Postgres and
-- changes nothing about the query's semantics or its plan.
DELETE FROM topology_events
WHERE id IN (SELECT te.id FROM topology_events te WHERE te.event_time < $1 ORDER BY te.event_time LIMIT $2);
