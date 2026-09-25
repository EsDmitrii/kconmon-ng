# Roadmap

Direction, not dates. The issue tracker has the detail.

## 2.5.0

- Path MTU probe: full-size datagrams with DF set, bisection on loss, a `PathMTUBlackHole` alert and
  a PMTU protocol in the console matrix.
- Node-level alerts (`NodeUnreachable`, `NodeIsolated`) so a node its peers cannot reach pages once, not once per pair.
- Maintenance windows hold console alert webhooks.
- Local user management in the console.
- Fault-injection end-to-end tests: a broken pair must trigger a reactive MTR, a black hole must be
  named.

## 2.6.0

- A Service plane: probe peers through a ClusterIP and kube-proxy, not only pod to pod.
- A CoreDNS plane: resolution from every node as a first-class plane.
- Webhook formats for Slack, Telegram and Alertmanager.

## 3.0.0

- One console for several clusters.

## Not planned

- ICMP and MTR from Windows hosts (see the FAQ for why).
- A certificate-signing flow or SPIFFE identities for external agents.
- apt and yum repositories: GitHub Releases carry the deb and rpm packages.
