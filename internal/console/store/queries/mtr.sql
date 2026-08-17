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
-- The MTR Explorer's left pane: the (source, destination) pairs path history knows about.
--
-- BOUNDED. The row count here is pairs — sources x destinations — not "a handful, like the webhook
-- list": a hundred nodes is ten thousand rows, aggregated over the whole snapshot table on every
-- request, materialised in the store and marshalled by the handler, for any caller holding
-- mtr:read. The limit is the caller's, and the handler caps it.
--
-- PAGED on the pair itself, not on last_seen. The pane displays most-recently-traced first, but a
-- keyset cursor over a mutable sort key drops rows -- see ListPathSnapshots below, which spells out
-- the same trap -- and every repeat trace bumps last_seen. (source_node, destination) never changes,
-- so walking it is complete by construction and the caller sorts the assembled set for display.
-- Before this the listing was capped with no way to ask for the rest: the pairs past the cap were
-- missing from the Explorer entirely and every per-destination total was short by their counts.
SELECT source_node,
       destination,
       count(*)::bigint AS snapshot_count,
       sum(trace_count)::bigint AS trace_count,
       min(first_seen)::timestamptz AS first_seen,
       max(last_seen)::timestamptz AS last_seen
FROM mtr_path_snapshots
WHERE (sqlc.arg('has_cursor')::boolean = false
       OR (source_node, destination) > (sqlc.arg('cursor_source')::text, sqlc.arg('cursor_destination')::text))
GROUP BY source_node, destination
ORDER BY source_node, destination
LIMIT sqlc.arg('lim');

-- name: ListPathSnapshots :many
-- The pair's route history, newest ROUTE first.
--
-- Ordered and paged on first_seen, which never changes after the insert — not on last_seen, which
-- every repeat trace bumps (UpsertPathSnapshot's ON CONFLICT). A keyset cursor over a mutable sort
-- key drops rows: the reader takes page 1 (the routes with the largest last_seen), a route sitting
-- just below the cursor is re-traced before they press "Load older", its last_seen jumps ABOVE the
-- cursor, and page 2's predicate excludes it — from a page it was never on. The route then exists
-- in the database and nowhere in the console, with nothing on screen admitting a row went missing.
-- mergeSnapshots on the client was written for the mirror case (a row handed out twice); it cannot
-- invent one that was never sent.
--
-- "When this route first appeared" is also the spine the changes timeline already positions its
-- markers on, so the list and the strip now agree on what "older" means. last_seen stays on every
-- row, which is where "most recently traced" is read.
SELECT id, source_node, destination, path_hash, hop_count, hops,
       first_seen, last_seen, trace_count, run_id
FROM mtr_path_snapshots
WHERE (sqlc.narg('source_node')::text IS NULL OR source_node = sqlc.narg('source_node')::text)
  AND (sqlc.narg('destination')::text IS NULL OR destination = sqlc.narg('destination')::text)
  AND (sqlc.narg('cur_time')::timestamptz IS NULL OR
       (first_seen, id) < (sqlc.narg('cur_time')::timestamptz, sqlc.narg('cur_id')::uuid))
ORDER BY first_seen DESC, id DESC
LIMIT sqlc.arg('lim');

-- name: ListPathTraces :many
-- The INDIVIDUAL traces behind a route row. mtr_path_snapshots folds every trace that walked one
-- path into a single row with a trace_count; these are the rows that count was counted over, still
-- carrying their own clock and their own per-hop readings.
--
-- Bounded by the snapshot's own [first_seen, last_seen] window by the caller, and paged: a route
-- that held for a day can have thousands. The path IDENTITY is not checked here — result is JSONB
-- and the hash is computed in Go (checks.TraceFromResult) exactly as the projector computed it —
-- so the caller filters what this returns.
SELECT r.id, r.run_id, r.source_node, r.destination_node, r.success, r.duration_ns, r.error,
       r.result, r.recorded_at, r.sample_seq
FROM check_results r
    JOIN check_runs run ON run.id = r.run_id
-- MTR rows ONLY. check_results holds every check type under the same (source, destination), so a TCP
-- or ICMP check running between the same pair in the same window used to consume the LIMIT and the
-- handler then dropped those rows in Go — whole pages came back empty and the operator paged blindly
-- through a route the console said had N traces.
WHERE run.check_type = 'mtr'
  AND r.source_node = sqlc.arg('source_node')::text
  AND r.destination_node = sqlc.arg('destination')::text
  AND r.recorded_at >= sqlc.arg('from_time')::timestamptz
  AND r.recorded_at <= sqlc.arg('to_time')::timestamptz
  AND (sqlc.narg('cur_time')::timestamptz IS NULL OR
       (r.recorded_at, r.id) < (sqlc.narg('cur_time')::timestamptz, sqlc.narg('cur_id')::bigint))
ORDER BY r.recorded_at DESC, r.id DESC
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
