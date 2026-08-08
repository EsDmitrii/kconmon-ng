-- +goose Up
-- M6's four tables: the K8s event capture the Investigate timeline reads, and
-- the three CRUD resources (incidents, maintenance windows, webhooks) that
-- turn an investigation into something saveable, shareable and announceable.
--
-- k8s_events is a CAPTURE, never an authority: the cluster's own event log is,
-- and this table holds the filtered slice of it the console is allowed to see
-- (M6 Decision 3 -- node events for nodes present in the fleet topology, pod
-- events from the release namespace; an unfiltered cluster firehose would be a
-- cardinality and a privacy bug).
--
-- (uid, resource_ver) is the dedupe key, and it is TWO columns rather than one
-- for a reason. A Kubernetes Event is mutable: the same uid comes back with a
-- bumped count and a new resourceVersion every time the reason recurs. Keying
-- on uid alone would keep the FIRST observation and silently drop every later
-- count bump; keying on the pair keeps each observed revision as its own row,
-- so the timeline shows when the event recurred, and a re-list after a watch
-- expiry (which redelivers every current object) inserts nothing new. That is
-- what makes InsertK8sEvent idempotent under the reader's own relist loop.
--
-- name is a node name or a pod name -- the same metric-safe label the rest of
-- the system uses (migration 00004's comment on targets.name). namespace is ''
-- for the cluster-scoped Node events.
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

-- incidents are annotations-class, not tickets (M6 Decision 7): a title, a
-- scope, a range, notes, and the findings the operator pinned while looking at
-- the timeline. The permalink is /investigate?incident={id} -- the investigate
-- page hydrates scope and range from the row, which is why from_at/to_at live
-- here rather than being re-derived.
--
-- to_at NULL means an OPEN-ENDED RANGE ("from then until further notice"), the
-- opposite of annotations.end_at's NULL (an instant mark). The two columns look
-- alike and mean different things, so every range predicate over this table
-- spells to_at's NULL as 'infinity' rather than folding it onto from_at.
--
-- status is open|resolved with resolved_at as its witness: resolved rows carry
-- a resolved_at, open rows do not, and reopening clears it. The invariant is
-- enforced in Go (store/incidents.go) rather than by a CHECK, so widening the
-- status vocabulary in a later milestone is code, not a migration -- the same
-- reasoning M4 Decision 9 applies to check_schedules.kind.
--
-- pinned is a JSONB array (NOT an object -- DEFAULT '[]', not '{}') of typed
-- refs {kind,id,note}: the findings the UI pinned, in the order the operator
-- pinned them. It is validated in Go against a closed kind vocabulary before
-- it is written; jsonb here only guarantees it is well-formed JSON.
--
-- RETENTION READS resolved_at, and an OPEN incident is therefore NEVER pruned:
-- see store/prune.go's sweep list for the whole of that rule.
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

-- maintenance_windows are DATA AND RENDERING in M6, not suppression logic (M6
-- Decision 6): nothing evaluates alert rules until M7, so there is nothing to
-- suppress. They render as markArea on scoped charts and as timeline rows, and
-- they are read by the correlation panel as an EXPLAINING class rather than an
-- implicating one.
--
-- Unlike incidents, the range here is closed on both ends and the CHECK says
-- so: a window with no end is not a maintenance window, it is a state of the
-- world. The same rule is enforced in Go before the INSERT (Validate), so the
-- caller gets a named error rather than a raw constraint violation -- the
-- CHECK is the backstop for anything that ever writes this table without
-- going through the store.
--
-- scope uses the annotations vocabulary: '' global, otherwise a node, a pair
-- (src->dst) or a target name, matched exactly.
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

-- webhooks are outbound incident-lifecycle notifications (M6 Decision 5). One
-- row per endpoint; the delivery OUTCOME lives on that same row
-- (last_status/last_attempt/failures) rather than in a delivery-log table,
-- which would be one unbounded row per delivery for marginal value -- the
-- ledger is the console log.
--
-- secret_enc is OPAQUE BYTES to this layer. The store does not encrypt, does
-- not decrypt, and never inspects it: the dispatcher package owns the crypto
-- (config-keyed AES-GCM, Decision 4) and hands the store ciphertext. That is
-- what keeps the secret out of every place the global constraints forbid it --
-- audit rows, API responses, logs, metric labels -- because this layer has
-- nothing to leak but a byte string it cannot read.
--
-- events is a TEXT[] rather than a child table: it is a closed three-value set
-- (incident.created|incident.resolved|incident.reopened) validated in Go, and
-- a join table for at most three enum values per endpoint would be structure
-- without information.
--
-- This table is NEVER swept by retention. See store/prune.go.
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
