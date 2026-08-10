-- +goose Up
-- check_runs is one fan-out execution: one spec, N (source, destination) pairs.
CREATE TABLE check_runs (
    id             UUID        PRIMARY KEY,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at     TIMESTAMPTZ,
    finished_at    TIMESTAMPTZ,
    status         TEXT        NOT NULL,  -- pending | running | succeeded | partial | failed | cancelled
    check_type     TEXT        NOT NULL,
    plane          TEXT        NOT NULL,
    spec           JSONB       NOT NULL,
    initiator_kind TEXT        NOT NULL,  -- authz.SubjectKind
    initiator_id   TEXT        NOT NULL,
    pair_total     INT         NOT NULL,
    pair_ok        INT         NOT NULL DEFAULT 0,
    pair_failed    INT         NOT NULL DEFAULT 0
);
CREATE INDEX check_runs_created_idx ON check_runs (created_at DESC, id DESC);

CREATE TABLE check_results (
    id               BIGSERIAL   PRIMARY KEY,
    run_id           UUID        NOT NULL REFERENCES check_runs(id) ON DELETE CASCADE,
    source_node      TEXT        NOT NULL,
    destination_node TEXT        NOT NULL,
    success          BOOLEAN     NOT NULL,
    duration_ns      BIGINT      NOT NULL,   -- nanoseconds, repo-wide convention
    error            TEXT        NOT NULL DEFAULT '',
    -- The agent's model.CheckResult verbatim, exactly as the controller returned it
    -- (internal/controller/diagnostics.go writes res.GetDetailsJson unmodified).
    result           JSONB       NOT NULL,
    recorded_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT check_results_pair_unique UNIQUE (run_id, source_node, destination_node)
);

-- +goose Down
DROP TABLE check_results;
DROP TABLE check_runs;
