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
and kubenurse:

| | kconmon-ng | [kconmon](https://github.com/Stono/kconmon) | [goldpinger](https://github.com/bloomberg/goldpinger) | [kubenurse](https://github.com/postfinance/kubenurse) |
|---|---|---|---|---|
| Status | active | archived (June 2026) | active | active |
| Language | Go | Node.js | Go | Go |
| Architecture | agent DaemonSet + controller; peer list pushed over gRPC | agent DaemonSet + controller; peers fetched every 5s | one DaemonSet; every pod queries the Kubernetes API for peers | one DaemonSet |
| Node-to-node probes | TCP, UDP and ICMP on every ordered pair, per protocol | TCP (HTTP GET), UDP | HTTP between pods; UDP optional, off by default | HTTP between neighbours |
| Other checks | DNS, HTTP(S) URLs, external targets behind an agent-side CIDR allowlist | DNS | DNS; TCP/HTTP(S) to external targets | API server (direct and via DNS), ingress, service |
| On probe failure | reactive MTR trace, per-hop path history | — | — | — |
| Zone awareness | `source_zone`/`destination_zone` on every peer metric | zone labels on metrics | — | — |
| Behaviour at scale | full N×N mesh by default; sparse mesh since v2.3.0 | full N×N mesh | full mesh | caps neighbour checks at 10 nodes by default |
| UI | optional Console: matrix, topology, incidents, Time Machine, alert rule editor | — (sample Grafana dashboard) | built-in connectivity graph | — (Grafana dashboard provided) |

The table states what each project's README claims as of August 2026; a `—`
means the README does not claim the feature, not that a flag or fork cannot add
it. Reach for **goldpinger** when an HTTP-level "can pods see each other" graph
with a tiny footprint is enough, for **kubenurse** when the question is the path
through ingress, service and API server rather than raw node-to-node transport,
and for **kconmon-ng** when you need per-protocol pair evidence — the
UDP-but-not-TCP class of failure — with the bad hop already traced.

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

Yes, since v2.3.0. The same agent installs on a bare host from the deb or
rpm package and joins the mesh through a **separate TLS gateway** on the
controller: it authenticates with a bearer token and can prove which agent
it is with a client certificate the gateway pins. The in-cluster plaintext
gRPC port stays unexposed, and opening it is still the wrong shortcut, since
it hands the probe mesh to anyone who can reach it.
[External agents](external-agents.md) walks through the trust model, the
cluster side and the host side; read
[What v1 does not do](external-agents.md#what-v1-does-not-do) before you
plan a rollout. 2.4.0 closes the largest of those gaps: per-agent ports, a
Prometheus HTTP SD endpoint that makes external hosts scrapable without
hand-written targets
([Scraping external agents](external-agents.md#scraping-external-agents)),
`agent.hostNetwork` for clusters whose pod network the hosts cannot route
to, and a console that shows external members as what they are. The one
rule to carry into a mixed fleet: keep one port set until every agent,
packaged hosts included, runs 2.4.0. If what you need is probing *toward* an external
destination, [external checks](scenarios/external-targets.md) do that from
the in-cluster agents with no new trust surface.

### Does the agent run on Windows?

No, and it is not planned without a named user. The agent compiles for
`windows/amd64`, and four checkers (TCP, UDP, DNS, HTTP) would work as
written, but the two that make this tool what it is do not: ICMP and MTR
sit on a datagram ICMP socket that `golang.org/x/net/icmp` supports only on
Linux and Darwin by its own contract, and the raw-socket alternative on
Windows needs the Administrators group, so an agent that also runs on-demand
probes for the controller would run as SYSTEM. On top of that, Go's
`time.Now()` on Windows is tick-granular (up to 15.6 ms), so a 0.3 ms LAN
round trip reads as zero or as one tick and the histograms look valid while
being wrong. A Windows vantage point is tracked as a tier-2 backlog item
(TCP, UDP, DNS and HTTP, with ICMP and MTR explicitly unsupported); the
trigger is a concrete host that needs it.

## Scale

### How many nodes can it handle?

50–100 nodes is the production-proven envelope. The cost centre is not the
probes, it is the metrics: each directed pair keeps roughly 75 active series
and pairs grow as N×(N−1), which lands around 740k series at 100 nodes. The
full arithmetic is in
[Scaling and cardinality](metrics.md#scaling-and-cardinality). For larger
fleets, `topology.mode: sparse` (since v2.3.0) trims the probed pairs to a
ring over the node names plus cross-zone chords, so the series count grows
roughly linearly with node count instead of quadratically.

### My Prometheus is drowning — what are the levers?

Three levers, and they work through two different mechanisms. The first two
are scrape-time: `agent.metrics.detail: counters-only` drops the per-pair
histograms (~75 → ~12 series per pair) while every pair alert keeps firing,
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
    The zone family comes from the agent image, and every chart since 2.3.0
    pins an agent that exports it, so a default install is fine. The trap is
    a fleet running an older agent image behind a newer chart (an image tag
    override, or host packages nobody upgraded): there `zone-only` drops the
    per-pair series with nothing replacing them, Prometheus goes dark on the
    mesh, and the two zone alerts sit inert. Upgrade the agent image first;
    the [v2.3.0 release notes](reference/release-notes.md) spell out the
    order. The same rule carries into 2.4.0, which adds per-agent ports,
    Prometheus HTTP SD, `agent.hostNetwork` and console awareness of
    external agents: each arrives with the image, and a mixed fleet falls
    back to the older behaviour rather than breaking.

### Why does PathMTUBlackHole fire when TCP works fine?

Because TCP is the one protocol that can be made to fit. A network that runs
below the interface MTU often clamps the TCP MSS at the edge, so handshakes and
TCP transfers stay inside the smaller path while large UDP datagrams, QUIC
included, die without an ICMP error. The probe reports exactly that: full-size
datagrams do not cross.

If that network is deliberate and your workloads are TCP-only, probe at the
size it really carries with `config.checkers.pmtu.size`, or switch the rule off
with `prometheusRule.pathMtuBlackHole.enabled: false`. See
[When the black hole is by design](scenarios/mtu-black-hole.md#when-the-black-hole-is-by-design).

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
fleet's probes. That is why external agents come in through a **separate**
authenticated TLS gateway (shipped in v2.3.0) rather than through "just
expose the port". Details in [External agents](external-agents.md).

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
