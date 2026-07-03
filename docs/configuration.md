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

## Zone auto-discovery

On registration the controller resolves each agent's zone from its node's
`failureDomainLabel` (default `topology.kubernetes.io/zone`) and the agent
adopts it, so `source_zone`/`destination_zone` labels are populated with no
per-agent config. An explicit `agent.zone` value (or `KCONMON_NG_ZONE`) always
wins. A node label change after registration is broadcast to peers immediately;
the agent's own `source_zone` refreshes on its next re-registration. Requires
`controller.leaderElection: true` — the node informer runs only on the leader.
