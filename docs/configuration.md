# Configuration reference

Configuration is loaded from a YAML file (default `/etc/kconmon-ng/config.yaml`,
override with `KCONMON_NG_CONFIG`) and can be selectively overridden via
environment variables. The file is watched for changes and reloaded at runtime —
no restart required. Since v1.2.0 the config is parsed strictly: unknown keys or
invalid checker settings fail startup and are rejected on hot-reload (the
previous config stays active).

## Full config file

```yaml
metricsPrefix: kconmon_ng # prefix for all Prometheus metric names
httpPort: 8080 # HTTP port: /metrics, /healthz, /readyz, /api/v1/...
grpcPort: 9090 # gRPC port: agent-controller communication
logLevel: info # debug | info | warn | error
logFormat: json # json | text
failureDomainLabel: topology.kubernetes.io/zone # node label used as zone

# Agent-only: gRPC address of the controller
controllerAddress: "" # e.g. kconmon-ng-controller:9090

controller:
  leaderElection: true # enable leader election for HA (requires k8s RBAC)
  agentTtl: 30s # evict agents that miss heartbeats for this duration
  events:
    enabled: false # serve EventStream.WatchEvents (Console realtime) and advertise
    # the "events" capability on GET /api/v1/version. Leader-only: passive
    # replicas reject subscriptions. Requires a controller image that includes
    # the M2 event stream (newer than v1.3.3).

checkers:
  tcp:
    enabled: true
    interval: 5s
    timeout: 1s

  udp:
    enabled: true
    interval: 5s
    timeout: 250ms
    packets: 5 # packets per probe burst (min 1)

  icmp:
    enabled: true
    interval: 5s
    timeout: 1s # requires NET_RAW capability

  dns:
    enabled: true
    interval: 5s
    hosts:
      - kubernetes.default.svc.cluster.local
    resolvers: [] # empty = system resolver; add IPs for explicit upstream DNS

  http:
    enabled: false
    interval: 30s
    timeout: 5s
    targets:
      - url: https://example.com/healthz
        method: GET # default GET
        expectStatus: 200 # 0 = any 2xx/3xx
        bodyPattern: "" # optional Go regexp matched against response body

  mtr:
    cooldown: 60s # minimum interval between traces for the same (src, dst) pair
    maxHops: 30 # max TTL / hop count (1–64)

observability:
  otel:
    enabled: false
    endpoint: "" # OTLP gRPC endpoint, e.g. otel-collector:4317
```

## Environment variable overrides

| Variable                          | Config field             |
| --------------------------------- | ------------------------ |
| `KCONMON_NG_CONFIG`               | path to config file      |
| `KCONMON_NG_MODE`                 | `mode`                   |
| `KCONMON_NG_METRICS_PREFIX`       | `metricsPrefix`          |
| `KCONMON_NG_LOG_LEVEL`            | `logLevel`               |
| `KCONMON_NG_LOG_FORMAT`           | `logFormat`              |
| `KCONMON_NG_CONTROLLER_ADDRESS`   | `controllerAddress`      |
| `KCONMON_NG_FAILURE_DOMAIN_LABEL` | `failureDomainLabel`     |
| `KCONMON_NG_NODE_NAME`            | injected by Downward API |
| `KCONMON_NG_POD_NAME`             | injected by Downward API |
| `KCONMON_NG_POD_IP`               | injected by Downward API |
| `KCONMON_NG_ZONE`                 | injected by Downward API (optional zone override) |

## Helm values that matter most

```yaml
controller:
  replicaCount: 2 # run 2 replicas; only the leader is active (leaderElection: true)
  leaderElection: true
  events:
    enabled: true # required for Console realtime (Live page, pushed matrix)

agent:
  tolerations:
    - operator: Exists # schedule on ALL nodes, including control-plane and tainted nodes
  securityContext:
    capabilities:
      add: [NET_RAW] # required for ICMP and MTR

config:
  checkers:
    http:
      enabled: true
      targets:
        - url: https://kubernetes.default.svc.cluster.local/healthz
          method: GET
          expectStatus: 200

serviceMonitor:
  enabled: true # scrape agents and controller via Prometheus Operator
  interval: 15s

prometheusRule:
  enabled: true # deploy default alerting rules

networkPolicy:
  enabled: true # restrict ingress/egress to required paths only
  prometheusNamespace: monitoring

pdb:
  enabled: true # prevent controller eviction during node drain
  minAvailable: 1

serviceAccount:
  create: true # creates ClusterRole with nodes get/list/watch
```

Every value is documented inline in
[charts/kconmon-ng/values.yaml](../charts/kconmon-ng/values.yaml).

## Console (M1/M2/M3)

The Console is off by default and reads its own config file, rendered by the
chart from `console.*` (it is not part of the `config:` block above). M1 gave it
read-only pages over Prometheus and the controller API; M2 added the realtime
path — the `/ws` WebSocket, the Live page and pushed matrix snapshots; M3
added optional PostgreSQL persistence and authentication/RBAC. Full detail —
the config file's every key/default/validation rule, the auth-mode matrix,
and the secret-mount layout — lives in
[docs/console/architecture/CONFIG.md](console/architecture/CONFIG.md); this
section stays a summary.

```yaml
controller:
  events:
    enabled: true # controller side of realtime; see the note below

console:
  enabled: false # default; the rest of this block is ignored while it is false
  replicas: 2
  controller:
    url: "" # empty = derive from this release's controller Service
    timeout: 10s
    # gRPC address of the controller's EventStream. Empty = derive from this
    # release's controller Service when controller.events.enabled=true,
    # otherwise realtime stays off and the UI polls with a "Delayed data" badge.
    grpcAddr: ""
  prometheus:
    url: "" # REQUIRED for the matrix/Explore/PromQL pages; empty = those APIs 503
    queryTimeout: 30s
    maxRange: 24h # max query_range window
    maxResponseBytes: 8388608 # 8 MiB
  # Valkey pub/sub, used only to fan events across Console replicas.
  valkey:
    mode: disabled # bundled | external | disabled
    address: "" # host:port; REQUIRED for mode=external, ignored otherwise
    dialTimeout: 5s
    port: 6379 # bundled: listen port and the address the console dials
    image: # bundled only
      repository: valkey/valkey
      tag: 8-alpine
      pullPolicy: IfNotPresent
    resources: # bundled only
      limits: { cpu: 200m, memory: 128Mi }
      requests: { cpu: 50m, memory: 64Mi }
  networkPolicy:
    # Egress rule list for console -> external Valkey. Empty + mode=external
    # renders a default allowing TCP console.valkey.port to any namespace;
    # ignored for mode=bundled (a precise pod-selector rule is rendered).
    valkeyEgress: []
  # PostgreSQL persistence (M3, ADR-001). Optional: with mode=disabled the
  # console runs exactly the M1/M2 surface, no history, no local/oidc auth.
  database:
    mode: disabled # cnpg | external | disabled
    existingSecret: "" # mode=external only; must hold a full postgres:// DSN
    existingSecretKey: dsn
  # Authentication (M3, SECURITY.md §10.1). anonymous is the default and a
  # fully supported deployment; RBAC still applies, with the fixed role below.
  auth:
    mode: anonymous # anonymous | local | header | oidc
    anonymous:
      role: viewer
    defaultRole: "" # role for an authenticated subject with no binding; empty = none (403)
    # mode=local and mode=oidc both require database.mode=cnpg|external.
    # mode=header requires a non-empty auth.header.trustedProxyCIDRs (no
    # default — an empty list would be an authentication bypass).
```

`controller.events.enabled` turns on the controller's `EventStream.WatchEvents`
RPC and the `"events"` capability flag on its `GET /api/v1/version`. It is
leader-only — passive replicas reject subscriptions — and needs a controller
image that includes the M2 event stream (newer than v1.3.3; the chart's
`appVersion` is bumped to that image at release). While it is `false` the
chart omits the `events` key from the
rendered controller config entirely, so a pre-M2 image (which would reject the
unknown key at startup) keeps rolling safely; enabling it is what commits the
fleet to an M2+ image.

Setting `console.controller.grpcAddr` explicitly points the Console at a
controller elsewhere. The chart still renders only a **same-namespace** egress
rule to this release's controller, so a target in another namespace or cluster
needs your own NetworkPolicy on **both** the egress and the ingress side, plus
any host firewall — there is no `grpcEgress` override list.

The bundled Valkey (`mode: bundled`) is a single-replica Deployment with **no
PersistentVolumeClaim**: per ADR-002 it holds nothing durable, so losing it on a
restart is a liveness event, never data loss. With `mode: disabled` the Console
falls back to an in-process bus, which has no cross-replica fan-out — so
realtime plus `console.replicas > 1` plus no Valkey is a misconfiguration the
chart **refuses to render**, with a message naming the fix. The check keys on the
resolved gRPC address rather than on `controller.events.enabled`, because an
explicit `grpcAddr` dials with events off too.

The Console serves `GET /ws` (one multiplexed WebSocket per browser tab) at the
top level of `console.service.port`, alongside its `/api/v1/*` REST endpoints. An
ingress in front of it must allow upgrades and **preserve `Host`** — the origin
check compares the browser's `Origin` header host against the request host, so a
proxy that rewrites `Host` (or forwards a mismatched `Origin`) makes every
upgrade refused and the UI silently falls back to 15s polling. A proxy that
strips `Origin` entirely still upgrades: an absent header is allowed, since
non-browser clients never send one.

`console.database.mode=cnpg` renders a CloudNativePG `Cluster` CR but this
chart does **not** install the CNPG operator or its CRDs — `helm install`
fails outright with a clear "no matches for kind Cluster" error if they are
not already present. Every console secret (the database DSN, the local-mode
bootstrap admin password, the OIDC client secret) mounts as a file under one
directory, `/etc/kconmon-ng-console-secrets/`, group-readable
(`console.podSecurityContext.fsGroup`, default matching the distroless
nonroot gid); rotating one is an operator-initiated restart, since the
Deployment rolls on ConfigMap changes only. `auth.mode=local|oidc` requires
`database.mode` to be `cnpg` or `external`, and — with `console.replicas > 1`
— `console.valkey.mode` to be `bundled` or `external` (sessions live in
Valkey/PostgreSQL, not the single-replica in-process fallback); the chart
refuses to render otherwise, with a message naming the fix. See
[docs/console/architecture/CONFIG.md](console/architecture/CONFIG.md) for
every validation rule and [SECURITY.md](console/architecture/SECURITY.md)
for the auth-mode/RBAC/audit detail.

## Zone auto-discovery

On registration the controller resolves each agent's zone from its node's
`failureDomainLabel` (default `topology.kubernetes.io/zone`) and the agent
adopts it, so `source_zone`/`destination_zone` labels are populated with no
per-agent config. An explicit `agent.zone` value (or `KCONMON_NG_ZONE`) always
wins. A node label change after registration is broadcast to peers immediately;
the agent's own `source_zone` refreshes on its next re-registration. Requires
`controller.leaderElection: true` — the node informer runs only on the leader.
