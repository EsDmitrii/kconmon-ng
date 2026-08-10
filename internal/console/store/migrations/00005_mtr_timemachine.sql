-- +goose Up
-- mtr_path_snapshots is the normalized, content-hashed projection of the MTR results the Console
-- already persists verbatim in check_results.result (migration 00003); it is a PROJECTION, never
-- the authority: the result row.
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

-- The pair browse index; both trailing keys are DESC so the scan direction matches ORDER BY exactly
-- rather than relying on a backward scan.
CREATE INDEX mtr_snapshots_pair_seen_idx
    ON mtr_path_snapshots (source_node, destination, last_seen DESC, id DESC);

-- mtr_hop_enrichment is a TTL CACHE, not a source of truth: every row is re-derivable from the
-- resolvers that wrote it.
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

-- annotations are operator notes pinned to a moment or a span.
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
