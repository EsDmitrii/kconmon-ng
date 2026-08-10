-- name: InsertTopologyEvent :execrows
INSERT INTO topology_events (event_seq, event_time, type, severity, scope, summary, details)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT ON CONSTRAINT topology_events_natural_key DO NOTHING;

-- name: ListTopologyEvents :many
-- scope is the EXACT filter. scope_node is the pair-aware one a node/target card needs.
SELECT id, event_seq, event_time, type, severity, scope, summary, details
FROM topology_events
WHERE (sqlc.narg('types')::text[]  IS NULL OR type = ANY(sqlc.narg('types')::text[]))
  AND (sqlc.narg('scope')::text    IS NULL OR scope = sqlc.narg('scope')::text)
  AND (sqlc.narg('scope_node')::text IS NULL
       OR scope = sqlc.narg('scope_node')::text
       OR scope LIKE replace(replace(replace(sqlc.narg('scope_node')::text, '\', '\\'), '%', '\%'), '_', '\_') || '→%' ESCAPE '\'
       OR scope LIKE '%→' || replace(replace(replace(sqlc.narg('scope_node')::text, '\', '\\'), '%', '\%'), '_', '\_') ESCAPE '\')
  AND (sqlc.narg('from_time')::timestamptz IS NULL OR event_time >= sqlc.narg('from_time')::timestamptz)
  AND (sqlc.narg('to_time')::timestamptz   IS NULL OR event_time <  sqlc.narg('to_time')::timestamptz)
  AND (sqlc.narg('cur_time')::timestamptz  IS NULL OR
       (event_time, id) < (sqlc.narg('cur_time')::timestamptz, sqlc.narg('cur_id')::bigint))
ORDER BY event_time DESC, id DESC
LIMIT sqlc.arg('lim');

-- name: OldestTopologyEventTime :one
-- The retention floor for the topology-at-t fold.
SELECT event_time FROM topology_events ORDER BY event_time LIMIT 1;

-- name: ListTopologyEventsForFold :many
-- A fold is only correct when it sees EVERY event from the beginning of retention.
SELECT id, event_time, details
FROM topology_events
WHERE type = sqlc.arg('type')::text
  AND event_time <= sqlc.arg('at')::timestamptz
ORDER BY event_time, id
LIMIT sqlc.arg('lim');

-- name: DeleteTopologyEventsBefore :execrows
-- The inner subquery aliases topology_events as te: sqlc v1.31.1's own query analyzer.
DELETE FROM topology_events
WHERE id IN (SELECT te.id FROM topology_events te WHERE te.event_time < $1 ORDER BY te.event_time LIMIT $2);
