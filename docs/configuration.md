# Configuration reference

Configuration is loaded from a YAML file (default `/etc/kconmon-ng/config.yaml`,
override with `KCONMON_NG_CONFIG`) and selectively overridable via environment
variables. Since v1.2.0 the file is parsed strictly: unknown keys or invalid
checker settings fail startup, and both binaries watch the file, re-parsing it
on every change. A reload that fails validation is rejected and logged while
the previous config stays active, so a typo cannot take a running fleet down.

## What reloads and what does not

The watch re-reads the whole file, but three blocks are explicitly bound to
process lifecycle. Knowing which saves a confused hour:

| Block | Takes effect | Why |
| --- | --- | --- |
| `agent` (identity: `nodeName`, `advertiseAddress`, `zone`) | next agent restart | a changed identity is a different agent to every peer, so it is resolved once at startup |
| `agent.tls.*`, `agent.bootstrapTokenFile` (file *contents*) | next reconnect, no restart | the agent re-reads certificate and token files on every dial |
| `controller.externalGateway` | next controller restart | the TLS listener is built once, before serving starts; the token is read once with it |

Rotating gateway material therefore means restarting the controller; rotating
agent-side material does not. The asymmetry is spelled out again in
[External agents](external-agents.md).

## Full config file

Every key both binaries accept, at its default:

```yaml
metricsPrefix: kconmon_ng # prefix for all Prometheus metric names
httpPort: 8080 # HTTP API: /healthz, /readyz, /api/v1/... (also still serves /metrics)
metricsPort: 9091 # /metrics and the health endpoints, on a listener of their own
grpcPort: 9090 # gRPC: agent-controller communication; on agents, also the UDP probe server
logLevel: info # debug | info | warn | error
logFormat: json # json | text
failureDomainLabel: topology.kubernetes.io/zone # node label used as zone

# Agent-only: gRPC address of the controller
controllerAddress: "" # e.g. kconmon-ng-controller:9090

# Agent-only: what the agent asserts about itself at registration. All keys are
# optional; in-cluster the Downward API env fills the same values, and on a
# bare host every key has a fallback (see "Agent identity" below).
agent:
  nodeName: "" # empty = the host's hostname
  advertiseAddress: "" # IP literal peers probe; empty = KCONMON_NG_POD_IP, else autodetect
  zone: "" # explicit zone; empty lets the controller resolve it from the node label
  # TLS towards the controller. Setting ANY key here switches the dial to the
  # external gateway; an empty block keeps the plaintext in-cluster dial
  # byte-identical.
  tls:
    caFile: "" # CA that signed the gateway's serving cert; empty = system trust pool
    certFile: "" # client certificate; certFile and keyFile go together
    keyFile: ""
    serverName: "" # verify the server cert against this name instead of the dialed host
  # File whose content rides as a bearer token on every RPC. Refused without
  # the tls block: a token over plaintext is a token published to the network.
  bootstrapTokenFile: ""

controller:
  leaderElection: true # enable leader election for HA (requires k8s RBAC)
  agentTtl: 30s # evict agents that miss heartbeats for this duration; minimum 10s
  events:
    # Serve EventStream.WatchEvents; leader-only, needs a controller newer than v1.3.3.
    enabled: false
  # Second gRPC listener for agents OUTSIDE the cluster: same services, same
  # registry, but TLS plus a bearer token. The in-cluster listener is untouched.
  externalGateway:
    enabled: false
    port: 9443 # must differ from httpPort, grpcPort and metricsPort
    tls:
      certFile: "" # serving pair; both required when the gateway is enabled
      keyFile: ""
      clientCaFile: "" # optional: mandates verified client certs + identity pinning
    bootstrapTokenFile: "" # required when enabled; content shorter than 16 chars is refused

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
    timeout: 1s # unprivileged ICMP socket; see the ping_group_range sysctl the chart sets

  dns:
    enabled: true
    interval: 5s
    timeout: 5s
    hosts:
      - kubernetes.default.svc.cluster.local
    resolvers: [] # empty = system resolver; see formats below

  http:
    enabled: false
    interval: 30s
    timeout: 5s
    targets:
      - url: https://example.com/healthz
        method: GET # default GET
        expectStatus: 200 # 0 = any 2xx/3xx
        bodyPattern: "" # optional Go regexp matched against response body
        insecureSkipVerify: false # certificates are VERIFIED; set true per target for a self-signed endpoint

  mtr:
    cooldown: 60s # minimum interval between traces for the same (src, dst) pair
    maxHops: 30 # max TTL / hop count (1-64)

  # Probes to destinations that are not fleet peers. Off by default, and
  # enabling it with an empty allowedCidrs fails startup (see below).
  external:
    enabled: false
    allowedCidrs: [] # e.g. ["8.8.8.8/32"]; matched against the RESOLVED address
    deniedCidrs: [] # subtracted from allowedCidrs
    maxTargets: 100
    timeout: 10s
```

> `observability.otel.*` used to be documented here. The keys parse and are then
> read by nothing: no tracer is created and no span is exported, so they have
> been removed from this reference rather than left as a knob that silently does
> nothing. The same goes for `KCONMON_NG_MODE`: the value lands in `cfg.Mode`
> and is never consulted.

### Why two HTTP ports

The controller's `httpPort` serves its whole API (`GET /api/v1/topology`,
`POST /api/v1/diagnostics`, `PUT /api/v1/external-checks`), and none of it
authenticates anything. A NetworkPolicy rule that let a scraper reach
`/metrics` on that port therefore let the scraper's entire namespace reach the
fleet's control plane, and a NetworkPolicy cannot say "this port, but only
these paths". Two listeners can: `metricsPort` carries `/metrics` plus the
health endpoints and nothing else, so "let Prometheus in" and "let this caller
drive the fleet" become two different firewall decisions. The API port keeps
serving `/metrics` too; nothing in the chart opens it to a scraper any more.

### Validation rules the loader enforces

Everything below fails startup (and is rejected on hot-reload) with a message
naming the field:

- All three ports must be in 1-65535 and pairwise distinct; the gateway port
  must additionally differ from all of them.
- `agent.advertiseAddress` must be an IP literal. The controller publishes it
  to every peer as a probe target and rejects anything `net.ParseIP` refuses,
  so a hostname or `host:port` fails here, not at registration.
- `agent.tls.certFile` and `keyFile` go together: a client certificate
  without its key cannot handshake.
- `agent.bootstrapTokenFile` without the `agent.tls` block is refused. The
  token never rides plaintext; the credential also refuses insecure transport
  at runtime, as a second net.
- An enabled gateway must have `tls.certFile`, `tls.keyFile` and
  `bootstrapTokenFile`. Half-configured means a startup error, never a
  silently-open listener.
- `controller.agentTtl` must be positive and at least 10s. The floor is two
  agent heartbeats (agents beat every 5s): anything under that evicts the
  whole fleet between beats. Zero used to panic the ticker the TTL feeds and
  produced a CrashLoopBackOff with a raw stack trace, which is why it is a
  named config error now.
- `udp.packets >= 1`; `mtr.maxHops` in 1-64.
- For every enabled checker, `interval` and `timeout` must be positive.
  `timeout >= interval` is deliberately only a *warning* ("probes may overlap
  or starve"): probes may be tuned tight, and the operator may know what they
  are doing.
- `dns.hosts` must be non-empty for an enabled DNS checker.
- `checkers.external.enabled: true` with an empty `allowedCidrs` is a startup
  refusal: "must be non-empty when enabled... never read as allow-everything".
  It is not a running agent that denies everything — the process does not come
  up. The CIDR lists are also parsed through the same constructor the agent
  enforces with, so a CIDR that would be rejected at probe time is rejected at
  startup instead.

`dns.resolvers` accepts three spellings: a bare host, `host:port`, and a bare
IPv6 address (`2001:4860:4860::8888`). The last one used to fail startup
because every colon sent the entry through host:port splitting; accepting it
was a deliberate fix, and the checker joins the port on for that spelling the
same way it does for a bare IPv4.

`maxTargets` and `timeout` under `external` are defaulted (100 and 10s) only
when the block is enabled, so a disabled block stays byte-identical to what
the operator wrote. The numbers themselves: 100 is far more than any realistic
target list and still a bound, and 10s bounds the resolution-and-authorization
step of one external destination — generous for a DNS lookup, short enough
that a hung resolver cannot pin a task slot.

## Environment variable overrides

| Variable                          | Config field             |
| --------------------------------- | ------------------------ |
| `KCONMON_NG_CONFIG`               | path to config file      |
| `KCONMON_NG_METRICS_PREFIX`       | `metricsPrefix`          |
| `KCONMON_NG_LOG_LEVEL`            | `logLevel`               |
| `KCONMON_NG_LOG_FORMAT`           | `logFormat`              |
| `KCONMON_NG_CONTROLLER_ADDRESS`   | `controllerAddress`      |
| `KCONMON_NG_FAILURE_DOMAIN_LABEL` | `failureDomainLabel`     |
| `KCONMON_NG_NODE_NAME`            | `agent.nodeName` (in-cluster: injected by Downward API) |
| `KCONMON_NG_ADVERTISE_ADDRESS`    | `agent.advertiseAddress` |
| `KCONMON_NG_ZONE`                 | `agent.zone` (in-cluster: injected by Downward API) |
| `KCONMON_NG_POD_NAME`             | injected by Downward API; not a config key |
| `KCONMON_NG_POD_IP`               | injected by Downward API; not a config key |

The identity block shares its env names with the chart's Downward API
injection on purpose: the same ConfigMap can be mounted fleet-wide while each
pod still registers as its own node.

## Agent identity

The `agent` block is what an agent asserts about itself when it registers, and
every key resolves the same way in-cluster and on a bare host:

- **nodeName**: `KCONMON_NG_NODE_NAME` env > `agent.nodeName` > the host's
  hostname.
- **advertiseAddress**: `KCONMON_NG_ADVERTISE_ADDRESS` env >
  `agent.advertiseAddress` > `KCONMON_NG_POD_IP` (the Downward API value
  in-cluster) > autodetect. The autodetect asks the kernel which source
  address a datagram to `controllerAddress` would leave from (nothing is
  sent), so it needs `controllerAddress` set and resolvable. Multi-homed hosts
  whose probe traffic should use a different interface must set the address
  explicitly. Whatever wins must be an **IP literal**; a non-IP value fails
  startup, not registration.
- **zone**: `KCONMON_NG_ZONE` env > `agent.zone` > controller-side resolution
  from the node's `failureDomainLabel` (in-cluster only; see
  [Zone auto-discovery](#zone-auto-discovery)).

An agent started without `KCONMON_NG_POD_NAME` (that is, outside any Pod) is
labeled `kconmon-ng.io/external=true` in its registration metadata, so
consoles and API consumers can tell bare-host agents apart. The controller
needs no configuration for any of this.

## Helm values that matter most

```yaml
controller:
  replicaCount: 2 # run 2 replicas; only the leader is active (leaderElection: true)
  leaderElection: true
  events:
    enabled: true # required for Console realtime (Live page, pushed matrix)
  pdb:
    enabled: true # prevent controller eviction during node drain; rendered only at replicaCount > 1
    minAvailable: 1

agent:
  tolerations:
    - operator: Exists # schedule on ALL nodes, including control-plane and tainted nodes
  # No added capabilities: ICMP and MTR use the unprivileged ICMP socket that the chart's
  # net.ipv4.ping_group_range sysctl opens.
  securityContext:
    capabilities:
      drop: [ALL]

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
  enabled: true # deploy the nine built-in alerting rules
  udpLossHigh:
    threshold: 0.25 # per-rule knobs: enabled / threshold / for / severity
    # a threshold may also be a string ("0.25") — that is what --set produces
  additionalRules: [] # your own rules, appended verbatim

networkPolicy:
  enabled: true # restrict ingress/egress to required paths only
  prometheusNamespace: monitoring

serviceAccount:
  create: true # creates ClusterRole with nodes get/list/watch
```

Every value is documented inline in
[the Helm values reference](reference/helm-values.md), which embeds the
chart's full
[`values.yaml`](https://github.com/EsDmitrii/kconmon-ng/blob/main/charts/kconmon-ng/values.yaml)
at build time; the reasoning behind the alerting rules and the chart's guards
is in the
[chart README](https://github.com/EsDmitrii/kconmon-ng/blob/main/charts/kconmon-ng/README.md).

## Console

The Console is off by default and reads its own config file, rendered by the
chart from `console.*` (it is not part of the `config:` block above). It grew
in three steps: v1.4.0 gave it read-only pages over Prometheus and the
controller API plus the realtime path (the `/ws` WebSocket, the Live page,
pushed matrix snapshots), and v1.5.0 added optional PostgreSQL persistence and
authentication/RBAC. The config file's every key, default and validation rule
lives in the
[chart README](https://github.com/EsDmitrii/kconmon-ng/blob/main/charts/kconmon-ng/README.md)
and the commented `values.yaml`; this section stays a summary.

```yaml
# The stack the Console runs on lives OUTSIDE the console block, and the chart installs none of it:
# run PostgreSQL and a Redis-compatible server however you already run infrastructure, then hand the
# chart one connection string for each.
database:
  existingSecret: "" # Secret holding a postgres:// DSN; empty = in-memory (no history, no auth)
  existingSecretKey: console-database-dsn

redis:
  existingSecret: "" # Secret holding a redis:// DSN; empty = in-process bus (console.replicas: 1)
  existingSecretKey: console-redis-dsn
  dialTimeout: 5s

controller:
  events:
    enabled: true # controller side of realtime; see the note below

console:
  enabled: false # default; the rest of this block is ignored while it is false
  # 1 by default. More than 1 REQUIRES redis.existingSecret — sessions, the rate-limit counters and
  # the realtime fan-out live there, and the chart refuses the combination rather than silently
  # multiplying every rate limit by the replica count.
  replicas: 1
  controller:
    url: "" # empty = derive from this release's controller Service
    timeout: 10s
    # gRPC address of the controller's EventStream; empty = derive from the release's Service.
    grpcAddress: ""
  prometheus:
    url: "" # REQUIRED for the matrix/Explore/PromQL pages; empty = those APIs 503
    queryTimeout: 30s
    maxRange: 24h # max query_range window
    maxResponseBytes: 8388608 # 8 MiB
  networkPolicy:
    # Egress rules for console -> external Redis; empty renders a permissive default.
    redisEgress: []
  # Authentication; anonymous is the default and RBAC still applies.
  auth:
    mode: anonymous # anonymous | local | header | oidc
    anonymous:
      role: viewer
    # Role for an authenticated subject with no binding; empty = none (403).
    # ONE OF THE BUILT-INS: viewer | operator | alert-editor | admin. A custom role name is refused
    # by the console at startup — this field is not resolved against the RBAC table.
    defaultRole: ""
    # Map a GROUP the identity provider asserts onto a role this console grants. This is what makes
    # an oidc or header install usable from a cold database: role_bindings are created through an
    # API that already needs rbac:manage, so without it nobody could make the first binding.
    # Roles resolve as the UNION of this map and the bindings; a group absent from the map grants
    # nothing, and a value may be a built-in or the name of a custom role.
    groupRoles: {}
    # local/oidc require a database; header requires a non-empty trustedProxyCIDRs.
```

`controller.events.enabled` turns on the controller's `EventStream.WatchEvents`
RPC and the `"events"` capability flag on its `GET /api/v1/version`. It is
leader-only (passive replicas reject subscriptions) and needs a controller
image that includes the event stream — newer than v1.3.3; the chart's
`appVersion` is bumped to that image at release. While it is `false` the chart
omits the `events` key from the rendered controller config entirely, so a
pre-v1.4.0 image, which would reject the unknown key at startup, keeps rolling
safely. Enabling it is what commits the fleet to the newer image.

Setting `console.controller.grpcAddress` explicitly points the Console at a
controller elsewhere. The chart still renders only a **same-namespace** egress
rule to this release's controller, so a target in another namespace or cluster
needs your own NetworkPolicy on **both** the egress and the ingress side, plus
any host firewall. There is no `grpcEgress` override list.

`redis.existingSecret` points the Console at any Redis-compatible server by
DSN (`redis://`, `rediss://`, `valkey://`, `unix://`); the chart installs
none. Left empty, the Console falls back to an in-process bus with no
cross-replica fan-out, which is why realtime plus `console.replicas > 1` plus
no bus fails the render with a message naming the fix. The check keys on the
resolved gRPC address rather than on `controller.events.enabled`, because an
explicit `grpcAddress` dials with events off too.

The Console serves `GET /ws` (one multiplexed WebSocket per browser tab) at
the top level of `console.service.port`, alongside its `/api/v1/*` REST
endpoints. An ingress in front of it must allow upgrades and **preserve
`Host`**. The origin check compares the browser's `Origin` header host against
the request host, so a proxy that rewrites `Host` (or forwards a mismatched
`Origin`) makes every upgrade refused, and the UI silently falls back to 15s
polling. A proxy that strips `Origin` entirely still upgrades: an absent
header is allowed, since non-browser clients never send one.

`database.existingSecret` names a Secret holding a `postgres://` DSN. The
chart installs no database and does not care which one answers: CloudNativePG,
Percona, RDS, a plain StatefulSet; the chart README documents the stack it is
tested against.

Every console secret (the database DSN, the local-mode bootstrap admin
password, the OIDC client secret) mounts as a file under one directory,
`/etc/kconmon-ng-console-secrets/`, group-readable via
`console.podSecurityContext.fsGroup` (default matching the distroless nonroot
gid). Rotation behaves differently per Secret kind. The Deployment's
annotations checksum the config and any chart-managed Secret, so a
`secret.create` Secret rolls the Deployment by itself; rotating an *existing*
Secret the chart only references is an operator-initiated restart, because the
chart cannot checksum content it does not render.

`auth.mode=local|oidc` requires `database.existingSecret`, and with
`console.replicas > 1` also `redis.existingSecret` — sessions live in
Redis/PostgreSQL, not the single-replica in-process fallback. Both violations
are caught at render time. Identity, group-to-role resolution and session
bounds for `oidc` mode are covered in depth in the
[OIDC setup scenario](scenarios/oidc-setup.md); the
[chart README](https://github.com/EsDmitrii/kconmon-ng/blob/main/charts/kconmon-ng/README.md)
carries the full auth-mode/RBAC/audit detail.

## Zone auto-discovery

On registration the controller resolves each agent's zone from its node's
`failureDomainLabel` (default `topology.kubernetes.io/zone`) and the agent
adopts it, so `source_zone`/`destination_zone` labels are populated with no
per-agent config. An explicit `agent.zone` value (or `KCONMON_NG_ZONE`) always
wins. A node label change after registration is broadcast to peers
immediately; the agent's own `source_zone` refreshes on its next
re-registration. Requires `controller.leaderElection: true` — the node
informer runs only on the leader.
