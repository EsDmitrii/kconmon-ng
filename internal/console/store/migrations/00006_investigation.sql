-- +goose Up
-- the four tables: the K8s event capture the Investigate timeline reads.
CREATE TABLE k8s_events (
    id           BIGSERIAL PRIMARY KEY,
    uid          TEXT NOT NULL,           -- k8s event UID
    resource_ver TEXT NOT NULL,
    event_time   TIMESTAMPTZ NOT NULL,
    kind         TEXT NOT NULL,           -- Node | Pod
    name         TEXT NOT NULL,           -- node name / pod name
    namespace    TEXT NOT NULL DEFAULT '',
    reason       TEXT NOT NULL,
    type         TEXT NOT NULL,           -- Normal | Warning
    message      TEXT NOT NULL,
    count        INT  NOT NULL DEFAULT 1,
    CONSTRAINT k8s_events_uid_rv UNIQUE (uid, resource_ver)
);

-- The unfiltered timeline scan and the retention sweep's scan order, both
-- DESC so ORDER BY event_time DESC, id DESC comes out of the index rather than
-- out of a sort.
CREATE INDEX k8s_events_time_idx ON k8s_events (event_time DESC, id DESC);

-- The scoped timeline: equality on name, then the (event_time DESC, id DESC)
-- keyset the listing pages by -- so "what happened to THIS node in this
-- window" is an index range scan with no sort, however long the capture runs.
CREATE INDEX k8s_events_name_time_idx ON k8s_events (name, event_time DESC, id DESC);

-- incidents are annotations-class, not tickets: a title, a scope; the permalink is
-- /investigate?incident={id}.
CREATE TABLE incidents (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    title       TEXT NOT NULL,
    scope       TEXT NOT NULL DEFAULT '',
    from_at     TIMESTAMPTZ NOT NULL,
    to_at       TIMESTAMPTZ,
    status      TEXT NOT NULL DEFAULT 'open',   -- open | resolved
    notes       TEXT NOT NULL DEFAULT '',
    pinned      JSONB NOT NULL DEFAULT '[]',
    created_by  TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at TIMESTAMPTZ
);

-- The Overview card's query ("open incidents, newest first") and the listing's
-- own status filter, with the (created_at DESC, id DESC) keyset trailing so
-- the page comes out of the index unsorted.
CREATE INDEX incidents_status_created_idx ON incidents (status, created_at DESC, id DESC);

-- The scoped listing: "every incident that ever named this node/pair/target".
CREATE INDEX incidents_scope_idx ON incidents (scope, created_at DESC, id DESC);

-- They render as markArea on scoped charts and as timeline rows.
CREATE TABLE maintenance_windows (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    scope      TEXT NOT NULL DEFAULT '',
    start_at   TIMESTAMPTZ NOT NULL,
    end_at     TIMESTAMPTZ NOT NULL,
    reason     TEXT NOT NULL,
    created_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT maintenance_end_after_start CHECK (end_at > start_at)
);

-- Both the listing's keyset order and the retention sweep's scan order.
CREATE INDEX maintenance_time_idx ON maintenance_windows (start_at DESC, id DESC);

-- webhooks are outbound incident-lifecycle notifications; one row per endpoint.
CREATE TABLE webhooks (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name         TEXT NOT NULL UNIQUE,
    url          TEXT NOT NULL,
    events       TEXT[] NOT NULL,        -- closed set: incident.created|incident.resolved|incident.reopened
    secret_enc   BYTEA NOT NULL,         -- AES-GCM(config key)
    enabled      BOOLEAN NOT NULL DEFAULT true,
    last_status  TEXT NOT NULL DEFAULT '',
    last_attempt TIMESTAMPTZ,
    failures     INT NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE webhooks;
DROP TABLE maintenance_windows;
DROP TABLE incidents;
DROP TABLE k8s_events;
