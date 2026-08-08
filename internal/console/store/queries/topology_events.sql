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

-- name: OldestTopologyEventTime :one
-- The retention floor for the topology-at-t fold (M5 Task 9): the console can
-- only answer "what did the cluster look like at t" for a t at or after this
-- row, because the pruner has already deleted everything older and no fold can
-- invent what was deleted. ORDER BY + LIMIT 1 rather than MIN(event_time) for
-- two reasons: it is a single lookup on topology_events_time_idx, and an empty
-- table comes back as pgx.ErrNoRows (a NULL aggregate would need a nullable
-- column type the sqlc timestamptz->time.Time override deliberately does not
-- produce, and would then be indistinguishable from a genuine zero time).
SELECT event_time FROM topology_events ORDER BY event_time LIMIT 1;

-- name: ListTopologyEventsForFold :many
-- The topology-at-t fold input: every event of the given type at or before
-- 'at', OLDEST first, so replaying the rows in order reproduces the node/agent
-- set as of that instant. (event_time, id) is a total order -- id breaks ties
-- inside the same microsecond, which the natural key permits -- so the replay
-- is deterministic across replicas. Rides topology_events_type_time_idx.
--
-- No keyset paging: this returns the whole history up to 'at' in one shot,
-- bounded by 'lim' (store passes topologyFoldLimit). A fold is only correct
-- when it sees EVERY event from the beginning of retention, so a page boundary
-- would silently produce a wrong answer -- the limit is a blast-radius guard
-- that the store reports as truncated, never a pagination cursor.
SELECT id, event_time, details
FROM topology_events
WHERE type = sqlc.arg('type')::text
  AND event_time <= sqlc.arg('at')::timestamptz
ORDER BY event_time, id
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
