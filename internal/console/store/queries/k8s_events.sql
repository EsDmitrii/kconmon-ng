-- name: InsertK8sEvent :execrows
-- The idempotent capture write (M6 Decision 3). ON CONFLICT DO NOTHING over
-- (uid, resource_ver) is the same shape InsertTopologyEvent uses over its own
-- natural key, and for the same reason: the writer re-lists on every watch
-- expiry, so it re-offers every event it has already stored on every relist,
-- and "already there" is the NORMAL outcome rather than an error. :execrows
-- makes that observable -- 0 rows means the conflict fired, 1 means the
-- revision is new -- which is what the reader's stored|duplicate metric split
-- is built on.
--
-- Note the key is the PAIR: a recurring event keeps its uid and gets a new
-- resourceVersion and a bumped count, so each revision lands as its own row
-- and the timeline can show the recurrence.
INSERT INTO k8s_events (uid, resource_ver, event_time, kind, name, namespace, reason, type, message, count)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT ON CONSTRAINT k8s_events_uid_rv DO NOTHING;

-- name: ListK8sEvents :many
-- The timeline's K8s source, newest first. Every filter is optional and every
-- one of them is spelled as a narg so an unbound filter folds out of the plan
-- entirely -- with name bound the WHERE/ORDER BY pair matches
-- k8s_events_name_time_idx exactly (equality on the leading column, then the
-- (event_time DESC, id DESC) keyset), and with name unbound the same query
-- rides k8s_events_time_idx.
--
-- The window is half-open, [from, to), the same convention every other
-- listing in this package uses. The cursor is the (event_time, id) bigint
-- keyset, i.e. the topology_events family, because the primary key here is a
-- BIGSERIAL rather than a UUID.
SELECT id, uid, resource_ver, event_time, kind, name, namespace, reason, type, message, count
FROM k8s_events
WHERE (sqlc.narg('name')::text IS NULL OR name = sqlc.narg('name')::text)
  AND (sqlc.narg('kind')::text IS NULL OR kind = sqlc.narg('kind')::text)
  AND (sqlc.narg('ev_type')::text IS NULL OR type = sqlc.narg('ev_type')::text)
  AND (sqlc.narg('from_time')::timestamptz IS NULL OR event_time >= sqlc.narg('from_time')::timestamptz)
  AND (sqlc.narg('to_time')::timestamptz IS NULL OR event_time < sqlc.narg('to_time')::timestamptz)
  AND (sqlc.narg('cur_time')::timestamptz IS NULL OR
       (event_time, id) < (sqlc.narg('cur_time')::timestamptz, sqlc.narg('cur_id')::bigint))
ORDER BY event_time DESC, id DESC
LIMIT sqlc.arg('lim');

-- name: DeleteK8sEventsBefore :execrows
-- Retention by event_time: the capture ages out with the window it describes.
-- k alias on the subquery's own FROM for the sqlc v1.31.1 analyzer quirk
-- documented on DeleteTopologyEventsBefore (topology_events.sql).
DELETE FROM k8s_events
WHERE id IN (SELECT k.id FROM k8s_events k WHERE k.event_time < $1 ORDER BY k.event_time LIMIT $2);
