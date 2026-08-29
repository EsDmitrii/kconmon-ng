# External agents: scope and status

kconmon-ng measures Kubernetes node-to-node connectivity. Running the agent on
hosts **outside** the cluster — bare-metal machines, VMs in another network,
the far end of a VPN — is the most-requested extension. This page states
exactly what holds today, what blocks it, and what is planned, so nobody has
to reverse-engineer the answer from the source.

## What already holds

- **The agent binary has no Kubernetes dependency.** It never talks to the API
  server and does not link client-go; only the controller watches nodes.
- **Identity is plain environment.** `KCONMON_NG_NODE_NAME`,
  `KCONMON_NG_POD_NAME` (falls back to the hostname), `KCONMON_NG_POD_IP` and
  `KCONMON_NG_ZONE` are read at startup (`internal/agent/agent.go`), and
  nothing requires the values to describe a Pod. The zone comes straight from
  that variable — the node-label lookup happens on the controller, and only
  for in-cluster nodes.
- **Registration validates an address, not a Pod.** The controller requires
  the advertised address to be an IP literal (`validateAgentMeta` in
  `internal/controller/grpc_server.go`) and otherwise does not care where the
  agent runs.

So mechanically, a static binary with four environment variables pointed at
the controller's gRPC port registers and probes. Do not run that across a
trust boundary — read on.

## What blocks it

- **The gRPC channel is plaintext and unauthenticated.** The agent dials with
  insecure transport credentials (`internal/agent/grpc_client.go`) and the
  controller trusts whatever registration metadata arrives. Inside the cluster
  this is a deliberate, bounded trade-off: the port is never exposed, and the
  optional NetworkPolicy pins it further. **Exposing that port outside the
  cluster hands the probe mesh to anyone who can reach it** — they can
  register fake agents, receive the full peer list, and steer the fleet's
  probes.
- **No host packaging.** There is no deb/rpm, no systemd unit, no sysctl
  drop-in for the unprivileged ICMP socket (`net.ipv4.ping_group_range`);
  container images are the only supported delivery.
- **Address detection is manual.** In-cluster the Downward API fills
  `KCONMON_NG_POD_IP`; on a bare host you would have to set it by hand.
- **Timing defaults assume a LAN.** Agents heartbeat every 5 seconds against a
  controller-side TTL sweep tuned for in-cluster latency; a WAN fleet needs
  more forgiving values than the defaults, or flapping links turn into
  register/evict churn.

## Planned

Tracked as the external-agents milestone on the project roadmap:

1. First-class host identity: `nodeName` / `advertiseAddress` / `zone` in the
   agent config with sane autodetection, and an explicit "external" marker on
   such agents.
2. A **separate** TLS gateway port on the controller with authenticated
   registration (bootstrap token, optional client-certificate pinning). The
   in-cluster plaintext port stays untouched and unexposed.
3. deb/rpm packages with a hardened systemd unit and the ICMP sysctl drop-in.

For network planning, the firewall shape an external agent will need:
agent ↔ agent — TCP on the probe HTTP port, UDP on the probe echo port, and
ICMP, both directions; agent → controller — TCP on the gateway port;
Prometheus → agent — TCP on the metrics port.

## What to use meanwhile

The supported answer for off-cluster reachability today is the reverse
direction: **external checks** (`config.checkers.external`) let in-cluster
agents continuously probe named external hosts and URLs, gated by an
agent-side CIDR allowlist (`allowedCidrs` / `deniedCidrs`). See the External
sections of [metrics.md](metrics.md) and [configuration.md](configuration.md).
