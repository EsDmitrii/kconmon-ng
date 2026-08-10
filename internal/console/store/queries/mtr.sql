-- name: UpsertPathSnapshot :one
-- A trace over a path the pair has already taken bumps last_seen and trace_count on the existing
-- row.
INSERT INTO mtr_path_snapshots (
    id, source_node, destination, path_hash, hop_count, hops,
    first_seen, last_seen, trace_count, run_id
)
VALUES (
    sqlc.arg('id'), sqlc.arg('source_node'), sqlc.arg('destination'), sqlc.arg('path_hash'),
    sqlc.arg('hop_count'), sqlc.arg('hops'),
    sqlc.arg('seen_at'), sqlc.arg('seen_at'), 1, sqlc.narg('run_id')
)
ON CONFLICT ON CONSTRAINT mtr_path_snapshots_pair_hash DO UPDATE
SET last_seen = GREATEST(mtr_path_snapshots.last_seen, EXCLUDED.last_seen),
    trace_count = mtr_path_snapshots.trace_count + 1
RETURNING id, source_node, destination, path_hash, hop_count, hops,
          first_seen, last_seen, trace_count, run_id,
          (xmax = 0) AS inserted;

-- name: ListMTRDestinations :many
-- The MTR Explorer's left pane: every (source, destination) pair path history knows about.
SELECT source_node,
       destination,
       count(*)::bigint AS snapshot_count,
       sum(trace_count)::bigint AS trace_count,
       min(first_seen)::timestamptz AS first_seen,
       max(last_seen)::timestamptz AS last_seen
FROM mtr_path_snapshots
GROUP BY source_node, destination
ORDER BY max(last_seen) DESC, source_node, destination;

-- name: ListPathSnapshots :many
-- The pair's route history.
SELECT id, source_node, destination, path_hash, hop_count, hops,
       first_seen, last_seen, trace_count, run_id
FROM mtr_path_snapshots
WHERE (sqlc.narg('source_node')::text IS NULL OR source_node = sqlc.narg('source_node')::text)
  AND (sqlc.narg('destination')::text IS NULL OR destination = sqlc.narg('destination')::text)
  AND (sqlc.narg('cur_time')::timestamptz IS NULL OR
       (last_seen, id) < (sqlc.narg('cur_time')::timestamptz, sqlc.narg('cur_id')::uuid))
ORDER BY last_seen DESC, id DESC
LIMIT sqlc.arg('lim');

-- name: GetPathSnapshot :one
SELECT id, source_node, destination, path_hash, hop_count, hops,
       first_seen, last_seen, trace_count, run_id
FROM mtr_path_snapshots
WHERE id = $1;

-- name: DeletePathSnapshotsBefore :execrows
-- Retention by last_seen, not first_seen: a route the pair still takes is still current however
-- long ago it was first observed.
DELETE FROM mtr_path_snapshots
WHERE id IN (SELECT s.id FROM mtr_path_snapshots s WHERE s.last_seen < $1 ORDER BY s.last_seen LIMIT $2);

-- name: GetEnrichment :many
-- The read half of the TTL cache; callers ask for a whole trace's hop IPs at once (a snapshot
-- detail is up to 64 of them).
SELECT ip, rdns, asn, provider, geo, resolved_at
FROM mtr_hop_enrichment
WHERE ip = ANY(sqlc.arg('ips')::text[]);

-- name: PutEnrichment :exec
-- The write-back half, one statement for the whole batch; the batch travels as ONE jsonb array
-- expanded by jsonb_to_recordset rather than as six parallel arrays.
INSERT INTO mtr_hop_enrichment (ip, rdns, asn, provider, geo, resolved_at)
SELECT r.ip, r.rdns, r.asn, r.provider, r.geo, r.resolved_at
FROM jsonb_to_recordset(sqlc.arg('rows')::jsonb)
     AS r(ip text, rdns text, asn bigint, provider text, geo jsonb, resolved_at timestamptz)
ON CONFLICT (ip) DO UPDATE
SET rdns = EXCLUDED.rdns,
    asn = EXCLUDED.asn,
    provider = EXCLUDED.provider,
    geo = EXCLUDED.geo,
    resolved_at = EXCLUDED.resolved_at;

-- name: DeleteEnrichmentBefore :execrows
-- The cache's retention sweep.
DELETE FROM mtr_hop_enrichment
WHERE ip IN (SELECT e.ip FROM mtr_hop_enrichment e WHERE e.resolved_at < $1 ORDER BY e.resolved_at LIMIT $2);
