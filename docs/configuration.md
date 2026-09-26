# Configuration reference

Configuration is loaded from a YAML file (default `/etc/kconmon-ng/config.yaml`,
override with `KCONMON_NG_CONFIG`) and selectively overridable via environment
variables. The file is parsed strictly: unknown keys or invalid
checker settings fail startup, and both binaries watch the file, re-parsing it
on every change. A reload that fails validation is rejected and logged while
the previous config stays active, so a typo cannot take a running fleet down.

## What reloads and what does not

Both binaries watch the file and re-read it on every change, a ConfigMap
mount included (the chart mounts the directory, so a `kubectl edit` of the
ConfigMap reaches the pods without a restart). A reload that passes
validation is applied as far as the running process can: each binary applies
the keys it can change live, keeps the startup value of every other key it
reads, and logs one warning naming those keys (`config changed in keys the
agent reads only at startup; restart the agent to apply them`, the same for
the controller). Keys that only the other binary reads are ignored without a
word.

| Binary | Applied live | Needs a restart |
| --- | --- | --- |
| agent | `logLevel`; everything under `checkers.*`: `enabled`, `interval` and `timeout` of every checker, `udp.packets`, `pmtu.size`, `dns.hosts`, `dns.resolvers`, `http.targets`, `mtr.cooldown`, `mtr.maxHops`, and `external.enabled`, `allowedCidrs`, `deniedCidrs`, `maxTargets`, `timeout` | `metricsPrefix`, `httpPort`, `grpcPort` (also the UDP echo port the udp and pmtu probes use), `metricsPort`, `logFormat`, `controllerAddress`, `agent.*` |
| controller | `logLevel`, `topology.*`, `controller.agentTtl`, `checkers.external.enabled`, `checkers.external.allowedCidrs` | `metricsPrefix`, `httpPort`, `grpcPort`, `metricsPort`, `logFormat`, `failureDomainLabel`, `controller.leaderElection`, `controller.events.enabled`, `controller.externalGateway.*`, `controller.prometheusSD.enabled` |

The agent ignores `controller.*`, `topology.*` and `failureDomainLabel`; the
controller ignores `agent.*`, `controllerAddress` and every other key under
`checkers.*`; both ignore the deprecated `mode` and `observability.otel.*`,
which nothing reads. What a live change does:

- **An agent checker** whose block changed stops, drops the results of the
  round it was in, and starts again on the new settings; the other checkers
  keep their cadence, and peers and metric series carry over. A checker
  switched off stops at once and its gauges go; its counters and histograms
  stop growing and stay until the agent restarts. Switching a plane or
  `checkers.external` on or off makes the agent register again with its new
  capabilities, so the console and the CLI follow without a restart. A
  re-registration that fails is retried after 2s, doubling to 30s with up to
  25% jitter, and logged as `re-registration failed, retrying` (WARN), or as
  `controller rejected the re-registration payload, ...` (ERROR) when the
  controller refuses the payload itself. A file
  that fails validation, an invalid `bodyPattern` regexp included, is refused
  as a whole and the running config stays.
- **`topology.*`** makes the leader replan the mesh and push a full peer
  list to every agent; a standby keeps the new topology and plans with it
  once it leads.
- **`controller.agentTtl`** applies from the next eviction sweep, and the
  sweep then runs every TTL/2.
- **The controller's `checkers.external` keys** only change what
  `GET /api/v1/version` publishes as `externalAllowedCidrs`; each agent
  enforces its own copy of the allowlist, applied live on the agent.

The restart-bound keys that are easy to trip over:

| Key | Takes effect | Why |
| --- | --- | --- |
| `agent` (identity: `nodeName`, `advertiseAddress`, `zone`) | next agent restart | a changed identity is a different agent to every peer, so it is resolved once at startup |
| `agent.tls.*`, `agent.bootstrapTokenFile` | paths: next restart; file *contents*: next reconnect | the agent re-reads certificate and token files on every dial |
| `httpPort`, `grpcPort`, `metricsPort` | next restart | listeners bind once per process; the ports an agent advertises to its peers (2.4.0+) travel with its registration, so they change only when it registers again |
| `controller.externalGateway` | next controller restart | the TLS listener is built once, before serving starts; the token is read once with it |

Rotating gateway material therefore means restarting the controller; rotating
agent-side material does not. The asymmetry is spelled out again in
[External agents](external-agents.md).

Three more rules of the watch. An empty file is ignored with a warning
(`config file is empty, keeping the current config until it is written`),
because a writer that edits in place truncates the file first; to go back to
the defaults, restart, or write a file that holds only comments. When
`config.yaml` is a symlink, into the same or another directory, edits to the
target reload on Linux and a repointed link is followed. `KCONMON_NG_*` environment
overrides are process environment and need a restart.

## Full config file

Every key both binaries accept, at its default:

```yaml
metricsPrefix: kconmon_ng # prefix for all Prometheus metric names
httpPort: 8080 # HTTP API: /healthz, /readyz, /api/v1/... (also still serves /metrics)
metricsPort: 9091 # /metrics and the health endpoints, on a listener of their own
grpcPort: 9090 # gRPC: agent-controller communication; on agents, also the UDP probe server
logLevel: info # debug | info | warn | error, any case
logFormat: json # json | text, any case
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
  # TLS towards the controller. `enabled: true` or ANY other key set here
  # switches the dial to TLS for the external gateway; an empty block keeps the
  # plaintext in-cluster dial byte-identical.
  tls:
    enabled: false # true = TLS with the system trust pool and nothing else set (2.5.0+)
    caFile: "" # CA that signed the gateway's serving cert; empty = system trust pool once TLS is on
    certFile: "" # client certificate; certFile and keyFile go together
    keyFile: ""
    serverName: "" # verify the server cert against this name instead of the dialed host
  # File whose content rides as a bearer token on every RPC. Refused without
  # TLS: a token over plaintext is a token published to the network.
  bootstrapTokenFile: ""

controller:
  leaderElection: true # enable leader election for HA (requires k8s RBAC)
  agentTtl: 30s # evict agents that miss heartbeats for this duration; minimum 10s
  events:
    # Serve EventStream.WatchEvents; leader-only, needs a controller newer than v1.3.3.
    enabled: false
  # GET /api/v1/prometheus/sd on httpPort and metricsPort: the external-agent
  # target list for Prometheus (2.4.0+). false answers 404 on both; only
  # `enabled` is accepted under this key.
  prometheusSD:
    enabled: true
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

  pmtu:
    enabled: true # on by default since 2.5.0
    interval: 60s # keep it at 28x timeout (14s here) or more and under 3m; outside that range the agent warns
    timeout: 500ms # per datagram; the whole search stays under half the interval
    size: 0 # IP-level bytes, 0 or 576-65535; 0 = the MTU of the route to the peer

  dns:
    enabled: true
    interval: 5s
    timeout: 2s
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

# Controller-only: which pairs the agents probe.
topology:
  mode: full # full | sparse
  sparse:
    ringDegree: 2 # ring successors each agent probes; >= 1 in sparse mode
    zoneChords: 2 # extra cross-zone peers per agent; 0-64
    autoThreshold: 0 # fleets smaller than this stay on full mesh; 0 = no floor
```

The chart writes a `pmtu` key into the agent ConfigMap only when it differs
from these defaults, because an agent older than 2.5.0 refuses the key. Tuning
`pmtu` therefore needs agent images 2.5.0 or newer.

The pmtu search gets half of `interval`, and a black-hole search needs about
fourteen datagram timeouts, hence the 28× floor. `PathMTUBlackHole` reads a
10-minute window, and its sustained arm (two failures in 30m, one of them in
the last 10m) needs a few probes per window: from 3m to 10m the agent warns
that it catches a black hole on one of several ECMP paths late or
intermittently, and above 10m that the alert loses its data between probes.
All three are warnings, never a refusal. How
the probe picks its size and why it is UDP is in
[The path MTU plane](concepts/mesh-and-planes.md#the-path-mtu-plane).

Mixed versions around pmtu, as far as they show outside the matrix:

- `kubectl kconmon check --type pmtu` from a 2.4.x agent, or from one with
  `checkers.pmtu.enabled: false`, gets a `501` from the controller ("does not
  run pmtu probes") and exits 1. An agent older than 2.4.0 advertises no
  planes, so the controller dispatches the task anyway; that agent answers
  `unknown check type "pmtu"`, which the CLI reports as a failed check with
  exit 2.
- A 2.4.x agent answers 2.5.0 peers' pmtu datagrams on its UDP echo port, so
  a fleet in the middle of an upgrade already measures the paths *into* the
  old agents.

> `observability.otel.*` and the deprecated `mode` (`KCONMON_NG_MODE`) still
> parse, so an old config keeps loading, and nothing reads them: no tracer is
> created and no span is exported. Every load that finds either set logs a
> warning (`observability.otel is ignored: no tracer is created; remove it from
> the config`, `mode (KCONMON_NG_MODE) is ignored: nothing reads it; remove it
> from the config`). Remove both from your config.

### Why two HTTP ports

The controller's `httpPort` serves its whole API (`GET /api/v1/topology`,
`POST /api/v1/diagnostics`, `PUT /api/v1/external-checks`), and none of it
authenticates anything. A NetworkPolicy rule that let a scraper reach
`/metrics` on that port therefore let the scraper's entire namespace reach the
fleet's control plane, and a NetworkPolicy cannot say "this port, but only
these paths". Two listeners can: `metricsPort` carries `/metrics`, the health
endpoints and, on the controller, the read-only external-agent target list
for Prometheus (`GET /api/v1/prometheus/sd`, since 2.4.0), so "let Prometheus
in" and "let this caller drive the fleet" become two different firewall
decisions. The API port keeps serving `/metrics` too; nothing in the chart
opens it to a scraper any more.

### Validation rules the loader enforces

Everything below fails startup (and is rejected on hot-reload) with a message
naming the field by its full YAML path, such as `checkers.udp.packets` or
`checkers.http.targets[2].url`. The chart (2.5.0 and later) refuses some of the
same values at `helm install` or `helm upgrade` time, before any pod
crash-loops on them: an `mtr.cooldown` that is zero, negative or has no
unit, a zero or invalid `interval` or `timeout` on an enabled tcp, udp, icmp,
pmtu, dns or http checker, an `interval` below 100ms written as one value
(`5ns`, `50ms`, `0.05s`), a `timeout` below 1ms written as one value (`5ns`,
`999us`, `0.5ms`), with external checks on an `external.timeout` that is
neither 0 nor at least 1ms, a `topology.sparse.zoneChords` above 64, an
`http.targets[].expectStatus` outside 0 or
100-599, an `http.targets[].url` that is not `http(s)://` with a host, a
`bodyPattern` that does not compile (with the http checker on), a port in
`dns.resolvers` outside 1-65535, a `controllerAgentTtl` below 10s, and a
CIDR without a prefix length in `checkers.external.allowedCidrs` or
`deniedCidrs`.

- The config file holds a single YAML document; an empty trailing `---` is
  fine.
- All three ports must be in 1-65535 and pairwise distinct; the gateway port
  must additionally differ from all of them.
- `metricsPrefix` starts with a letter and holds only letters, digits and
  underscores: empty, a leading digit or underscore, dashes and dots are
  refused.
- `logLevel` is one of debug, info, warn, error and `logFormat` json or text,
  in any case.
- `agent.advertiseAddress` must be an IP literal. The controller publishes it
  to every peer as a probe target and rejects anything `net.ParseIP` refuses,
  so a hostname or `host:port` fails here, not at registration. An address
  no peer can probe is refused too: the unspecified address (`0.0.0.0`,
  `::`), loopback (`127.0.0.0/8`, `::1`), multicast and `255.255.255.255`.
  Link-local, private and IPv6 addresses are accepted.
- `agent.tls.certFile` and `keyFile` go together: a client certificate
  without its key cannot handshake.
- `agent.bootstrapTokenFile` without TLS is refused: set `agent.tls.enabled`
  (the system trust pool), a `caFile`, a client certificate or a
  `serverName`. An empty `caFile` alone leaves the dial plaintext. The token
  never rides plaintext; the credential also refuses insecure transport at
  runtime, as a second net.
- An enabled gateway must have `tls.certFile`, `tls.keyFile` and
  `bootstrapTokenFile`. Half-configured means a startup error, never a
  silently-open listener.
- `controller.agentTtl` must be positive and at least 10s. The floor is two
  agent heartbeats (agents beat every 5s): anything under that evicts the
  whole fleet between beats. The TTL also feeds a ticker, which a zero value
  would panic, so zero is a named config error.
- `udp.packets` in 1-100; `mtr.maxHops` in 1-64; `mtr.cooldown` above zero;
  `pmtu.size` 0 or 576-65535.
- For every enabled checker (tcp, udp, icmp, pmtu, dns, http), `interval`
  must be at least 100ms and `timeout` at least 1ms. A smaller value fails
  with `checkers.<checker>.interval must be at least 100ms when the checker is
  enabled` or `checkers.<checker>.timeout must be at least 1ms when the
  checker is enabled`; on a hot reload the running config stays. The floors
  catch a unit typo such as `5ns` for `5s`: as an interval it would panic the
  agent and, through a hot reload, every agent at once; as a timeout it fails
  every probe. Through the Helm chart, an `interval` below 100ms or a `timeout`
  below 1ms written as one value already fails `helm install` or
  `helm upgrade` on the schema.
  `timeout >= interval` is deliberately only a *warning* ("probes may overlap
  or starve"): probes may be tuned tight, and the operator may know what they
  are doing. Three more warnings work the same way:
  `checkers.udp.packets x checkers.udp.timeout >= checkers.udp.interval`, a
  pmtu `interval` under 28× its `timeout`, and a pmtu `interval` of 3m or
  more, which leaves `PathMTUBlackHole` few probes per window. The pmtu
  warnings carry `checker=pmtu`; the two about the alert also carry a
  `window` attribute with the 10m range `PathMTUBlackHole` reads. These
  warnings, and the ones for a set `mode` or `observability.otel`, go through
  the configured `logLevel` and `logFormat` on stdout, like every other log
  line.
- `dns.hosts` must be non-empty for an enabled DNS checker, and a port in
  `dns.resolvers` must be 1-65535.
- `http.targets[].expectStatus` is 0 (unset) or 100-599, `method` is a
  valid HTTP method token, and `bodyPattern` compiles as a Go regular
  expression. The error names the target by its index
  (`checkers.http.targets[2].method`). A URL with a password is quoted with
  everything from the first colon after the scheme to the last `@` masked as
  `xxxxx`; when the masked URL parses, the reason says the fault is inside
  that masked part.
- `topology.mode` is `full` or `sparse`; in sparse mode `ringDegree` is at
  least 1, `zoneChords` is 0-64 (`topology.sparse.zoneChords must be between
  0 and 64, got N`) and `autoThreshold` is not negative. A fleet that wants
  more than 64 cross-zone peers per agent wants the full mesh.
- `checkers.external.enabled: true` with an empty `allowedCidrs` is a startup
  refusal: "must be non-empty when enabled... never read as allow-everything".
  It is not a running agent that denies everything: the process does not come
  up. The CIDR lists are also parsed through the same constructor the agent
  enforces with, so a CIDR that would be rejected at probe time is rejected at
  startup instead.

`dns.resolvers` accepts three spellings: a bare host, `host:port`, and a bare
IPv6 address (`2001:4860:4860::8888`). The checker joins the port on for a
bare IPv6 address the same way it does for a bare IPv4.

Against an explicit resolver each host is sent as an absolute name (the
checker adds the trailing dot), so the pod's search list and `ndots` do not
apply: write `kubernetes.default.svc.cluster.local`, not
`kubernetes.default`, and no cluster suffix reaches an outside resolver.
The agent (2.5.0 and later) sends those queries itself: A and AAAA in
parallel, recursion desired, EDNS0 with a 1232-byte UDP size, and again over
TCP when the answer comes back truncated. It never answers from `/etc/hosts`
or a pod's `hostAliases`, so a dead resolver reads red even for a name pinned
in either. The same holds for a continuous external `dns` check. With `resolvers: []` the
system resolver keeps the search list and `/etc/hosts`, because that path
measures what the pod's own lookups go through. The shipped DNS timeout is 2s,
under the 5s interval, so the default config logs no `timeout >= interval`
warning.

`maxTargets` and `timeout` under `external` are defaulted (100 and 10s) only
when the block is enabled, so a disabled block stays byte-identical to what
the operator wrote. The numbers themselves: 100 is far more than any realistic
target list and still a bound, and 10s bounds the resolution-and-authorization
step of one external destination: generous for a DNS lookup, short enough
that a hung resolver cannot pin a task slot. With the block enabled, a
`timeout` you set must be 0 (the default) or at least 1ms, the same floor as
the checkers'.

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
| `KCONMON_NG_POD_IP`               | injected by Downward API; not a config key (`status.hostIP` under `agent.hostNetwork`) |
| `KCONMON_NG_HOST_NETWORK`         | set to `true` by the chart under `agent.hostNetwork`; a 2.4.0+ agent image then adds the label `kconmon-ng.io/host-network=true` to its registration, older images ignore it; not a config key |

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
  explicitly. Whatever wins must be an **IP literal** that peers can probe; a
  non-IP value, or an unspecified, loopback, multicast or broadcast address,
  fails startup, not registration. That covers `KCONMON_NG_POD_IP` as well,
  and an autodetect that lands on a loopback source (a `controllerAddress`
  reached through a local tunnel, such as `127.0.0.1:<port>`) fails with
  `set agent.advertiseAddress explicitly` instead of advertising 127.0.0.1.
  The controller refuses such an address at registration too, so an older
  agent cannot slip one in.
- **zone**: `KCONMON_NG_ZONE` env > `agent.zone` > controller-side resolution
  from the node's `failureDomainLabel` (in-cluster only; see
  [Zone auto-discovery](#zone-auto-discovery)).

An agent started without `KCONMON_NG_POD_NAME` (that is, outside any Pod) is
labeled `kconmon-ng.io/external=true` in its registration metadata, so
consoles and API consumers can tell bare-host agents apart. A 2.4.0+ agent
started with `KCONMON_NG_HOST_NETWORK=true` (what the chart sets under
`agent.hostNetwork`) adds `kconmon-ng.io/host-network=true` the same way. The
controller needs no configuration for any of this.

### Ports travel with the identity

Since 2.4.0 the registration also carries the agent's three listener ports:
`httpPort` (the TCP probe target), `grpcPort` (on an agent, the UDP echo port;
reported as `udpPort`) and `metricsPort` (never probed; published so Prometheus
can [discover external hosts](external-agents.md#scraping-external-agents)).
Peers probe an agent on the ports *it* reported, so an external host may run on
ports of its own. An agent that reports none (older than 2.4.0) is probed on
the prober's own configured ports, and the agent itself never adopts ports from
the controller's reply; zone is the only field it takes from there. The rule
that follows, spelled out in [External agents](external-agents.md#ports):
keep one port set for the whole fleet until every agent runs 2.4.0, because an
old agent dials every peer on its own values, on-demand diagnostics included.

The UDP echo server on `grpcPort` (agents 2.5.0 and newer) does not answer
datagrams that no probe sends. That closes the echo-loop amplification between two
responders, and the broadcast fan-out. It refuses:

- a datagram from a source port below 1024;
- one from any registered agent's echo endpoint, its advertised address and
  echo port. The controller sends every agent the whole fleet's endpoints
  with each peer list, in full mesh and sparse mode alike; an agent connected
  to a controller older than 2.5.0 gets none and falls back to its current
  peers' endpoints;
- one from its own echo port, sent from the address the datagram reached or
  from loopback;
- one sent to `255.255.255.255`, to an IPv4 subnet broadcast address of the
  host, or to an IPv4 or IPv6 multicast group. One such datagram reaches
  every agent on the segment.

A source port alone refuses nothing above 1023. A NAT on the path, such as
`MASQUERADE --random-fully`, can hand a probe any unprivileged source port,
9090 included, and that probe gets its echo. So does a hand test such as
`nc -u -p 9090 <agent> 9090` from a host that is not an agent; from the
agent's own address or from an agent's echo endpoint it gets no reply. The
rate limit below ends a loop these rules miss, such as one forged from an
address that is not an agent's.

The endpoint list costs about 20 bytes per registered agent (IPv4) in every
peer list the controller sends. Under a sparse plan a peer list therefore
grows with the fleet again rather than with the few peers it names: about
20 KB at 1000 agents.

The echo replies from the address it was probed at. It listens on two sockets
on `grpcPort`, one IPv4 and one IPv6-only (`ss -lun` shows both), and sets
each reply's source to the address the datagram arrived at. An agent probed
at a secondary address, at an `agent.advertiseAddress` on a multi-homed host
or at a VIP therefore answers from that address, which the prober's connected
socket requires. An agent older than 2.5.0 lets the kernel pick the source by
route, so a pair probing it at such an address reads as 100% UDP loss and pmtu
`unreachable` while TCP and ICMP stay green. If the kernel refuses that source, for example because the address
left the host after the datagram arrived, the reply is sent once more with
the kernel's own choice. On a kernel without IPv6 the echo serves IPv4 only
and logs `UDP echo serves IPv4 only: no IPv6 socket` at info.

The echo server also rate-limits replies per source `ip:port`: a burst of 256
datagrams, refilled at 64 a second, for up to 4096 sources at once. A probe
sends at most a few hundred datagrams from one socket and then closes it, so
normal probing never reaches the limit; a reflection loop or a flood does, and
the agent logs `UDP echo rate limit reached, replies dropped` at most once a
minute, with a `dropped` count. A reply the kernel refuses to send is logged
the same way: `UDP write error` on the first one, then at most once a minute
with a `failed` count, and so is a failed read on the echo socket
(`UDP read error`).

## Helm values that matter most

```yaml
controller:
  replicaCount: 2 # run 2 replicas; only the leader is active (leaderElection: true)
  leaderElection: true
  events:
    enabled: true # required for Console realtime (Events page, pushed matrix)
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
  enabled: true # deploy the built-in alerting rules (fourteen; externalAgentDown ships off)
  udpLossHigh:
    threshold: 0.25 # per-rule knobs: enabled / threshold / for / severity
    # a threshold may also be a string ("0.25"); that is what --set produces
  additionalRules: [] # your own rules, appended verbatim

networkPolicy:
  enabled: true # separate policies for the agents, the controller and the console
  prometheusNamespace: monitoring
  # Replaces the default cluster DNS rule (UDP/TCP 53 to any pod). Needed for NodeLocal DNSCache
  # or any host-network resolver, which no namespaceSelector matches; keep namespaceSelector {}
  # in the list if the kube-dns pods must stay reachable.
  dnsEgress: []

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

### NetworkPolicy on Cilium, Calico and Antrea { #networkpolicy-and-cilium }

`networkPolicy.enabled` renders plain Kubernetes NetworkPolicies:
`<fullname>-agent`, `<fullname>-controller`, the controller's egress policy
`<fullname>-controller-apiserver`, `<fullname>-controller-gateway` with the
external gateway on and, with the console on, `<fullname>-console`. They are
the base on every CNI. The e2e suite runs a leg with
`networkPolicy.enabled=true` on kind, whose kindnet enforces policies, and
checks refused flows as well as allowed ones: the controller and the agents
admit the fixture Prometheus on the metrics port and refuse it on 8080, and
an agent probe to a port no egress rule names fails.

Several default egress rules reach their peer through an `ipBlock` of
`0.0.0.0/0`: `networkPolicy.kubeAPIEgress` and
`console.networkPolicy.kubeAPIEgress` (TCP 443/6443),
`networkPolicy.httpEgress` (TCP 80/443), and on the console
`webhookEgress` (TCP 443/80), `oidcEgress` and `geoipEgress` (TCP 443). What
such a rule matches depends on the CNI, and so does what an in-cluster peer
needs.

**Cilium.** Under its default `policy-cidr-match-mode`, Cilium never matches
a pod, a node or the apiserver against an `ipBlock`: it resolves them to
identities first (the pod's labels, the `remote-node` and `host` entities,
the `kube-apiserver` entity). The `0.0.0.0/0` rules therefore reach only
peers outside the cluster. For the apiserver that is fatal: the controller
cannot reach it, stays NotReady, and agents never register.
`networkPolicy.ciliumKubeAPIEgress` covers the chart's own flows with
`CiliumNetworkPolicy` objects, egress-only or ingress-only, additive, and
rendered only with `networkPolicy.enabled`:

- `<fullname>-kube-apiserver`: egress to the `kube-apiserver` entity on TCP
  443 and 6443, for the controller and, when
  `console.kubernetesContext.enabled` or `console.alerting.enabled` is on,
  the console.
- `<fullname>-node-ingress`, with `agent.hostNetwork` or
  `controller.externalGateway.enabled`: ingress from the `remote-node` and
  `host` entities to the controller's `grpcPort` (host-network agents
  register from their node IP) and to the gateway port (a gateway behind a
  NodePort or an `externalTrafficPolicy: Cluster` LoadBalancer sees a node
  IP). On Cilium the `networkPolicy.nodeCidrs` and
  `networkPolicy.externalAgentCidrs` `ipBlock`s match no node IP; this
  policy is what admits those peers there.

| `networkPolicy.ciliumKubeAPIEgress` | Renders the `CiliumNetworkPolicy` objects |
| --- | --- |
| `auto` (default) | when the cluster serves `cilium.io/v2` `CiliumNetworkPolicy` |
| `true` | always; the CRD must exist |
| `false` | never |

`auto` keys on the API versions Helm sees. A plain `helm template` pipeline
sees none, so pass `--api-versions cilium.io/v2/CiliumNetworkPolicy` or set
`true`. The policies do not select the agents: an HTTP checker target on the
apiserver, like the sample above, needs a `CiliumNetworkPolicy` of your own
on Cilium.

**Calico and Antrea.** Here an `ipBlock` matches pod IPs too, and kube-proxy
translates a Service address before policy sees the packet, so an egress
rule sees the backend pod's IP and its `targetPort`, not the Service port.
Two things follow. First, the controller reaches the apiserver through the
default `ipBlock`: the Service on 443 becomes a node IP on 6443, which the
rule names, and `auto` renders no `CiliumNetworkPolicy`. Second, the default
is wider than its purpose: `0.0.0.0/0` on 80/443 also opens every pod
listening on 80 or 443 to the agents and the console. `networkPolicy.clusterCIDRs`
narrows that. List the cluster's IPv4 pod and Service CIDRs and they become
`except` entries of the default `0.0.0.0/0` rules of `httpEgress`,
`webhookEgress`, `oidcEgress` and `geoipEgress`. The `httpEgress`,
`webhookEgress` and `geoipEgress` defaults then reach off-cluster peers only.
The default `oidcEgress` does not: next to the `ipBlock` it carries a
`namespaceSelector: {}` peer on 443 for an IdP behind an in-cluster ingress
controller, so every pod stays reachable on 443 from the console. Closing that
takes an explicit `console.networkPolicy.oidcEgress` list.

`clusterCIDRs` does not touch the apiserver rules, and those keep pods open
too. Their default is `0.0.0.0/0` on 443 and 6443 with no `except`, because
after kube-proxy's DNAT the apiserver is a node IP, outside both CIDRs. So the
controller always, and the console whenever it calls the apiserver
(`console.kubernetesContext` or `console.alerting` on), still reaches every
pod listening on 443 or 6443, whatever `clusterCIDRs` and `oidcEgress` say.
To close that, name the apiserver endpoint in `networkPolicy.kubeAPIEgress`
and `console.networkPolicy.kubeAPIEgress`; `kubectl get endpointslice -l
kubernetes.io/service-name=kubernetes` lists its addresses and port.
`clusterCIDRs` changes nothing on Cilium, where these rules never matched a
pod.

```yaml
networkPolicy:
  clusterCIDRs: [10.244.0.0/16, 10.96.0.0/12]
  kubeAPIEgress:
    - to: [{ipBlock: {cidr: 172.18.0.3/32}}]   # the apiserver endpoint
      ports: [{protocol: TCP, port: 6443}]
console:
  networkPolicy:
    kubeAPIEgress:
      - to: [{ipBlock: {cidr: 172.18.0.3/32}}]
        ports: [{protocol: TCP, port: 6443}]
```

This was measured on Calico 3.32.2 with IPIP encapsulation and kube-proxy in
iptables mode: the controller reached the apiserver on the default rule, the
pmtu probe read 1480 on every pair (Calico's MTU there), and an HTTP target
behind a Service on port 80 with `targetPort: 8080` was refused while a pod
listening on 80 was reached through the default rule.

**An in-cluster peer on any CNI.** An HTTP target, a webhook receiver or an
IdP running as a pod needs a selector peer on the pod's own port, not the
Service's: Cilium never matches the pod against an `ipBlock`, and Calico and
Antrea see the `targetPort`. A list you set replaces the default, so keep an
`ipBlock` rule for the external peers:

```yaml
networkPolicy:
  httpEgress:
    - to:
        - namespaceSelector:
            matchLabels: {kubernetes.io/metadata.name: monitoring}
          podSelector:
            matchLabels: {app.kubernetes.io/name: grafana}
      ports: [{protocol: TCP, port: 3000}]
    - to: [{ipBlock: {cidr: 0.0.0.0/0}}]
      ports: [{protocol: TCP, port: 443}]
```

`console.networkPolicy.webhookEgress` takes the same shape for webhook
receivers. The default `oidcEgress` already carries a `namespaceSelector: {}`
peer on 443 next to the `ipBlock`, which covers an IdP behind an in-cluster
ingress controller listening on 443; an IdP pod on another port needs its own
rule, as [OIDC setup](scenarios/oidc-setup.md) shows.

## Console

The Console is off by default and reads its own config file, rendered by the
chart from `console.*` (it is not part of the `config:` block above). Without
a database it serves read-only pages over Prometheus and the controller API
plus the realtime path (the `/ws` WebSocket, the Events page, pushed matrix
snapshots); PostgreSQL adds persistence and authentication/RBAC. The config
file's every key, default and validation rule
lives in the
[chart README](https://github.com/EsDmitrii/kconmon-ng/blob/main/charts/kconmon-ng/README.md)
and the commented `values.yaml`; this section stays a summary.
The chart's schema (2.5.0 and later) refuses at `helm install` or
`helm upgrade` many console values the console itself would refuse at startup, such as a
zero or negative `console.auth.session.ttl`, `console.controller.timeout`
or `redis.dialTimeout`, or a `console.controller.url` or
`console.prometheus.url` with a trailing slash, so a bad value fails the release instead of crash-looping
the new console pod.

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
  # 1 by default. More than 1 REQUIRES redis.existingSecret: sessions, the rate-limit counters and
  # the realtime fan-out live there, and the chart refuses the combination rather than silently
  # multiplying every rate limit by the replica count.
  replicas: 1
  controller:
    url: "" # empty = derive from this release's controller Service
    timeout: 10s
    # gRPC address of the controller's EventStream; empty = derive from the release's Service.
    grpcAddress: ""
  prometheus:
    url: "" # REQUIRED for the Matrix, Metrics and PromQL pages; empty = those APIs 503
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
    # by the console at startup; this field is not resolved against the RBAC table.
    defaultRole: ""
    # Map a GROUP the identity provider asserts onto a role this console grants. This is what makes
    # an oidc or header install usable from a cold database: role_bindings are created through an
    # API that already needs rbac:manage, so without it nobody could make the first binding.
    # Roles resolve as the UNION of this map and the bindings; a group absent from the map grants
    # nothing, and a value may be a built-in or the name of a custom role.
    groupRoles: {}
    # local/oidc require a database; header requires a non-empty trustedProxyCIDRs.
    header:
      # Identity: the authenticating proxy whose X-Remote-User/X-Remote-Groups are believed.
      trustedProxyCIDRs: []
  # Read in EVERY mode: the proxies whose X-Forwarded-For gives the client address. Never identity.
  # Empty falls back to auth.header.trustedProxyCIDRs.
  clientAddress:
    trustedProxyCIDRs: []
  # Open /ws sockets per console replica; 0 turns one cap off, a negative value fails startup.
  websocket:
    maxConnections: 1024
    maxConnectionsPerAddress: 256
    maxConnectionsPerSubject: 32
```

`controller.events.enabled` turns on the controller's `EventStream.WatchEvents`
RPC and the `"events"` capability flag on its `GET /api/v1/version`. It is
leader-only (passive replicas reject subscriptions) and needs a controller
image that includes the event stream, v1.4.0 or newer. While it is `false`
the chart omits the `events` key from the rendered controller config
entirely, so an older image, which would reject the unknown key at startup,
keeps rolling. Enabling it is what commits the fleet to the newer image.

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

The Console does not use the Redis client's server-assisted caching, which
needs RESP3 and `CLIENT TRACKING`, so a RESP2-only server such as Redis 5, or
a managed endpoint that refuses `CLIENT TRACKING`, works as the shared bus for
sessions, rate limits and realtime (console 2.5.0 and later). The Console
falls back to the in-process bus for a server it cannot reach at startup, or
one that refuses the handshake (a wrong password, an ACL, a TLS mismatch): each replica then keeps
its own sessions and counters until it is restarted. The log line tells the
two apart: `redis unreachable at startup` for a failed dial, DNS lookup or
timeout, and `redis refused the connection handshake at startup
(credentials, ACL, TLS scheme or DSN)` for a server that answered and
refused.

The Console serves `GET /ws` (one multiplexed WebSocket per browser tab) at
the top level of `console.service.port`, alongside its `/api/v1/*` REST
endpoints. An ingress in front of it must allow upgrades and **preserve
`Host`**. The origin check compares the browser's `Origin` header host against
the request host, so a proxy that rewrites `Host` (or forwards a mismatched
`Origin`) makes every upgrade refused, and the UI silently falls back to 15s
polling. A proxy that strips `Origin` entirely still upgrades: an absent
header is allowed, since non-browser clients never send one.

Since 2.5.0 each replica caps its open `/ws` sockets (console config
`websocket.*`, Helm `console.websocket.*`): `websocket.maxConnections`
(1024) for the replica, `websocket.maxConnectionsPerAddress` (256) per client
address and `websocket.maxConnectionsPerSubject` (32) per user or API token.
Anonymous callers share one subject, so only the total and per-address caps
apply to them. `0` turns a cap off.
A socket over a cap is closed with code 1013 and a reason naming the cap, the
browser reconnects with backoff, and
`kconmon_ng_console_ws_refused_total{limit="total|address|subject"}` counts
the refusals. The chart writes the block only for console images 2.5.0 or
newer, since an older console refuses the unknown key. A browser tab holds a
socket only while a page in it subscribes to a realtime topic: 30 seconds
after the last subscription goes, the tab closes its socket and dials again
on the next realtime page, so parked tabs do not hold cap slots.

**Behind an Ingress or a NAT, set `console.clientAddress.trustedProxyCIDRs`
in every auth mode** (console config `clientAddress.trustedProxyCIDRs`; the chart writes it only
for console images 2.5.0 or newer). The console takes the client address for
the per-address login budget, the OIDC start and callback budgets, the anonymous runs and PromQL budgets, the
per-address `/ws` cap and the audit log's `remoteAddr` from
`X-Forwarded-For`, but only when the TCP peer is inside one of these CIDRs.
Without them every client shares the ingress's address, and one noisy client
can make every user's OIDC sign-in answer 429. Every `X-Forwarded-For` line
is read as one list and walked from the right past trusted proxies; a hop may
carry a port, and a hop that is not an IP address stops the walk, so the
trusted peer's own address is used. An invalid CIDR fails startup in any
mode. When the OIDC start budget trips with no trusted proxies configured,
the console logs a one-time warning naming the key. An IPv6 client is
budgeted per /64. A proxy inside the list can name any
client address it likes, so an anonymous or credential-less request it
forwards, once its own budgets admit it, also spends a runs or PromQL budget
of that proxy's own, twenty times one client's; signed-in users'
per-subject budgets are not affected. Sign-in spends no budget of the
proxy's, since that would be one counter for everyone behind the ingress,
which any of them could spend. What bounds sign-in behind a proxy is each
account's per-username limit, whatever address a request names, a start of
an OIDC sign-in that stores nothing on the server, and the fixed number of
password-check slots. List the ingress controller's own addresses rather
than the whole pod network where you can.

The list never authenticates anyone. `console.auth.header.trustedProxyCIDRs`
is the identity list: in `header` mode it names only the authenticating proxy
whose `X-Remote-User` and `X-Remote-Groups` the console believes, and trust is
decided on the TCP peer alone, as
[Enable the console](getting-started/enable-the-console.md#why-header-mode-ships-no-default-for-trustedproxycidrs)
explains. Keep the ingress controller's pod network out of it unless that
ingress is the authenticating proxy, or the identity headers of every request
it forwards are believed. While
`clientAddress.trustedProxyCIDRs` is empty, or the console image is older
than 2.5.0, the header list also gives the client address, which is what
values written before 2.5.0 rely on; the chart therefore renders it into the
console config in every mode once it is set.

`database.existingSecret` names a Secret holding a `postgres://` DSN. The
chart installs no database and does not care which one answers: CloudNativePG,
Percona, RDS, a plain StatefulSet; the chart README documents the stack it is
tested against.

Every console secret (the database DSN, the local-mode bootstrap admin
password, the OIDC client secret) mounts as a file under one directory,
`/etc/kconmon-ng-console-secrets/`, group-readable via
`console.podSecurityContext.fsGroup` (default matching the distroless nonroot
gid). Rotation behaves differently per Secret kind. The Deployment's
annotations checksum the config and every chart-managed Secret but one, so a
`secret.create` Secret rolls the Deployment by itself; rotating an *existing*
Secret the chart only references is an operator-initiated restart, because the
chart cannot checksum content it does not render. The exception is the
local-mode bootstrap admin password (`console.auth.local.secret.password`): it
only ever seeds an empty users table, so changing it rolls nothing since
2.5.0.

The console container gets `GOMEMLIMIT` from its own `limits.memory`
(`resourceFieldRef`, in bytes), so the Go garbage collector paces itself
against the pod limit (256Mi by default) before the OOM killer does. Without
a memory limit it resolves to the node's allocatable memory.

`auth.mode=local|oidc` requires `database.existingSecret`, and with
`console.replicas > 1` also `redis.existingSecret`: sessions live in
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
re-registration. Requires `controller.leaderElection: true`: the controller
starts its node informer only with leader election on.
