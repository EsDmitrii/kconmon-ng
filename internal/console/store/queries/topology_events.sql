-- name: InsertTopologyEvent :execrows
INSERT INTO topology_events (event_seq, event_time, type, severity, scope, summary, details)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT ON CONSTRAINT topology_events_natural_key DO NOTHING;

-- name: ListTopologyEventsByScopeNode :many
--
-- The PAIR-AWARE filter a node or target card needs: every event whose scope names this node, on
-- either side of a pair scope ("node-7" or "node-7→node-9" or "node-9→node-7").
--
-- It is its own query, and a UNION ALL rather than an OR, because of what the ordering does to a
-- disjunction. The filter used to be three ORed predicates inside ListTopologyEvents, one of them
-- `scope LIKE '%→' || $node` -- a leading wildcard, which no btree can answer. Splitting the scope
-- into two indexed generated columns (migration 00016) was not enough on its own: with
-- `ORDER BY event_time DESC LIMIT n` over an OR, PostgreSQL still prefers to walk the time index
-- and filter, so it read the whole table either way. Measured on 300 000 rows, scope naming a node
-- with no events (a drained node, a typo -- the case deep paging and stale links produce):
--   OR over the generated columns: 300 000 rows removed by filter, 4 238 buffers, 18 ms
--   this shape:                    0 rows removed,                     6 buffers, 0.06 ms
--
-- Each arm is an index-ordered scan that stops at LIMIT, and Merge Append interleaves them without
-- a sort. The second arm excludes rows the first already returned: a NON-pair scope has
-- scope_left = scope_right = scope, so without it every node-scoped event would appear twice.
(
SELECT id, event_seq, event_time, type, severity, scope, summary, details
FROM topology_events
WHERE scope_left = sqlc.arg('scope_node')::text
  AND (sqlc.narg('types')::text[] IS NULL OR type = ANY(sqlc.narg('types')::text[]))
  AND (sqlc.narg('from_time')::timestamptz IS NULL OR event_time >= sqlc.narg('from_time')::timestamptz)
  AND (sqlc.narg('to_time')::timestamptz   IS NULL OR event_time <  sqlc.narg('to_time')::timestamptz)
  AND (sqlc.narg('cur_time')::timestamptz  IS NULL OR
       (event_time, id) < (sqlc.narg('cur_time')::timestamptz, sqlc.narg('cur_id')::bigint))
ORDER BY event_time DESC, id DESC
LIMIT sqlc.arg('lim')
)
UNION ALL
(
SELECT id, event_seq, event_time, type, severity, scope, summary, details
FROM topology_events
WHERE scope_right = sqlc.arg('scope_node')::text
  AND scope_left <> sqlc.arg('scope_node')::text
  AND (sqlc.narg('types')::text[] IS NULL OR type = ANY(sqlc.narg('types')::text[]))
  AND (sqlc.narg('from_time')::timestamptz IS NULL OR event_time >= sqlc.narg('from_time')::timestamptz)
  AND (sqlc.narg('to_time')::timestamptz   IS NULL OR event_time <  sqlc.narg('to_time')::timestamptz)
  AND (sqlc.narg('cur_time')::timestamptz  IS NULL OR
       (event_time, id) < (sqlc.narg('cur_time')::timestamptz, sqlc.narg('cur_id')::bigint))
ORDER BY event_time DESC, id DESC
LIMIT sqlc.arg('lim')
)
ORDER BY event_time DESC, id DESC
LIMIT sqlc.arg('lim');

-- name: ListTopologyEvents :many
-- scope is the EXACT filter. The pair-aware one lives in ListTopologyEventsByScopeNode.
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
