-- name: UpsertPathSnapshot :one
-- The dedupe write (M5 Decision 2). A trace over a path the pair has already
-- taken bumps last_seen and trace_count on the existing row; a trace over a
-- new path inserts one. Which of the two happened is the observable the
-- caller needs -- "the route changed" is the whole point of this table -- and
-- (xmax = 0) is how PostgreSQL answers it: the RETURNING clause of an
-- INSERT ... ON CONFLICT DO UPDATE sees the tuple's xmax, which is zero for a
-- freshly inserted row and the updating transaction's id for a conflicting
-- one. Reported back rather than derived from a count, because :one gives no
-- rows-affected and a follow-up SELECT would race another replica's trace.
--
-- last_seen takes GREATEST rather than EXCLUDED outright: results arrive from
-- several agents through several replicas and a late-delivered older trace
-- must not walk the pair's "last seen" backwards. first_seen is never
-- touched on conflict, so the row keeps the moment the path was discovered.
-- hops is likewise never overwritten -- the stored payload is the FIRST
-- trace at this path, by design (migration 00005).
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
-- The MTR Explorer's left pane: every (source, destination) pair path history
-- knows about, with how many distinct routes it has taken and when it was
-- last traced. Unbounded by design -- the row count is pairs, not traces, so
-- it is bounded by the cluster's own size rather than by time.
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
-- The pair's route history, newest first. The WHERE/ORDER BY pair is written
-- to match mtr_snapshots_pair_seen_idx exactly: equality on the two leading
-- columns, then the (last_seen, id) keyset the cursor carries, then the
-- index's own DESC ordering -- so a pair with a long history pages without a
-- sort. Keeping the source/destination filters optional costs nothing here:
-- with both bound to real values the planner's custom plan folds the
-- IS NULL arms away and the index scan is what is left.
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
-- Retention by last_seen, not first_seen: a route the pair still takes is
-- still current however long ago it was first observed. s alias on the
-- subquery's own FROM for the sqlc v1.31.1 analyzer quirk documented on
-- DeleteRunsBefore (checks.sql).
DELETE FROM mtr_path_snapshots
WHERE id IN (SELECT s.id FROM mtr_path_snapshots s WHERE s.last_seen < $1 ORDER BY s.last_seen LIMIT $2);

-- name: GetEnrichment :many
-- The read half of the TTL cache. Callers ask for a whole trace's hop IPs at
-- once (a snapshot detail is up to 64 of them), so this is one round trip
-- with an array rather than one per hop. Rows the cache does not have are
-- simply absent from the result -- a miss is not an error, it is the signal
-- to resolve.
SELECT ip, rdns, asn, provider, geo, resolved_at
FROM mtr_hop_enrichment
WHERE ip = ANY(sqlc.arg('ips')::text[]);

-- name: PutEnrichment :exec
-- The write-back half, one statement for the whole batch. The batch travels
-- as ONE jsonb array expanded by jsonb_to_recordset rather than as six
-- parallel arrays: sqlc v1.31.1's own analyzer has no six-argument unnest in
-- its catalog ("function unnest(unknown, unknown, ...) does not exist"), and
-- six positional arrays is in any case a shape where one mis-ordered slice on
-- the Go side silently writes every row's provider into its rdns. One array
-- of objects cannot be mis-zipped.
--
-- ON CONFLICT DO UPDATE, not DO NOTHING: a re-resolve past the TTL is exactly
-- when the row must be replaced, and resolved_at moving is what makes the TTL
-- work at all.
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
-- The cache's retention sweep. Every row here is re-derivable from the
-- resolvers that wrote it, so this deletes cheerfully: the cost of being
-- wrong is one lookup, not lost data. e alias for the same analyzer quirk.
DELETE FROM mtr_hop_enrichment
WHERE ip IN (SELECT e.ip FROM mtr_hop_enrichment e WHERE e.resolved_at < $1 ORDER BY e.resolved_at LIMIT $2);
