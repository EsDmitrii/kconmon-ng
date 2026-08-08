-- +goose Up
-- mtr_path_snapshots is the normalized, content-hashed projection of the MTR
-- results the Console already persists verbatim in check_results.result
-- (migration 00003). It is a PROJECTION, never the authority: the result row
-- is, and a snapshot that fails to be written loses nothing but history.
--
-- path_hash is a hex sha256 over the ORDERED hop IP list and nothing else
-- (M5 Decision 2). Not RTTs -- they jitter on every trace, so folding them in
-- would make every trace a "new path" and defeat dedupe entirely. Not
-- hostnames -- rDNS is enrichment (mtr_hop_enrichment below), resolved long
-- after the trace and mutable, so a PTR record change would look like a route
-- change. The UNIQUE (source_node, destination, path_hash) is therefore the
-- dedupe key: one row per distinct route a pair has ever taken, with
-- first_seen/last_seen/trace_count maintained on conflict.
--
-- destination is a node NAME or a target NAME, never an address: it is the
-- same metric-safe label the rest of the system uses (migration 00004's
-- comment on targets.name), and an address here would eventually leak into a
-- label value.
--
-- hops carries the FULL payload (number, ip, hostname, rttNs, lossRatio) of
-- the FIRST trace observed at this path, not a running average: the row's job
-- is to describe the route, and one concrete trace of it is an honest sample
-- where an average across weeks would be a number nothing ever measured.
--
-- run_id is ON DELETE SET NULL, not CASCADE: the retention sweep deletes old
-- check_runs, and a path that outlives the run that first produced it is the
-- point of keeping path history at all.
CREATE TABLE mtr_path_snapshots (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    source_node  TEXT        NOT NULL,
    destination  TEXT        NOT NULL,   -- node name or target NAME, never an address
    path_hash    TEXT        NOT NULL,   -- hex sha256 of the ordered hop IPs
    hop_count    INT         NOT NULL,
    hops         JSONB       NOT NULL,   -- [{number,ip,hostname,rttNs,lossRatio}] of the first trace at this path
    first_seen   TIMESTAMPTZ NOT NULL,
    last_seen    TIMESTAMPTZ NOT NULL,
    trace_count  BIGINT      NOT NULL DEFAULT 1,
    run_id       UUID        REFERENCES check_runs(id) ON DELETE SET NULL,
    CONSTRAINT mtr_path_snapshots_pair_hash UNIQUE (source_node, destination, path_hash)
);

-- The pair browse index. Its column order is the query's: equality on
-- (source_node, destination), then the (last_seen DESC, id DESC) keyset the
-- listing pages by -- so ListPathSnapshots is an index range scan with no
-- sort, however long a pair's history grows. Both trailing keys are DESC so
-- the scan direction matches ORDER BY exactly rather than relying on a
-- backward scan.
CREATE INDEX mtr_snapshots_pair_seen_idx
    ON mtr_path_snapshots (source_node, destination, last_seen DESC, id DESC);

-- mtr_hop_enrichment is a TTL CACHE, not a source of truth (M5 Decision 4):
-- every row is re-derivable from the resolvers that wrote it, so the retention
-- sweep can drop it wholesale and the next read simply re-resolves. resolved_at
-- is the TTL anchor; there is no background refresher in M5, so an unread IP
-- costs nothing and a stale one costs exactly one lookup.
--
-- ip is the primary key rather than a surrogate: the cache is keyed by the
-- thing being looked up, and there is nothing else to say about an IP twice.
CREATE TABLE mtr_hop_enrichment (
    ip          TEXT        PRIMARY KEY,
    rdns        TEXT        NOT NULL DEFAULT '',
    asn         BIGINT      NOT NULL DEFAULT 0,
    provider    TEXT        NOT NULL DEFAULT '',
    geo         JSONB       NOT NULL DEFAULT '{}'::jsonb,
    resolved_at TIMESTAMPTZ NOT NULL
);

-- The resolved_at sweep index. Without it the retention pruner's
-- "oldest resolved_at first, LIMIT n" batch would seq-scan the whole cache on
-- every batch of every sweep.
CREATE INDEX mtr_hop_enrichment_resolved_idx ON mtr_hop_enrichment (resolved_at);

-- annotations are operator notes pinned to a moment or a span (M5 Decision
-- 10). end_at NULL means an instant mark, not "still open": an annotation is
-- a mark, not a document, which is also why M5 has create/list/delete and no
-- edit. scope '' is global; any other value names a node, a pair or a target
-- and is matched exactly -- it is a filter key, never a Prometheus label
-- value, and neither it nor text is ever exported as one.
CREATE TABLE annotations (
    id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    start_at   TIMESTAMPTZ NOT NULL,
    end_at     TIMESTAMPTZ,                -- NULL = instant mark
    scope      TEXT        NOT NULL DEFAULT '',
    text       TEXT        NOT NULL,
    created_by TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- (start_at DESC, id DESC) is both the listing's keyset order and the
-- retention sweep's scan order.
CREATE INDEX annotations_time_idx ON annotations (start_at DESC, id DESC);

-- +goose Down
DROP TABLE annotations;
DROP TABLE mtr_hop_enrichment;
DROP TABLE mtr_path_snapshots;
