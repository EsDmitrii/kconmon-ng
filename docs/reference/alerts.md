# Alert runbooks

One section per built-in alert of the chart's `PrometheusRule`
(`prometheusRule.enabled: true`). Each alert carries a `runbook_url` that
points at its section here, and a `namespace` label with the release
namespace, so notification templates and Alertmanager routes can tell
kconmon-ng alerts apart. The rules themselves and the reasoning behind every
expression are in [Metrics and alerting](../metrics.md#default-alerting-rules);
the knobs are `prometheusRule.<alertName>.{enabled,threshold,for,severity}`
in the [Helm values](helm-values.md).

These rules are evaluated by Prometheus and delivered by Alertmanager whether
or not the console runs. The console's own alerting (`console.alerting.enabled`)
is a separate layer for rules built in its UI; see
[Set up alerting](../scenarios/set-up-alerting.md).

## UDPLossHigh

UDP packet loss on one directed pair has stayed above the threshold
(`prometheusRule.udpLossHigh.threshold`, 50%) for `for` (5m).

- Open the pair on the console Investigate page, or the "kconmon-ng / Node
  Detail" dashboard with `node=<source_node>`. Loss from the source to every
  peer points at the source node; loss to one peer only points at that link
  or at the destination.
- Compare ICMP loss on the same pair: both lossy means the path, UDP alone
  means a policy or a rate limit on UDP.

## TCPChecksFailing

More than the threshold (`prometheusRule.tcpChecksFailing.threshold`, 5%) of
TCP probes on one pair failed over the last 5m.

- On the "kconmon-ng / Overview" worst-pairs table or the console, check
  whether UDP and ICMP fail on the same pair. All three failing is a path
  problem; TCP alone is the listener on the destination agent or a network
  policy that admits UDP and ICMP but not TCP.

## PathMTUBlackHole

Packets up to the size in the alert cross from the source to the destination,
larger ones are dropped and no ICMP "fragmentation needed" comes back. Pings
and TCP handshakes still pass, so the other pair alerts stay quiet while large
transfers and big UDP datagrams (large DNS responses, QUIC) stall.

- Compare the MTU the source pod uses with what the path carries. On the
  source node: the MTU of the CNI's devices (`ip link` on the host: `cilium_*`,
  `vxlan.calico`, `flannel.1`, the pod's `lxc*`/`cali*` veth) against the
  physical or private NIC minus the encapsulation overhead (VXLAN and Geneve
  take 50 bytes, WireGuard 60 to 80).
- A single source node firing to all its peers usually means that node's CNI
  picked a different MTU, for example because the interface it was told to
  use (`devices` in Cilium) has a different name on that node.
- Check whether ICMP type 3 code 4 is filtered on the path: with it allowed,
  the kernel learns the path MTU instead of losing the packets.
- The rule fires on more than 50% of path MTU probes lost over 10m, or, with
  `sustainedThreshold` below 1, on more than 10% over 30m with at least two
  losses and one in the last 10m: that second arm is a black hole on one of
  several ECMP next hops, which fails only the probes hashed onto it.

## ZonePathMTUBlackHole

The zone-pair variant of PathMTUBlackHole. It fires only where Prometheus
holds no per-pair path MTU series for the zone pair (as under
`agent.metrics.detail=zone-only`); otherwise PathMTUBlackHole names the node
pairs and this rule stays quiet.

- The ratio is probe-weighted across every pair between the zones, so one
  black-holed pair among many is diluted here. The console Matrix and
  Investigate pages show which node pairs fail.
- Then follow the PathMTUBlackHole checks on those nodes.

## NodeUnreachable

Most of the peers that probe a node fail TCP to it, while the node's own agent
is still registered with the controller.

- Look at the node, not the pairs: its CNI agent, the host firewall, kubelet,
  a NetworkPolicy that isolates it.
- A node that stops altogether is not this alert: its agent leaves the mesh
  within the agent TTL and it pages as KconmonAgentsMissing.
- The [inhibit rules](../metrics.md#one-alert-per-node-instead-of-one-per-pair)
  let this alert stand for the pair alerts it explains.

## NodeIsolated

One node fails TCP to most of the peers it probes: its own egress is broken.

- Check that node's CNI agent, its routes and the network policy on its agent
  pod, not the peers.
- The same [inhibit rules](../metrics.md#one-alert-per-node-instead-of-one-per-pair)
  mute the pair alerts this one explains.

## PairWentSilent

A pair that reported probe results within the last hour has reported nothing
for 5m plus `for`. No failure ratio can be computed for it, so the other
rules have gone quiet about the pair rather than healthy.

- Check the agent pod on the source node and its Prometheus scrape target,
  then the controller's peer list.
- A node that was drained or removed produces this too; the alert clears on
  its own an hour after the last result. Pairs that a sparse topology plan
  dropped do not fire.

## DNSChecksFailing

Lookups of one name through one resolver fail from a node above the
threshold.

- This is resolver-side, not a peer link: check CoreDNS or kube-dns and the
  node's `resolv.conf` before the network.
- Against an explicit resolver a relative name fails; use a fully qualified
  name in `checkers.dns.hosts`.

## ExternalChecksFailing

Probes from a node to an external target fail above the threshold.

- A probe the allowlist refused never reaches this counter. If the target
  looks untested rather than failing, read `external_denied_total` for its
  reason (`cidr`, `resolve` or `disabled`) before suspecting the network.

## ZoneChecksFailing

More than the threshold of all TCP, UDP and ICMP probes from one zone to
another failed over the last 5m. This zone-level aggregate keeps firing when
`agent.metrics.detail` drops the per-pair series.

- The "kconmon-ng / Zone Heatmap" dashboard shows which protocol carries the
  failures and whether one direction or both are affected.
- The console Matrix or Investigate page finds the node pairs pulling the
  ratio up.

## ZoneLossHigh

UDP and ICMP probes between two zones lose more than the threshold of their
packets.

- The ratio is packet-weighted across every pair between the zones, so one
  broken link weighs about 1/N of it with N node pairs; between small zones a
  single link crosses the threshold alone, and UDPLossHigh names it.
- Use the Zone Heatmap dashboard and the console Matrix for the pairs.

## KconmonAgentsMissing

The leading controller expects one agent per schedulable node, and some have
not registered for `for`. Every pair involving a missing node stops being
probed, so the other rules go quiet about it rather than firing.

- Check the agent DaemonSet: pods that cannot schedule (taints, resources),
  crash-looping pods, gRPC from the agents to the controller being blocked.
- Agents that joined through the external gateway are not counted against the
  node total.

## KconmonControllerDown

No controller replica reports itself leader, or Prometheus scrapes none of
them. Peer lists stop being distributed and agents keep probing a frozen
topology.

- Check the controller Deployment, its lease in the release namespace and the
  controller scrape target in Prometheus.

## KconmonExternalAgentDown

Prometheus discovered an external agent through the controller's service
discovery but cannot scrape it. The agent still registers with the gateway,
otherwise the target would have left the list, so the host is up.

- The usual cause is the host firewall or the monitoring namespace's egress
  policy blocking the metrics port. On most CNIs Prometheus egress is NATed
  to a node IP, so the host must admit the cluster's node CIDR rather than
  the Prometheus pod IP.
