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

# Agent-only: what the agent asserts about itself at registration. All keys are
# optional — in-cluster the Downward API env fills the same values; on a bare
# host every key has a fallback (see "Agent identity" below). Resolved once at
# startup: changing this block takes effect on the next agent restart.
agent:
  nodeName: "" # empty = the host's hostname
  advertiseAddress: "" # IP literal peers probe; empty = KCONMON_NG_POD_IP, else autodetect
  zone: "" # explicit zone; empty lets the controller resolve it from the node label

controller:
  leaderElection: true # enable leader election for HA (requires k8s RBAC)
  agentTtl: 30s # evict agents that miss heartbeats for this duration
  events:
    # Serve EventStream.WatchEvents; leader-only, needs a controller newer than v1.3.3.
    enabled: false

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
        insecureSkipVerify: false # certificates are VERIFIED; set true per target for a self-signed endpoint

  mtr:
    cooldown: 60s # minimum interval between traces for the same (src, dst) pair
    maxHops: 30 # max TTL / hop count (1–64)

  # Probes to destinations that are not fleet peers. Off by default: the agent
  # refuses every external destination until allowedCidrs names one, and the
  # cluster still has to let the packet out (networkPolicy.externalEgress).
  external:
    enabled: false
    allowedCidrs: [] # e.g. ["8.8.8.8/32"]; empty means no external destination is probeable
    deniedCidrs: [] # subtracted from allowedCidrs
    maxTargets: 100
    timeout: 10s
```

> `observability.otel.*` used to be documented here. The keys parse and are then
> read by nothing — no tracer is created and no span is exported — so they have
> been removed from this reference rather than left as a knob that silently does
> nothing. The same goes for `KCONMON_NG_MODE`: the value lands in `cfg.Mode`
> and is never consulted.

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

## Agent identity

The `agent` block is what an agent asserts about itself when it registers, and
every key resolves the same way in-cluster and on a bare host:

- **nodeName**: `KCONMON_NG_NODE_NAME` env > `agent.nodeName` > the host's
  hostname.
- **advertiseAddress**: `KCONMON_NG_ADVERTISE_ADDRESS` env >
  `agent.advertiseAddress` > `KCONMON_NG_POD_IP` (the Downward API value
  in-cluster) > autodetect. The
  autodetect asks the kernel which source address a datagram to
  `controllerAddress` would leave from (nothing is sent), so it needs
  `controllerAddress` to be set and resolvable; multi-homed hosts whose probe
  traffic should use a different interface must set the address explicitly.
  Whatever wins must be an **IP literal** — the controller publishes it to
  every peer as a probe target and rejects hostnames — and a non-IP value
  fails startup, not registration.
- **zone**: `KCONMON_NG_ZONE` env > `agent.zone` > controller-side resolution
  from the node's `failureDomainLabel` (in-cluster only; see
  [Zone auto-discovery](#zone-auto-discovery)).

An agent started without `KCONMON_NG_POD_NAME` — i.e. outside any Pod — is
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
  enabled: true # deploy the seven built-in alerting rules
  udpLossHigh:
    threshold: 0.25 # per-rule knobs: enabled / threshold / for / severity
    # a threshold may also be a string ("0.25") — that is what --set produces
  additionalRules: [] # your own rules, appended verbatim

networkPolicy:
  enabled: true # restrict ingress/egress to required paths only
  prometheusNamespace: monitoring

controller:
  pdb:
    enabled: true # prevent controller eviction during node drain; rendered only at replicaCount > 1
    minAvailable: 1

serviceAccount:
  create: true # creates ClusterRole with nodes get/list/watch
```

Every value is listed in
[charts/kconmon-ng/values.yaml](../charts/kconmon-ng/values.yaml); the reasoning
behind the alerting rules and the chart's guards is in the
[chart README](../charts/kconmon-ng/README.md).

## Console (M1/M2/M3)

The Console is off by default and reads its own config file, rendered by the
chart from `console.*` (it is not part of the `config:` block above). M1 gave it
read-only pages over Prometheus and the controller API; M2 added the realtime
path — the `/ws` WebSocket, the Live page and pushed matrix snapshots; M3
added optional PostgreSQL persistence and authentication/RBAC. Full detail —
the config file's every key/default/validation rule, the auth-mode matrix,
and the secret-mount layout — lives in the
[chart README](../charts/kconmon-ng/README.md) and the commented
`charts/kconmon-ng/values.yaml`; this section stays a summary.

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
leader-only — passive replicas reject subscriptions — and needs a controller
image that includes the M2 event stream (newer than v1.3.3; the chart's
`appVersion` is bumped to that image at release). While it is `false` the
chart omits the `events` key from the
rendered controller config entirely, so a pre-M2 image (which would reject the
unknown key at startup) keeps rolling safely; enabling it is what commits the
fleet to an M2+ image.

Setting `console.controller.grpcAddress` explicitly points the Console at a
controller elsewhere. The chart still renders only a **same-namespace** egress
rule to this release's controller, so a target in another namespace or cluster
needs your own NetworkPolicy on **both** the egress and the ingress side, plus
any host firewall — there is no `grpcEgress` override list.

`redis.existingSecret` points the Console at any Redis-compatible server by DSN (`redis://`,
`rediss://`, `valkey://`, `unix://`); the chart installs none. Left empty, the Console falls back to
an in-process bus with no cross-replica fan-out — so realtime plus `console.replicas > 1` plus no
bus is a misconfiguration the chart **refuses to render**, with a message naming the fix. The check keys on the
resolved gRPC address rather than on `controller.events.enabled`, because an
explicit `grpcAddress` dials with events off too.

The Console serves `GET /ws` (one multiplexed WebSocket per browser tab) at the
top level of `console.service.port`, alongside its `/api/v1/*` REST endpoints. An
ingress in front of it must allow upgrades and **preserve `Host`** — the origin
check compares the browser's `Origin` header host against the request host, so a
proxy that rewrites `Host` (or forwards a mismatched `Origin`) makes every
upgrade refused and the UI silently falls back to 15s polling. A proxy that
strips `Origin` entirely still upgrades: an absent header is allowed, since
non-browser clients never send one.

`database.existingSecret` names a Secret holding a `postgres://` DSN — the chart installs no
database and does not care which one answers (CloudNativePG, Percona, RDS, a plain StatefulSet); the
chart README documents the stack it is tested against. Every console secret (the database DSN, the local-mode
bootstrap admin password, the OIDC client secret) mounts as a file under one
directory, `/etc/kconmon-ng-console-secrets/`, group-readable
(`console.podSecurityContext.fsGroup`, default matching the distroless
nonroot gid); rotating an EXISTING Secret (`existingSecret`) is an
operator-initiated restart, because the Deployment's annotations checksum the
config and the chart-MANAGED Secret, not a Secret the chart only references; a
chart-managed one (`secret.create`) therefore rolls the Deployment by itself. `auth.mode=local|oidc` requires
`database.existingSecret` to be set, and — with `console.replicas > 1`
— `redis.existingSecret` to be set (sessions live in
Redis/PostgreSQL, not the single-replica in-process fallback); the chart
refuses to render otherwise, with a message naming the fix. The
[chart README](../charts/kconmon-ng/README.md) carries every validation rule
and the auth-mode/RBAC/audit detail.

In `auth.mode=oidc` a person's identity is `oidc:<sub>` and nothing else. `sub`
is the only claim OIDC Core §5.7 permits as an identifier — `preferred_username`
and `email` are explicitly forbidden as one, because an IdP may reassign them,
which is how Grafana's CVE-2023-3128 (CVSS 9.4) let a leaver's address inherit
their roles. `console.auth.oidc.usernameClaim` therefore decides only the DISPLAY
name (falling back to `name`, then `email`, then the sub itself) — the label in
the header menu, not an identity. The audit log is keyed on the identity and
records `oidc:<sub>`, so a display name never appears there at all; changing
this claim renames a person in the UI and moves nothing else. A login
whose ID token carries no `sub` is refused, as is one whose `sub` sits inside a
reserved namespace (`oidc:`, `local:`, `header:`, `token:`) — an issuer minting
`sub = "local:<uuid>"` would otherwise be handed that local user's bindings.

Group membership is re-read on every token refresh, so removing someone from a
group at the IdP takes effect within the access token's lifetime rather than at
their next login. A provider that returns no `id_token` on refresh (most do not)
leaves the session's groups as they were: an empty group list is a silent, total
deauthorization, and inventing one out of a missing optional field would be worse
than the staleness.

Bindings created before this scheme name a username (`alice`) and now resolve to
nothing, which is the correct direction to fail but an invisible one. At boot in
`oidc` mode the console logs a WARN naming every user binding that is not
`oidc:`-prefixed, with its role, so they can be remapped against the IdP's own
sub values. This is a report rather than an automatic rewrite on purpose:
rewriting `alice` to `oidc:<sub>` means trusting the username claim to say who
`alice` was, and not trusting that claim is the entire reason the scheme changed.

A session is bounded twice. `console.auth.session.ttl` (default 12h) is the
ABSOLUTE lifetime: it is counted from login and is never extended, so a session
ends 12h after sign-in no matter how busy it was. `console.auth.session.idleTimeout`
(default 1h) is the second bound: a session unused for that long is refused with
`401` and purged from Valkey on the next attempt to use it. The idle deadline
slides forward as the session is used — but never past the absolute one, which is
the whole reason there are two numbers rather than one. Setting `idleTimeout: 0`
disables the idle bound and leaves `ttl` alone in charge, which is exactly how
every release before this one behaved. The session cookie's `Max-Age` is the
absolute lifetime, so a browser may still hold a cookie the server has already
stopped honouring; that is the ordinary case behind a mid-session `401`, and the
console routes it to the login page.

## Zone auto-discovery

On registration the controller resolves each agent's zone from its node's
`failureDomainLabel` (default `topology.kubernetes.io/zone`) and the agent
adopts it, so `source_zone`/`destination_zone` labels are populated with no
per-agent config. An explicit `agent.zone` value (or `KCONMON_NG_ZONE`) always
wins. A node label change after registration is broadcast to peers immediately;
the agent's own `source_zone` refreshes on its next re-registration. Requires
`controller.leaderElection: true` — the node informer runs only on the leader.
