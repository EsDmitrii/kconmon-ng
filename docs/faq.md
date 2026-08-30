# FAQ

## General

### How does this relate to the original kconmon?

The name is inherited, so credit first:
[kconmon](https://github.com/Stono/kconmon) by Karl Stoney established this
exact shape: per-node agents, a controller handing out peer lists, per-pair
Prometheus metrics enriched with zones. It is written in Node.js and was
archived in June 2026. kconmon-ng is a ground-up Go implementation of the
same idea, not a fork (no code is shared), extended with ICMP, reactive MTR
tracing and the Console. Here is the comparison against kconmon, goldpinger
and kubenurse, included from the project README:

--8<-- "README.md:329:348"

### Do I need the Prometheus Operator?

No. With it you get the bundled `ServiceMonitor` and `PrometheusRule`
objects; without it, [one endpoints-role scrape
job](getting-started/install-15-min.md#no-prometheus-operator) covers the
whole fleet, and the [alert rules](metrics.md#default-alerting-rules) are
plain PromQL you can lift into your own `rule_files`.

### Do I need the Console?

Also no. It is off by default, and everything it shows comes from Prometheus
and the controller API: three Grafana dashboards ship with the project, the
metric names are stable and documented, and `kubectl-kconmon` runs one-shot
checks from a terminal. The Console adds the N×N matrix, incident timelines,
MTR path history, the Time Machine and managed alert rules on top of the
same data.

### Does it monitor pod-to-pod or service-to-service traffic?

Neither, strictly speaking: it measures **node-to-node transport**. Agents
probe each other pod-IP to pod-IP, one series per ordered node pair per
protocol, which is the layer a CNI bug, a conntrack overflow or a fabric
problem lives in. It does not trace your application's connections. For the
path through ingress, Service and API server, or an HTTP-level pod graph,
the [comparison table above](#how-does-this-relate-to-the-original-kconmon)
points at the tools built for those questions; DNS, HTTP and
[external checks](scenarios/external-targets.md) cover named endpoints from
each node's point of view.

## Operations

### What privileges do the agents need?

None added. ICMP and MTR ride the unprivileged ICMP socket that the
`net.ipv4.ping_group_range` sysctl opens (a kubelet *safe* sysctl the chart
sets), and every component runs non-root, drops `ALL` capabilities and
passes restricted Pod Security Standards unchanged.

### What happens when the controller is down?

The fleet keeps measuring. Agents probe the last known peer list while
registration retries in the background, and an agent's health endpoints come
up before its first registration, so a controller outage neither blinds the
mesh nor crash-loops the DaemonSet. What you lose until it returns is
coordination: peer-list updates, zone changes, on-demand checks. Two bundled
rules, `KconmonControllerDown` and `KconmonAgentsMissing`, page you about
the monitor itself.

### Does restarting kconmon-ng write false loss into its own metrics?

No. An agent deregisters on shutdown, so its peers stop probing it instead
of recording failures, and a departed peer's per-pair gauges are dropped
rather than left as ghost readings.

### How do I change checker settings without a rollout?

The binaries watch their config file and hot-reload it, so an edited
ConfigMap propagates with no restart. A `helm upgrade`, though, still rolls
pods on a config change: each workload carries a checksum annotation over
the config it consumes, so a values edit cannot leave old and new config
running side by side. The rolls are scoped to what actually changed:
shared `config.*` values are checksummed by the agent DaemonSet and the
controller, while `console.*` values live in the console's own ConfigMap and
Secrets and roll only the console. One caveat cuts the other way: the chart
cannot checksum the *content* of a Secret it merely references
(`existingSecret` names), so rotating a DSN in place rolls nothing and needs
a `kubectl rollout restart` by hand. Either way the config is parsed
strictly: unknown keys or invalid settings fail startup, and on hot-reload
an invalid config is rejected while the previous one stays active.

### Can I run an agent on a host outside the cluster?

Not yet, and do not work around it by exposing the controller's gRPC port:
that hands the probe mesh to anyone who can reach it.
[External agents](external-agents.md) states exactly what holds today, what
blocks it and what is planned; the supported answer meanwhile is the reverse
direction, [external checks](scenarios/external-targets.md).

## Scale

### How many nodes can it handle?

50–100 nodes is the production-proven envelope. The cost centre is not the
probes, it is the metrics: each directed pair keeps roughly 70 active series
and pairs grow as N×(N−1), which lands around 690k series at 100 nodes. The
full arithmetic is in
[Scaling and cardinality](metrics.md#scaling-and-cardinality); a sparse mesh
for larger fleets is on the roadmap.

### My Prometheus is drowning — what are the levers?

Three levers, and they work through two different mechanisms. The first two
are scrape-time: `agent.metrics.detail: counters-only` drops the per-pair
histograms (~70 → ~10 series per pair) while every pair alert keeps firing,
and `zone-only` keeps only the Z² zone-pair plane. Both render as
`metricRelabelings` on the agent ServiceMonitor, so they require
`serviceMonitor.enabled` (the chart refuses the combination otherwise); on
plain Prometheus, copy the equivalent `metric_relabel_configs` from
[Levers that exist today](metrics.md#levers-that-exist-today). The third is
a config change, not a relabeling: disabling a checker
(`config.checkers.<type>.enabled`) edits the shared ConfigMap and rolls the
agent pods, and the agent then stops exporting that protocol's families at
the source.

!!! warning "Check your agent version before `zone-only`"
    The zone family comes from the agent image, and the chart still pins
    agent 2.0.3, which does not export it. On such a fleet `zone-only` drops
    the per-pair series with nothing replacing them: Prometheus goes dark on
    the mesh, and the two zone alerts sit inert. Upgrade the agent image
    first; the [v2.2.0 release notes](reference/release-notes.md) spell out
    the order.

### Why is everything in one zone / why does Topology say no zone?

The controller reads zones from the node label named by
`config.failureDomainLabel` (default `topology.kubernetes.io/zone`). Unlabelled
nodes have no zone, and everything zone-aware (the zone metric family, the
two zone alert rules, the heatmap) is inert until the labels exist. Label
the nodes; agents pick the zone up via the controller, no agent config
needed.

## Security

### Why is the agent–controller channel plaintext?

A bounded trade-off, made once and stated out loud: the port is never
exposed outside the cluster, and the optional NetworkPolicy pins it further.
Outside that boundary the same channel is disqualifying, since anyone who
can reach it can register agents, receive the full peer list and steer the
fleet's probes. That is why external agents are planned around a **separate**
authenticated TLS gateway rather than around "just expose the port". Details
in [External agents](external-agents.md).

### Is the Console safe to expose?

Treat `console.auth.mode` as the boundary: the default is `anonymous` with
the `viewer` role, which is fine for a port-forward and wrong for an
ingress. Set a real mode (`local`, `header` or
[`oidc`](scenarios/oidc-setup.md)) before putting it behind one, and narrow
`console.networkPolicy.ingressFrom` to whatever fronts the UI. That cannot
lock you out, since `kubectl port-forward` does not traverse NetworkPolicy.

### Why does Prometheus scrape a separate port?

The controller's API shares its HTTP port and authenticates nothing, so
`/metrics` gets a listener of its own (`config.metricsPort`, 9091). The
scrape NetworkPolicy rule opens only that port, and only from
`networkPolicy.prometheusNamespace`: letting a scraper in must not mean
letting its whole namespace drive the fleet.

### Can a compromised Console leak probe traffic outside the cluster?

External probing is double-gated in places the Console cannot reach: the
**agent's** CIDR allowlist (`config.checkers.external.allowedCidrs`) and the
cluster's egress policy (`networkPolicy.externalEgress`). A console-declared
target outside the allowlist is refused by every agent and counted in
`kconmon_ng_external_denied_total`. The only Console feature that talks to
anything outside the cluster on its own is optional MTR hop enrichment
(rDNS/GeoIP), off by default — and webhooks you configure yourself, which
are HMAC-signed with per-endpoint secrets encrypted at rest.
