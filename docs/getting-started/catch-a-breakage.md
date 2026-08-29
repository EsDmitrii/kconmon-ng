# Catch a breakage

Fifteen more minutes: break one protocol on one node pair, on purpose, and
watch kconmon-ng name the pair, the protocol and the hop. This page is the
short version of the full demo,
[Breaking the network on purpose](../demo/breaking-cni.md), which also breaks
TCP, ICMP and HTTP in three other places at once and then declares an alert
rule and a webhook for the failure — go there for the complete script.

## The scenario

A disposable 3-node Minikube stand from the repo's helper:

```bash
./hack/local-test.sh up
```

That brings up the cluster (`kconmon-test`), kube-prometheus-stack in
`monitoring`, and kconmon-ng — Console included — in `default`. Reach the
Console:

```bash
kubectl port-forward svc/kconmon-ng-console 8081:8080
```

We will blackhole **only UDP** from node `m02` to node `m03`, leaving every
other protocol and every other pair untouched. That is what a firewall typo, a
full conntrack table or an overlay offload bug actually looks like in
production: one protocol, one direction, one pair — and every HTTP health
check in the cluster still green.

## Break a node pair

Agents probe each other pod-IP to pod-IP, and **UDP probes target the agent's
gRPC/probe port 9090**. Find the agent pod IPs first:

```bash
kubectl get pods -l app.kubernetes.io/component=agent -o wide
```

Cross-node pod traffic transits the FORWARD chain on the destination node, so
the drop rule goes on `m03` (substitute the two pod IPs from your topology —
the source agent's on `m02` and the destination agent's on `m03`):

```bash
minikube -p kconmon-test ssh -n kconmon-test-m03 -- \
  'sudo iptables -I FORWARD 1 -p udp -s 10.244.1.10 -d 10.244.2.15 --dport 9090 \
   -j DROP -m comment --comment "kconmon-demo-udp-blackhole"'
```

Confirm the rule is matching packets — the counter climbs within seconds:

```bash
minikube -p kconmon-test ssh -n kconmon-test-m03 -- \
  'sudo iptables -L FORWARD 1 -v -n --line-numbers | head'
```

## Watch it surface in the console

Open **Matrix**, protocol **UDP**. Within one scrape cycle plus a probe
interval (~15–20s), five cells stay green and the `m02 → m03` cell turns red.
Switch the protocol selector to TCP or ICMP and the same cell is green: the
Console is showing you protocol isolation, not just a red square.

The same fact in PromQL, if you prefer proof over pictures:

```promql
# exactly one pair, total loss
kconmon_ng_udp_packet_loss_ratio > 0

# the same pair, healthy on ICMP
kconmon_ng_icmp_packet_loss_ratio{source_node="kconmon-test-m02",destination_node="kconmon-test-m03"}
```

Clicking the red cell opens the **pair card**: the pair's loss and RTT charts,
its recent MTR path history, and a rail of the topology and diagnostic events
around it. The bundled `UDPLossHigh` alert goes *pending* within about half a
minute and fires after its 5-minute hold.

## Read the MTR trace

You did not have to ask for a trace. A failed TCP, UDP or ICMP probe
auto-triggers an MTR trace for that pair, rate-limited by a per-pair cooldown
(60s by default):

```promql
kconmon_ng_mtr_triggered_total{source_node="kconmon-test-m02",destination_node="kconmon-test-m03"}
kconmon_ng_mtr_hop_rtt_seconds{source_node="kconmon-test-m02",destination_node="kconmon-test-m03"}
```

The hop series shows the path toward the destination pod IP, captured the
moment it broke — not after you SSH in to investigate. In the Console the same
trace is on the pair card and in [Routes (MTR)](../console/routes-mtr.md),
where every traced path is content-hashed and deduped, so path history is a
list of *changes*, not a wall of identical traces.

## Clean up

Remove the rule and watch recovery — stale gauges are reset every cycle, so
the cell is green again within ~one scrape:

```bash
minikube -p kconmon-test ssh -n kconmon-test-m03 -- \
  'sudo iptables -D FORWARD -p udp -s 10.244.1.10 -d 10.244.2.15 --dport 9090 \
   -j DROP -m comment --comment "kconmon-demo-udp-blackhole"'
```

Verify no demo rules remain (a leftover DROP quietly breaks your next run),
then tear the stand down when you are done:

```bash
for n in kconmon-test kconmon-test-m02 kconmon-test-m03; do
  echo "$n:"; minikube -p kconmon-test ssh -n "$n" -- 'sudo iptables -S | grep kconmon-demo || echo "  clean"'
done
```

```bash
./hack/local-test.sh down
```

**Keep going:** the [full demo](../demo/breaking-cni.md) runs four
protocol-specific breaks at once, correlates one on the Incidents page, saves
it as an incident, and wires an alert rule with a signed webhook to the same
failure.
