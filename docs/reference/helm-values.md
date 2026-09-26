# Helm values

The chart installs the **monitor** (agent, controller, console) and nothing
else: PostgreSQL, a Redis-compatible bus and Prometheus are infrastructure it
consumes, each configured by one DSN or URL. The
[full values.yaml below](#full-valuesyaml) documents every key inline and is
the authoritative reference; this table is the short list you will actually
touch.

## Key values at a glance

| Key | Default | What it does |
| --- | --- | --- |
| `config.metricsPrefix` | `kconmon_ng` | Prefix for every exported metric; changing it renames all of them |
| `config.checkers.tcp.enabled` | `true` | TCP checker (interval `5s`, timeout `1s`) |
| `config.checkers.udp.enabled` | `true` | UDP checker (interval `5s`, timeout `250ms`, `packets: 5`) |
| `config.checkers.icmp.enabled` | `true` | ICMP checker (interval `5s`, timeout `1s`); unprivileged socket, no added capabilities |
| `config.checkers.pmtu.enabled` | `true` | Path MTU probe (interval `60s`, timeout `500ms` per datagram, `size: 0` = the MTU of the route to the peer: a CNI route MTU such as Cilium's when set, else the egress device's); DF-marked UDP to the peer's echo port, no added capabilities. Keep `interval` at 28× `timeout` (`14s` at the default) or more and under `3m`; the agent warns outside that range, since from `3m` on `PathMTUBlackHole` gets few probes per window. The chart writes a `pmtu` key only when it differs from these defaults, and tuning one needs agent images 2.5.0 or newer |
| `config.checkers.dns.enabled` | `true` | DNS checker (interval `5s`, timeout `2s`) |
| `config.checkers.http.enabled` | `false` | HTTP checker; `targets` are required when enabled |
| `config.checkers.external.enabled` | `false` | Probes to non-peer destinations, gated by `allowedCidrs`; see [External targets](../scenarios/external-targets.md) |
| `config.checkers.mtr.cooldown` | `60s` | Minimum gap between reactive MTR traces for the same (src, dst) pair |
| `config.checkers.mtr.maxHops` | `30` | Traceroute hop ceiling (1–64) |
| `config.controllerAgentTtl` | `30s` | Evict an agent missing heartbeats for this long; any Go duration spelling (`45s`, `1.5m`, `.5m`), min `10s`, and the chart refuses less at install |
| `agent.tolerations` | `[{operator: Exists}]` | Run the agent on every node, tainted ones included |
| `agent.metrics.detail` | `full` | Scrape-time cardinality valve on the agent `ServiceMonitor` and the external-agent `ScrapeConfig`: `full` / `counters-only` / `zone-only` (78 / 14 / 0 series per directed pair); needs `serviceMonitor.enabled` or `scrapeConfig.externalAgents.enabled` |
| `agent.pingGroupRange` | `true` | Render the `net.ipv4.ping_group_range` sysctl the ICMP socket needs; not rendered under `agent.hostNetwork`, where the node OS must set it |
| `agent.hostNetwork` | `false` | Run the agents in the node's network namespace so external hosts can reach them without a routable pod network. Changes what every in-cluster pair measures (node-to-node underlay, not the CNI datapath); needs a `privileged` namespace, free ports on every node, and `networkPolicy.nodeCidrs` when policies are on. Read [External agents](../external-agents.md#when-the-pod-network-does-not-route) first |
| `agent.dnsPolicy` | `""` | Pod `dnsPolicy`, passed through verbatim (`ClusterFirst`, `ClusterFirstWithHostNet`, `Default`, `None`); empty renders `ClusterFirstWithHostNet` under `agent.hostNetwork` and nothing otherwise |
| `agent.updateStrategy` | `RollingUpdate`, `maxUnavailable: 1` | DaemonSet rollout, passed through verbatim; raise on large fleets |
| `controller.replicaCount` | `1` | Controller replicas; only the leader is active |
| `controller.leaderElection` | `true` | Leader election; `false` also disables zone enrichment and `expected_agents` |
| `controller.events.enabled` | `false` | Domain event stream for the Console's realtime pages (leader-only) |
| `controller.externalGateway.enabled` | `false` | The TLS gateway for [external agents](../external-agents.md): a second gRPC listener, exposed by its own NodePort/LoadBalancer Service |
| `controller.externalGateway.port` | `9443` | Gateway listener; must differ from `config.{httpPort,grpcPort,metricsPort}` |
| `controller.externalGateway.tls.secretName` | `""` | `kubernetes.io/tls` Secret with the serving pair; REQUIRED when enabled. `tls.clientCaKey` names the client-CA bundle key in the same Secret; empty means token-only mode |
| `controller.externalGateway.bootstrapToken.secretName` | `""` | Secret holding the shared bearer token (< 16 chars is refused); REQUIRED when enabled |
| `controller.prometheusSD.enabled` | `true` | Serve `GET /api/v1/prometheus/sd`, the external-agent target list, on `httpPort` and `metricsPort`; `false` answers 404 on both. Written to the shared ConfigMap only when `false`, so an older controller image never sees the key |
| `console.enabled` | `false` | Deploy the [Console](../getting-started/enable-the-console.md) |
| `console.replicas` | `1` | More than 1 REQUIRES `redis.existingSecret`; the chart refuses the combination otherwise |
| `console.prometheus.url` | `""` | Required by the data pages (Matrix, Metrics, PromQL), which answer `503` without it |
| `console.networkPolicy.prometheusTargetPort` | `0` | The pod port behind `console.prometheus.url` when that URL names a Service whose `targetPort` differs from its port (the Bitnami Thanos chart: Service 9090, pod 10902). A NetworkPolicy sees the pod port after the Service DNAT, so the default Prometheus egress rule then opens both ports. `0` opens the URL's port only; ignored when `console.networkPolicy.prometheusEgress` is set. `redisEgress` and `databaseEgress` have no such key: a Service mapping 6379 or 5432 to another `targetPort` needs the list set on the pod port |
| `console.auth.mode` | `anonymous` | `anonymous` / `local` / `header` / `oidc` |
| `console.auth.groupRoles` | `{}` | IdP group → console role map; what makes an `oidc`/`header` install usable from a cold database |
| `console.auth.header.trustedProxyCIDRs` | `[]` | Identity: the authenticating proxy whose `X-Remote-User`/`X-Remote-Groups` the console believes. Required in `header` mode; name that proxy and nothing wider. Gives the client address too, but only while `console.clientAddress.trustedProxyCIDRs` is empty |
| `console.clientAddress.trustedProxyCIDRs` | `[]` | Proxy networks whose `X-Forwarded-For` gives the client address, in every mode; never identity. Behind an Ingress or a NAT, list the ingress controller's addresses (its pod CIDR at the widest; a pod inside the list can name any client address), or the per-address login, OIDC, runs and PromQL budgets, the `/ws` cap and the audit address all see one shared address. Empty falls back to the header list, and so does a console image older than 2.5.0, for which the chart does not write the key |
| `console.websocket.maxConnections` / `maxConnectionsPerAddress` / `maxConnectionsPerSubject` | `1024` / `256` / `32` | Open `/ws` sockets per console replica, per client address and per user or token; `0` turns a cap off. A refused socket closes with 1013 and counts in `kconmon_ng_console_ws_refused_total` |
| `console.alerting.enabled` | `false` | Console-managed alert rules, reconciled into one `PrometheusRule`; needs a database and the operator CRD |
| `console.webhooks.existingSecret` | `""` | Secret with the AES-256-GCM key encrypting webhook signing secrets at rest (key `console-webhooks-encryption-key`); empty leaves endpoint create and test at `503`; see [Set up alerting](../scenarios/set-up-alerting.md) |
| `console.scheduler.enabled` | `false` | The schedule/dispatch loop for [scheduled and external checks](../concepts/checks-runs-schedules.md) |
| `console.scheduler.tickInterval` | `5s` | Poll cadence of that loop; must be > 0 when enabled |
| `database.existingSecret` | `""` | Secret holding a `postgres://` DSN; empty means an in-memory console |
| `database.retentionDays` | `90` | Daily prune of stored history; `0` keeps everything |
| `redis.existingSecret` | `""` | Secret holding a `redis://` DSN; empty means the in-process bus (single replica only) |
| `dashboards.enabled` | `false` | Ship the Grafana dashboards as sidecar-labelled ConfigMaps |
| `serviceMonitor.enabled` | `false` | Prometheus Operator `ServiceMonitor` for agents, controller and console |
| `scrapeConfig.externalAgents.enabled` | `false` | Prometheus Operator `ScrapeConfig` that reads the controller's HTTP SD endpoint and scrapes [external agents](../external-agents.md#scraping-external-agents); refused without `controller.externalGateway.enabled` or with `controller.prometheusSD.enabled=false` |
| `scrapeConfig.externalAgents.labels` | `{}` | Selector labels your Prometheus requires on the object; kube-prometheus-stack selects only `release: <its release name>`, an empty `scrapeConfigSelector` needs nothing |
| `scrapeConfig.externalAgents.jobName` | `""` | Job label; empty means `<release>-agent-external`. Keep `kconmon` in it (the dashboards filter on it); `KconmonExternalAgentDown` follows whatever name you set |
| `scrapeConfig.externalAgents.refreshInterval` | `30s` | How often Prometheus re-reads the target list; matches `config.controllerAgentTtl` |
| `scrapeConfig.externalAgents.interval` | `""` | Scrape interval; empty falls back to `serviceMonitor.interval` |
| `prometheusRule.enabled` | `false` | The fourteen [built-in alert rules](../metrics.md#default-alerting-rules) as one `PrometheusRule` (thirteen on by default) |
| `prometheusRule.<alertName>` | all enabled | Per-rule `enabled` / `threshold` / `for` / `severity` knobs; `nodeUnreachable` and `nodeIsolated` also take `minPeers` (`2`), and `pathMtuBlackHole` drives `ZonePathMTUBlackHole` too |
| `prometheusRule.pathMtuBlackHole.sustainedThreshold` | `0.1` | Second arm of `PathMTUBlackHole` and `ZonePathMTUBlackHole`: a pmtu failure ratio over 30m above this, with at least two failed probes in that window and one in the last 10m, fires as well, which catches a black hole on one of several ECMP paths. A ratio 0.0-1.0; `1` turns the arm off |
| `prometheusRule.externalAgentDown.enabled` | `false` | `KconmonExternalAgentDown`: an external agent the SD endpoint lists at `up == 0` for `for` (`5m`, `warning`); the job exists only with the ScrapeConfig or a hand-written job named `*agent-external*` |
| `prometheusRule.additionalRules` | `[]` | Your rules, appended verbatim |
| `networkPolicy.enabled` | `false` | NetworkPolicies per component: `<fullname>-agent`, `<fullname>-controller` (with its `-apiserver` egress policy, and `-gateway` when the gateway is on) and, with the console on, `<fullname>-console`. On Cilium it adds `CiliumNetworkPolicy` objects, see the next row |
| `networkPolicy.prometheusNamespace` | `""` | REQUIRED for the scrape rule to render when policies are on; unset means `up == 0`, visibly |
| `networkPolicy.ciliumKubeAPIEgress` | `auto` | `auto` / `true` / `false`. Renders the `<fullname>-kube-apiserver` `CiliumNetworkPolicy`, allowing TCP 443/6443 to the `kube-apiserver` entity for the controller, and for the console with `console.kubernetesContext.enabled` or `console.alerting.enabled`; Cilium matches no `ipBlock` against that entity, so without it the controller stays NotReady. With `agent.hostNetwork` or `controller.externalGateway.enabled` it also renders `<fullname>-node-ingress`, which admits the `remote-node` and `host` entities to the controller's `grpcPort` and gateway port, since `nodeCidrs` and `externalAgentCidrs` match no node IP on Cilium. `auto` renders it when the cluster serves `cilium.io/v2` `CiliumNetworkPolicy`; plain `helm template` needs `--api-versions cilium.io/v2/CiliumNetworkPolicy` or `true`. See [NetworkPolicy on Cilium, Calico and Antrea](../configuration.md#networkpolicy-and-cilium) |
| `networkPolicy.httpEgress` | `[]` | Egress for HTTP checker targets; empty renders TCP 80/443 to `0.0.0.0/0` minus `clusterCIDRs`. An in-cluster target needs a selector peer on the pod's own port on every CNI (Cilium never matches a pod IP against an `ipBlock`, Calico and Antrea see the `targetPort` after kube-proxy's DNAT); a set list replaces the default, so keep an `ipBlock` rule for external targets. `console.networkPolicy.webhookEgress` works the same for webhook receivers |
| `networkPolicy.clusterCIDRs` | `[]` | IPv4 pod and Service CIDRs, rendered as `except` entries of the default `0.0.0.0/0` rules of `httpEgress`, `console.networkPolicy.webhookEgress`, `oidcEgress` and `geoipEgress`. On Calico and Antrea that `ipBlock` also matches pod IPs, so without this list those defaults open every pod on their ports; Cilium never matches a pod against it. The default `oidcEgress` also carries a `namespaceSelector: {}` peer on 443, so it keeps every pod open on 443 with this list too; only an explicit `console.networkPolicy.oidcEgress` narrows it. The apiserver defaults (`networkPolicy.kubeAPIEgress`, `console.networkPolicy.kubeAPIEgress`) get no `except` and keep every pod open on 443/6443 to the controller and to a console that calls the apiserver until you name the apiserver endpoint in them. Entries must be network addresses: `/0` and host bits set fail the render. See [NetworkPolicy on Cilium, Calico and Antrea](../configuration.md#networkpolicy-and-cilium) |
| `networkPolicy.externalAgentCidrs` | `[]` | Source CIDRs of external agents, opened on the gateway port toward the controller pods alone; REQUIRED when the gateway and this policy are both on (plain CIDR strings with a prefix length). Mind NAT, see [External agents](../external-agents.md); on Cilium a node IP here matches nothing and `ciliumKubeAPIEgress` admits node sources instead |
| `networkPolicy.externalPeerCidrs` | `[]` | The probe half: the same hosts' CIDRs spliced into the agent↔agent rules in both directions (UDP `grpcPort`, TCP `httpPort`, the ports-less ICMP/MTR rule), never into the gateway rule. Without it an external agent registers and every cell between it and the cluster stays red |
| `networkPolicy.dnsEgress` | `[]` | Replaces the default cluster DNS egress rule (UDP/TCP 53 to any pod) in all three policies; needed for NodeLocal DNSCache or any host-network resolver, which a `namespaceSelector` cannot match. Keep `namespaceSelector: {}` in the list if the kube-dns pods must stay reachable |
| `networkPolicy.nodeCidrs` | `[]` | Node CIDRs admitted to the controller's gRPC port; REQUIRED with `agent.hostNetwork` when policies are on, since host-network agents register from node IPs that no pod selector matches, and the chart refuses to render the policy without it. On Cilium this `ipBlock` matches nothing; the `<fullname>-node-ingress` `CiliumNetworkPolicy` from `ciliumKubeAPIEgress` admits the nodes there |

Secrets follow one pattern everywhere: `existingSecret` names a Secret you
created (recommended), or a sibling `secret.create: true` block lets the
chart render it, meant for a secrets injector's `${vault:...}` placeholders,
not for literals. The
[chart README](https://github.com/EsDmitrii/kconmon-ng/tree/main/charts/kconmon-ng#chart-managed-secrets)
lists every consumer and its key.

## Full values.yaml

The complete, commented `values.yaml` of the current chart, embedded from
the repo at build time so it cannot drift from what the chart ships:

??? example "charts/kconmon-ng/values.yaml: every key, documented inline"

    ```yaml
    --8<-- "charts/kconmon-ng/values.yaml"
    ```
