-- +goose Up
-- targets is an external probe destination: name.
CREATE TABLE targets (
    id         UUID        PRIMARY KEY,
    name       TEXT        NOT NULL UNIQUE CHECK (length(name) BETWEEN 1 AND 63),
    kind       TEXT        NOT NULL,  -- host | url
    address    TEXT        NOT NULL,
    labels     JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- check_definitions is a saved spec: what to probe.
CREATE TABLE check_definitions (
    id                     UUID        PRIMARY KEY,
    name                   TEXT        NOT NULL UNIQUE CHECK (length(name) BETWEEN 1 AND 63),
    source_selection       TEXT        NOT NULL,  -- all | per-zone | one-per-zone
    destination_kind       TEXT        NOT NULL,  -- node | target | adhoc
    destination_target_id  UUID        REFERENCES targets(id) ON DELETE RESTRICT,
    destination_address    TEXT        NOT NULL DEFAULT '',
    check_type             TEXT        NOT NULL,  -- tcp|udp|icmp|dns|http|mtr
    plane                  TEXT        NOT NULL DEFAULT 'pod',
    params                 JSONB       NOT NULL DEFAULT '{}'::jsonb,
    enabled                BOOLEAN     NOT NULL DEFAULT false,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX check_definitions_target_idx ON check_definitions (destination_target_id);

-- check_schedules binds a definition to a cadence (DATA.md §5.2 "one-shot / cron / interval /
-- continuous bindings").
CREATE TABLE check_schedules (
    id            UUID        PRIMARY KEY,
    definition_id UUID        NOT NULL REFERENCES check_definitions(id) ON DELETE CASCADE,
    kind          TEXT        NOT NULL,  -- once | interval | continuous  (cron: later milestone)
    interval_ns   BIGINT      NOT NULL DEFAULT 0,  -- nanoseconds, repo-wide convention
    run_at        TIMESTAMPTZ,                     -- kind='once' only
    enabled       BOOLEAN     NOT NULL DEFAULT false,
    last_fired_at TIMESTAMPTZ,
    next_fire_at  TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX check_schedules_due_idx ON check_schedules (next_fire_at) WHERE enabled;

-- +goose Down
DROP TABLE check_schedules;
DROP TABLE check_definitions;
DROP TABLE targets;
