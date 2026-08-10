-- name: InsertK8sEvent :execrows
-- The idempotent capture write; ON CONFLICT DO NOTHING over (uid, resource_ver) is the same shape
-- InsertTopologyEvent uses over its own natural key.
INSERT INTO k8s_events (uid, resource_ver, event_time, kind, name, namespace, reason, type, message, count)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT ON CONSTRAINT k8s_events_uid_rv DO NOTHING;

-- name: ListK8sEvents :many
-- The timeline's K8s source; the cursor is the (event_time, id) bigint keyset, i.e. the
-- topology_events family.
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
DELETE FROM k8s_events
WHERE id IN (SELECT k.id FROM k8s_events k WHERE k.event_time < $1 ORDER BY k.event_time LIMIT $2);
