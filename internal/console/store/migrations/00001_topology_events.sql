-- +goose Up
-- topology_events is the durable projection of the controller event stream that events.Ingester
-- already receives (internal/console/events); every console replica ingests the same stream and
-- writes the same row.
CREATE TABLE topology_events (
    id          BIGSERIAL   PRIMARY KEY,
    event_seq   BIGINT      NOT NULL,
    event_time  TIMESTAMPTZ NOT NULL,
    type        TEXT        NOT NULL,
    severity    TEXT        NOT NULL,
    scope       TEXT        NOT NULL,
    summary     TEXT        NOT NULL,
    details     JSONB       NOT NULL,
    ingested_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT topology_events_natural_key UNIQUE (event_seq, event_time)
);

-- Keyset pagination is (event_time DESC, id DESC); every list query and the
-- retention sweep ride one of these three indexes.
CREATE INDEX topology_events_time_idx       ON topology_events (event_time DESC, id DESC);
CREATE INDEX topology_events_type_time_idx  ON topology_events (type, event_time DESC, id DESC);
CREATE INDEX topology_events_scope_time_idx ON topology_events (scope, event_time DESC, id DESC);

-- +goose Down
DROP TABLE topology_events;
