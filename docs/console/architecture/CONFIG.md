<!--
Status: current
Owner: @EsDmitrii
Source: written from the as-built M0–M3 implementation (2026-08-06):
internal/console/config/config.go, cmd/console/main.go, and the chart's
templates/console/configmap.yaml + templates/console/deployment.yaml +
values.yaml at chart 1.5.0.
This document is the source of truth for Console configuration. Update it (and the ADRs) in the same PR as any deviation.
-->

# Console configuration

The Console binary reads its **own** file, separate from the agent/controller
config documented in `docs/configuration.md`.

| | |
| --- | --- |
| Default path | `/etc/kconmon-ng-console/config.yaml` |
| Override | `KCONMON_NG_CONSOLE_CONFIG` |
| Parsing | strict — `KnownFields(true)`, so an unknown key fails startup |
| Missing file | **not** an error: defaults are loaded and validated |
| Hot reload | **no**. Unlike the agent/controller file this one is read once, at boot |

`KCONMON_NG_CONSOLE_CONFIG` is **still the only environment variable the
console binary reads** — that was true at chart 1.4.0 and M3 deliberately
kept it true. Every new M3 setting (`database.*`, `auth.*`, and all their
secrets) is expressed in `config.yaml` like everything else. Secrets
themselves never ride in that file as inline values: `database.dsnFile`,
`auth.local.bootstrapAdminPasswordFile` and `auth.oidc.clientSecretFile` are
paths, and the chart mounts the Kubernetes Secrets they point at as files —
see "Secret mounts" below.

Rendered by the chart from `console.*` values (`templates/console/configmap.yaml`)
and mounted into the Deployment.

## Full file, with the code's defaults

```yaml
httpPort: 8080
logLevel: info # debug | info | warn | error
logFormat: json # json | text
metricsPrefix: kconmon_ng # Console self-metrics are <prefix>_console_*
auth:
  mode: anonymous # anonymous | local | header | oidc
  anonymous:
    role: viewer # must not be empty
  defaultRole: "" # role for an authenticated subject with no binding; empty = none (403)
  local:
    bootstrapAdmin: "" # username created on first start when the users table is empty
    bootstrapAdminPasswordFile: "" # file path; NEVER an inline password
  header:
    userHeader: X-Remote-User
    groupsHeader: X-Remote-Groups
    groupsDelimiter: ","
    trustedProxyCIDRs: [] # REQUIRED non-empty in header mode
  oidc:
    issuer: ""
    clientID: ""
    clientSecretFile: ""
    redirectURL: ""
    scopes: [openid, profile, email, groups]
    usernameClaim: preferred_username
    groupsClaim: groups
  session:
    ttl: 12h
    cookieName: __Host-kconmon_session
    secure: true # false ONLY for local http:// development
controller:
  url: "" # e.g. http://kconmon-ng-controller:8080; empty = topology API answers 503
  timeout: 10s # per-request, console -> controller HTTP
  grpcAddr: "" # e.g. kconmon-ng-controller:9090; empty = realtime ingestion off
prometheus:
  url: "" # empty = matrix and PromQL APIs answer 503
  queryTimeout: 30s
  maxRange: 24h # query_range end-start cap
  maxResponseBytes: 8388608 # 8 MiB
valkey:
  address: "" # host:port; empty = in-process bus (single replica only)
  dialTimeout: 5s # initial dial only
database:
  dsn: "" # postgres://... ; MUST NOT embed a password
  dsnFile: "" # path to a file holding the full DSN; WINS over dsn when set
  maxConns: 10
  connectTimeout: 10s
  migrateOnStart: true # ADR-001: embedded goose migrations, advisory-locked, run on start
  retentionDays: 90 # 0 disables pruning
```

`database` is entirely omitted from the rendered config when
`console.database.mode=disabled` (the chart's default) — an absent block,
not an explicit `dsn: ""` — so the empty-DSN meaning below is what
`Config.defaults()` already assumes with no database block present at all.

## Validation, key by key

Startup fails — loudly, before serving — on any of these:

| Rule | Failure |
| ---- | ------- |
| `httpPort` outside 1–65535 | error |
| `logLevel` not one of `debug\|info\|warn\|error` | error |
| `logFormat` not `json\|text` | error |
| `metricsPrefix` empty | error |
| `auth.mode` not one of `anonymous\|local\|header\|oidc` | error |
| `controller.url` / `prometheus.url` not `""` and not an absolute http(s) URL, or with a trailing slash | error |
| `controller.timeout`, `prometheus.queryTimeout`, `prometheus.maxRange`, `prometheus.maxResponseBytes`, `valkey.dialTimeout` non-positive | error |
| `valkey.address` not `""` and not `host:port` | error |
| `database.dsn` and `database.dsnFile` both set | error ("set either ... not both") |
| `database.dsn` set and not a `postgres://`/`postgresql://` URL with a host | error |
| `database.dsn` embeds a password | error ("it would land in a ConfigMap: use database.dsnFile") |
| `database.maxConns` < 1 | error |
| `database.connectTimeout` non-positive | error |
| `database.retentionDays` < 0 | error (0 is legal: "disables pruning") |
| `auth.defaultRole` set and not a known built-in role (`viewer\|operator\|alert-editor\|admin`) | error |
| `auth.session.ttl` non-positive | error |
| `auth.session.cookieName` starts with `__Host-` and `auth.session.secure=false` | error (browsers reject `__Host-` without `Secure`) |

### The per-mode auth matrix

`auth.mode` selects exactly one of these branches; the other three blocks are
parsed but ignored:

| Mode | Extra requirement | Failure if unmet |
| --- | --- | --- |
| `anonymous` | `auth.anonymous.role` non-empty | error |
| `header` | `auth.header.userHeader` non-empty, `auth.header.trustedProxyCIDRs` non-empty and every entry a valid CIDR | error — an empty list is an auth bypass, not a default to fall back on (SECURITY.md §10.1) |
| `local` | a resolved `database.dsn`/`dsnFile` (users live in PostgreSQL) | error |
| `oidc` | a resolved `database.dsn`/`dsnFile` (sessions live in PostgreSQL), plus `auth.oidc.issuer` an absolute `https://` URL with no trailing slash, `clientID` non-empty, `clientSecretFile` non-empty, `redirectURL` an absolute http(s) URL ending in `config.OIDCCallbackPath` (`/api/v1/auth/oidc/callback`) | error |

`controller.grpcAddr` is deliberately **unvalidated** beyond being a string. Like
`controller.url` it is operator config, and an unreachable address just fails to
dial at runtime, surfaced by the ingester's reconnect logs — not by refusing to
boot.

## Empty means off, and off is a tested state

These keys are feature switches whose empty/absent value is a supported deployment, not a
misconfiguration:

| Key empty | Effect |
| --------- | ------ |
| `controller.url` | `GET /api/v1/topology` answers `503 application/problem+json`; the topology pusher is not started |
| `prometheus.url` | matrix and PromQL APIs answer `503`; the matrix pusher is not started |
| `controller.grpcAddr` | realtime event ingestion is off. `/ws` still serves snapshot topics; `GET /api/v1/version` advertises no `events`; the UI badges itself "Delayed data" and stays on 15 s polling |
| `valkey.address` | the console runs on `cache.InProcessBus` — no cross-replica fan-out, correct only for one replica (ADR-002) |
| `database.dsn` **and** `database.dsnFile` (the resolved DSN) | the console runs with no store at all: `GET /api/v1/events` and `GET /api/v1/audit` answer `503`, RBAC admin (`/api/v1/rbac/*`) and tokens (`/api/v1/tokens`) answer `503`, run history is in-memory only (`checks.MemoryStore`), and `auth.mode` is restricted to `anonymous`/`header` (validation rejects `local`/`oidc` — see the per-mode matrix above) |

One combination is downgraded rather than accepted: `controller.grpcAddr` set
while `controller.url` is empty logs a warning and disables ingestion, because
the capability precheck needs the controller's HTTP API.

### Known M3 limitation: a Valkey dial failure at boot degrades sessions silently

The table row above describes `valkey.address` being *empty*. A configured
address that simply **fails to dial at startup** takes the same fallback path,
and this is the case worth alerting on. `newBus` (`cmd/console/main.go`) logs

```
WARN valkey unreachable at startup — falling back to the in-process bus;
     realtime fan-out is single-replica only until the console is restarted
```

and returns `cache.NewInProcessBus()`. Because the session/OIDC-state `cache.KV`
is built from that same bus object — `kv = cache.NewValkeyKVFromBus(vb)` only
when the bus type-asserts to `*cache.ValkeyBus`, otherwise
`cache.NewInProcessKV()` — the fallback silently takes **sessions** with it, not
just realtime fan-out, and the WARN text mentions only the latter. The pod then
reports **Ready**: nothing about the degraded bus is reflected in the readiness
probe, and there is no retry — the in-process bus is kept until the process
restarts.

The consequence, with `auth.mode=local|oidc` and `replicas > 1`: that one
replica holds its sessions in its own memory. Users load-balanced onto it are
logged out at random and cannot stay logged in, while the other replicas behave
normally. The Helm render guard (`auth.mode=local|oidc` + `valkey.mode=disabled`
+ `replicas > 1`, below) does **not** cover this — it is a render-time check on
the *declared* mode, and a `bundled`/`external` Valkey that is merely down at
the moment the console boots renders fine.

Operationally, until a readiness reflection lands (post-M3):

- Alert on the log line `valkey unreachable at startup` from any console pod;
  treat it as a paging-grade event in a multi-replica auth deployment, and
  restart the affected pod once Valkey is reachable again.
- Order the rollout so Valkey is up before the console (`console.replicas=1`
  during a Valkey outage is the crude but effective mitigation — one replica
  makes the in-process session KV correct rather than merely survivable).

The WebSocket hub itself is constructed **unconditionally**, since snapshot topics
do not depend on the event pipeline.

## Secret mounts

`database.dsnFile`, `auth.local.bootstrapAdminPasswordFile` and
`auth.oidc.clientSecretFile` — whichever apply — all resolve to paths under
one **sibling** directory, `/etc/kconmon-ng-console-secrets/`, mounted
alongside (not nested inside) the ConfigMap-backed
`/etc/kconmon-ng-console/`:

| Config key | Mounted path |
| --- | --- |
| `database.dsnFile` | `/etc/kconmon-ng-console-secrets/database-dsn` |
| `auth.local.bootstrapAdminPasswordFile` | `/etc/kconmon-ng-console-secrets/local-admin-password` |
| `auth.oidc.clientSecretFile` | `/etc/kconmon-ng-console-secrets/oidc-client-secret` |

This deviates from the milestone's original plan, which nested a `secrets/`
subdirectory under the ConfigMap-backed config directory itself: a container
runtime cannot create a mountpoint inside an already-mounted **read-only**
volume (`CreateContainerError`), so the two paths had to become siblings,
one mount, multiple projected `Secret` sources.

The projected volume renders with `defaultMode: 0440`, and
`console.podSecurityContext` (default `{fsGroup: 65532}`, matching the
distroless nonroot gid) sets the pod's `fsGroup` so the nonroot console
process can actually read a Secret file the kubelet writes root-owned. The
milestone's original plan called for `0400`; that mode is owner-only and
unreadable by the nonroot process without matching UID ownership, which a
projected Secret volume does not give you — `0440` (group-readable) plus
`fsGroup` is what actually works. `console.podSecurityContext` renders **only
when `console.database.mode != disabled`**, so the default manifest
(database off) stays byte-identical to chart 1.4.0.

Secrets are read once at boot; rotating one is an operator-initiated restart
(the Deployment rolls on ConfigMap changes only, never on Secret changes).

## Helm mapping (chart 1.5.0)

| Config key | Helm value |
| ---------- | ---------- |
| `httpPort` | `console.service.port` |
| `logLevel`, `logFormat`, `metricsPrefix` | shared `config.*` (same values as agent/controller) |
| `auth.*` | `console.auth.*` (secret-backed fields resolve to the mounted paths above) |
| `controller.url` | `console.controller.url`, or derived from this release's controller Service when empty |
| `controller.timeout` | `console.controller.timeout` |
| `controller.grpcAddr` | `console.controller.grpcAddr`, else this release's controller Service **only when `controller.events.enabled=true`**, else empty |
| `prometheus.*` | `console.prometheus.*` |
| `valkey.address` | derived from `console.valkey.mode`: bundled → the rendered Valkey Service, external → `console.valkey.address` (rendering fails if unset), disabled → empty |
| `valkey.dialTimeout` | `console.valkey.dialTimeout` |
| `database.*` | `console.database.*`; the whole `database:` block is omitted from the rendered config when `console.database.mode=disabled` |
| `database.dsnFile` | derived: `console.database.mode=cnpg` → the CNPG-operator-generated `<cluster>-app` Secret; `mode=external` → `console.database.existingSecret`/`existingSecretKey` |

Realtime therefore needs **two** flags, one on each side:

```yaml
controller:
  events:
    enabled: true # serve EventStream.WatchEvents + advertise "events". Leader-only. Default false

console:
  enabled: true
  replicas: 2
  valkey:
    mode: bundled # bundled | external | disabled; with disabled, keep replicas: 1
```

### Chart behaviours worth knowing before you debug one

- **`controller.events` is emitted into the shared ConfigMap only when it is
  `true`.** The shared config is parsed with `KnownFields(true)` and Go's zero
  value makes an omitted key identical to an explicit `false`, so an
  unconditional key would crashloop a pre-M2 controller image as Pods roll. The
  chart's rendered output for a fleet with events off, `auth.mode=anonymous` and
  `database.mode=disabled` is byte-identical to 1.3.x's — the pre-events chart,
  before M2's Valkey/WebSocket additions and M3's auth/database additions both
  landed. (`1.5.0` post-dates this milestone and now names a **future**
  release, so it can no longer be the comparison point.)
- **The render-time guard keys on the RESOLVED gRPC address**, not on
  `controller.events.enabled`: an explicit `grpcAddr` with events off is still
  realtime-on-with-no-bus. Resolved address + `valkey.mode=disabled` +
  `replicas > 1` fails rendering with an explanatory message.
- **The console→controller gRPC NetworkPolicy egress rule is gated the same
  way**, for the same reason: egress missing on an explicitly configured dial
  target is a silent packet drop with no diagnostic. The controller-side *ingress*
  half lives in the shared root policy and is keyed on `events.enabled` — that
  flag belongs to the controller.
- **`grpcAddr` pointed outside this namespace is the operator's own policy to
  write**, on both the egress and the ingress side. There is no `grpcEgress`
  override list.
- **Three more render-time `fail` guards were added in M3**
  (`templates/console/configmap.yaml`), all keyed on RESOLVED values the same
  way the M2 guard above is:
  - `auth.mode=local|oidc` with `valkey.mode=disabled` and `replicas > 1` fails:
    sessions for local/oidc live in Valkey (DATA.md §5.3), not the in-process
    KV, once more than one replica is running.
  - `auth.mode=local|oidc` with `database.mode=disabled` fails: users, tokens
    and the audit log live in PostgreSQL (SECURITY.md §10.1), so a database is
    not optional the way it is for anonymous/header.
  - `auth.mode=header` with an empty `auth.header.trustedProxyCIDRs` fails: an
    empty list would make an unauthenticated request header an authentication
    bypass.
- **Two NetworkPolicy paths were added in M3**, both requiring both layers
  (egress on the console side, ingress on the destination side) exactly like
  the M2 controller-gRPC and Valkey rules:
  - **console↔database**: `console.database.mode=cnpg` renders a precise
    pod-selector egress rule (`templates/console/networkpolicy.yaml`) plus a
    matching ingress rule on the CNPG cluster's own NetworkPolicy
    (`templates/console/database-networkpolicy.yaml`, which also opens CNPG's
    own inter-instance replication/status ports and the Prometheus scrape port
    9187). `mode=external` renders a namespace-wide default (tighten via
    `console.networkPolicy.databaseEgress`).
  - **console→OIDC IdP**: `auth.mode=oidc` renders an egress rule defaulting to
    `ipBlock: 0.0.0.0/0` on TCP 443 (an IdP is almost always outside the
    cluster, so a `namespaceSelector` would silently match nothing), tighten
    via `console.networkPolicy.oidcEgress`.
- **A NetworkPolicy is only one of two layers.** If the destination sits behind a
  host firewall (iptables/nftables, a puppet-managed ruleset, a cloud security
  group), that layer must be opened separately. Egress allowed in the chart and
  still refused on the wire is almost always the host-firewall layer.
- **Whether kubelet probes are subject to the ingress policy is CNI-dependent.**
  This is chart-wide and pre-existing, not specific to the console.
- **The bundled Valkey is ephemeral by design**: a Deployment with no PVC, no
  volumeClaimTemplates, no writable data dir (ADR-002).
- **CNPG is a CRD chicken-and-egg** (ADR-001): `console.database.mode=cnpg`
  renders a `Cluster` CR but the chart never installs the CNPG operator or its
  CRDs. `helm install` fails outright with a clear "no matches for kind
  Cluster" error if the operator is not already present — there is no silent
  partial-render.
