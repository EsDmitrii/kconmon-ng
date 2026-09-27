# Roadmap

Direction, not dates. The issue tracker has the detail, and [RELEASE_NOTES.md](RELEASE_NOTES.md)
what has shipped.

## 2.6.0

- A Service plane: probe peers through a ClusterIP and kube-proxy, not only pod to pod.
- A CoreDNS plane: resolution from every node as a first-class plane.
- Webhook formats for Slack, Telegram and Alertmanager.

## 3.0.0

- One console for several clusters.

## Later

- A path MTU alert template in the console's rule builder; the chart's rules cover it today.
- Alertmanager silences created from maintenance windows, so a window also quiets alerts that do
  not go through console webhooks.
- DCO sign-off on every commit and a move to a dedicated GitHub organisation, once the project is
  accepted into the CNCF Sandbox.
- An absence-based rule for a node that stops altogether: today it leaves the mesh and pages as
  `KconmonAgentsMissing`, not as `NodeUnreachable`.

## Not planned

- ICMP and MTR from Windows hosts (see the FAQ for why).
- A certificate-signing flow or SPIFFE identities for external agents.
- apt and yum repositories: GitHub Releases carry the deb and rpm packages.
- Recording rules as the way to scale: they make queries cheaper but do not reduce what Prometheus
  scrapes. `topology.mode: sparse` and `agent.metrics.detail` cut series at the source.
- Aggregating results in the controller: results never pass through it, and routing them there
  would make the leader a throughput bottleneck.
- Zone loss from averaged per-pair ratios: an average of ratios misweights pairs, so the zone rules
  use packet counters.
