# Security Policy

## Reporting a vulnerability

Report vulnerabilities privately through
[GitHub private vulnerability reporting](https://github.com/EsDmitrii/kconmon-ng/security/advisories/new)
— please do not open a public issue. Include the versions involved (chart and
image), the deployment shape (the values that matter for the finding), and
reproduction steps.

Expect an acknowledgement within a few days. This is a solo-maintained
project: confirmed issues are fixed in the next release rather than on an SLA,
and severe ones get a release of their own.

## Supported versions

Only the latest released version receives security fixes; there are no
backports to older minors.

| Version | Supported |
| --- | --- |
| latest release | yes |
| anything older | no |

## Supported platforms

Linux only, for the in-cluster images and for the deb/rpm host agent alike
(amd64 and arm64). Windows is not supported: ICMP and MTR depend on the
datagram ICMP socket that exists on Linux, and the raw-socket alternative on
Windows would run the agent as an administrator; the
[FAQ](docs/faq.md#does-the-agent-run-on-windows) has the full reasoning.

## Known design boundaries

The following are documented design decisions, not vulnerabilities — unless
the chart's **defaults** cause the exposure, in which case that absolutely is
a report we want:

- The controller's in-cluster gRPC registration API is plaintext and
  unauthenticated and must stay unreachable from outside the cluster. The
  chart never exposes it. External agents use the shipped path instead (since
  v2.3.0): a separate TLS gateway with a bootstrap token and optional
  client-certificate pinning, described in
  [docs/external-agents.md](docs/external-agents.md). In token-only mode any
  token holder is a fleet member and can register under any name, including
  a fictitious external agent with an address of its choosing; pin client
  certificates when token holders are not all equally trusted.
- The Prometheus HTTP SD endpoint (`/api/v1/prometheus/sd`, served on the
  metrics listener as well as the API port) authenticates nothing, like the
  rest of the controller API. Its body carries a fixed label set that no agent
  can extend, but that set discloses every external host's address, node name
  and zone to whoever reaches `metricsPort`. Narrow the scraper with
  `networkPolicy.prometheusPodLabels`, or opt out with
  `controller.prometheusSD.enabled=false`.
- `agent.hostNetwork` puts the agent's read-only HTTP surface (`/metrics`,
  `/healthz`, `/readyz`, `/api/v1/version`) and its UDP echo on the node's own
  IP, where no NetworkPolicy applies. The host firewall is the boundary there.
- The controller's HTTP API authenticates nothing and shares the workload's
  `httpPort`. This is why `/metrics` gets a listener of its own
  (`config.metricsPort`) and why the optional NetworkPolicy opens only the
  metrics port to Prometheus.
- Agents probe whatever peer list the controller hands them, plus external
  targets gated by the agent-side CIDR allowlist
  (`config.checkers.external.allowedCidrs` / `deniedCidrs`), which is enforced
  on the agent rather than in the Console precisely so a compromised Console
  cannot widen it.
