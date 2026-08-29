# Diagnose a slow pair

## Symptom

An application team reports that traffic between two services is slow —
sometimes, for some replicas. Classic partial failure: node-level health is
green, `kubectl get nodes` says `Ready`, and the only hard fact is a latency
graph that looks worse than last week. The question kconmon-ng can answer:
**is the network between specific nodes actually slower, on which protocol,
and at which hop?**

## Localize with the Matrix

Open the Console's [Matrix](../console/matrix.md) and flip through the
protocols. You are looking for structure, not a single number:

- **One cell** off-colour → one directed pair. Check the mirror cell: if
  `A → B` is bad and `B → A` is clean, the problem is directional — think
  asymmetric routing or a policy on the return path.
- **A row or column** → one node, outbound or inbound respectively.
- **A block** aligned with zones → a zone boundary; confirm on the zone view.

Click the suspect cell to open the [pair page](../console/pair-and-node-pages.md):
loss and RTT charts for that pair, recent MTR path history, and the events
around it. If the fleet is large, the [Overview](../console/overview.md)
page's worst-pairs table is the shortcut to the same cell.

## Confirm with Metrics

The [Metrics](../console/metrics.md) page (or your own Prometheus) turns
"looks slow" into numbers. RTT distributions are histograms, so ask for a
quantile — for the pair the Matrix pointed at:

```promql
# p99 TCP round-trip for one directed pair, over 10-minute windows
histogram_quantile(0.99, sum by (le) (
  rate(kconmon_ng_tcp_total_duration_seconds_bucket{
    source_node="node-a", destination_node="node-b"}[10m])))
```

Three checks worth doing while you are here:

- **Latency or loss?** `kconmon_ng_udp_packet_loss_ratio` and
  `kconmon_ng_icmp_packet_loss_ratio` for the same pair. Loss retries dressed
  up as latency is a different investigation than genuinely slow forwarding.
- **Jitter**: `kconmon_ng_udp_jitter_seconds` — a congested or rerouted path
  usually shows variance before it shows sustained slowness.
- **Compare against a healthy pair** in the same zone pairing, to separate
  "this link" from "everything in that direction".

Use A/B compare on the Metrics page for the last one, or add a second
selector in [PromQL](../console/promql.md).

## Find the hop with MTR

If loss is involved, traces already exist: a failed TCP, UDP or ICMP probe
auto-triggers an MTR trace for its pair (per-pair cooldown, 60s by default).
Open [Routes (MTR)](../console/routes-mtr.md) and pick the destination — path
history is deduplicated by content, so what you see is the list of *path
changes*, with a diff view between any two traces.

For a pair that is slow without failing, fire a trace yourself from
[Run checks](../console/run-checks.md) (check type `mtr`), or from a terminal:

```bash
kubectl kconmon mtr node-a node-b
```

Read the hop table for where RTT jumps or per-hop loss starts: everything
before that hop is fine, the problem lives at or after it. A path that
*changed* around the time the symptom started — an extra hop, a different
middle — is your correlation; the pair page's history shows when.

## Fix and verify

The fix is yours — a CNI daemon restart, an offload toggle, a rerouted
uplink, a revert of yesterday's firewall change. Verification is the same
loop backwards, and it is cheap:

1. Re-run the quantile query — p99 back at baseline.
2. The Matrix cell back in the healthy band, both directions.
3. A fresh MTR run showing the expected path again.

If this was an incident worth remembering, save it as one on the
[Incidents](../console/incidents.md) page: the permalink rehydrates the exact
scope and window, and the [Time Machine](../console/time-machine.md) can
replay the console at the minute it broke for the postmortem. If the pair
deserves a tighter watch than the bundled rules give it, declare a scoped
rule for it — see [Set up alerting](set-up-alerting.md).
