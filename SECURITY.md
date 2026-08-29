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

## Known design boundaries

The following are documented design decisions, not vulnerabilities — unless
the chart's **defaults** cause the exposure, in which case that absolutely is
a report we want:

- The controller's gRPC registration API is plaintext and unauthenticated and
  must stay unreachable from outside the cluster. The chart never exposes it;
  authenticated registration for external agents is planned separately — see
  [docs/external-agents.md](docs/external-agents.md).
- The controller's HTTP API authenticates nothing and shares the workload's
  `httpPort`. This is why `/metrics` gets a listener of its own
  (`config.metricsPort`) and why the optional NetworkPolicy opens only the
  metrics port to Prometheus.
- Agents probe whatever peer list the controller hands them, plus external
  targets gated by the agent-side CIDR allowlist
  (`config.checkers.external.allowedCidrs` / `deniedCidrs`), which is enforced
  on the agent rather than in the Console precisely so a compromised Console
  cannot widen it.
