# Catch an MTU black hole

The failure this page reproduces is the one small probes cannot see. The TCP
handshake crosses, pings cross, the UDP loss probe's 4-byte datagrams cross,
and every one of those planes stays green. Full-size packets on the same pair
vanish, and so does every large transfer: an image pull hangs, a database
replica stops at the first big row, gRPC streams stall after the headers.

The usual causes are an encapsulation that eats the headroom (VXLAN and
Geneve take 50 bytes, WireGuard 60 to 80), an underlay with a smaller MTU than
the pods were told (a Hetzner vSwitch is 1400), and a firewall that drops the
ICMP "fragmentation needed" message path MTU discovery depends on.

## What kconmon-ng measures

Every agent sends each peer, once a minute, a small datagram and a full-size
one with the Don't Fragment bit set, to the peer's UDP echo port. The full size
is the MTU of the interface the agent reaches that peer through: in a pod it is
what the CNI configured, under `agent.hostNetwork` it is the node's NIC.

| What happens to the full-size datagram | Verdict | What it means |
| --- | --- | --- |
| echoed back | `ok` | full-size traffic crosses |
| refused with ICMP frag-needed | `reduced` | the path is smaller, and says so; TCP adapts, UDP without its own discovery does not |
| lost while the small one crosses | `blackhole` | the path is smaller and silent; large transfers stall |
| the small one is lost too | none | a connectivity problem, the UDP plane reports it |

On a black hole the agent bisects the size and reports the largest datagram
that still crosses: `kconmon_ng_pmtu_bytes` for the pair, next to
`kconmon_ng_agent_pmtu_probe_bytes`, the size it probed at. Before it calls
the pair a black hole it sends the full size once more, which must vanish
again, and the size it found once more, which must cross again, so random
loss on a congested path does not read as one.

## Reproduce it on kind

Any two nodes will do; this uses the e2e cluster layout.

```bash
kubectl get pods -l app.kubernetes.io/component=agent -o wide
```

Pick one agent pod as the source and note its node and IP, and the IP of an
agent on another node. On the source's node, drop every UDP datagram longer
than 1400 bytes to that peer:

```bash
docker exec <source-node> iptables -I FORWARD -s <source-ip> -d <peer-ip> \
  -p udp -m length --length 1401:65535 -j DROP
```

Within one probe interval (a minute by default):

- the console matrix, protocol **PMTU**, turns the pair red with `1400` as its
  figure and "black hole" under it, while the TCP matrix stays green;
- `kconmon_ng_pmtu_results_total{result="fail"}` grows for the pair and
  `kconmon_ng_pmtu_bytes` reads 1400;
- after 5 minutes over the 50% threshold, `PathMTUBlackHole` fires with the
  path MTU in its summary;
- `kubectl kconmon check <source-node> <peer-node> --type pmtu` prints
  `verdict=blackhole path_mtu=1400 probe_mtu=1500` and exits 2.

Remove the rule with the same command and `-D` in place of `-I`.

## When the black hole is by design

Some networks run below the interface MTU on purpose and rely on MSS clamping
to keep TCP working. There the probe is right (large UDP datagrams do not
cross) and the alert is noise for a TCP-only workload. Either probe at the
size the network really carries with `config.checkers.pmtu.size`, or turn the
rule off with `prometheusRule.pathMtuBlackHole.enabled: false`.
