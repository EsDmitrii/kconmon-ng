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
  [docs/external-agents.md](docs/external-agents.md). The gateway serves
  agent registration only; the event stream stays on the in-cluster port. In
  token-only mode any token holder is a fleet member and can register under
  any name, including a fictitious external agent with an address of its
  choosing; pin client certificates when token holders are not all equally
  trusted. A pinned certificate binds both the node name and the agent ID.
  Anyone who reaches the gateway can open connections before presenting a
  token, so the gateway bounds them: the TLS handshake gets 10 seconds, a
  connection whose RPCs have not passed the token check within 30 seconds is
  closed, at most 128 connections may be unauthenticated at once, and a
  connection with no RPC in flight for 2 minutes is closed. Past the cap the
  new connection is admitted and an unauthenticated one of the source
  holding the most (an IPv4 address or an IPv6 /64, the new connection
  counted) is closed: its oldest still in the TLS handshake, else its oldest
  past TLS. A flooding source pushes out its own connections first, and bare
  TCP sockets push out each other rather than an agent. None of these is
  configurable. Behind `externalTrafficPolicy: Cluster` or a NodePort every
  source is a node IP, and a client can still push an agent out by opening
  more than 128 connections from the agent's source while the agent is in
  its TLS handshake, or by completing more than 128 TLS handshakes before
  the agent's first RPC; the agent retries. `loadBalancerSourceRanges`, or
  `externalTrafficPolicy: Local` with tight source CIDRs, narrows this.
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
  The controller then has to admit node addresses on its gRPC port
  (`networkPolicy.nodeCidrs`, or on Cilium the `remote-node` and `host`
  entities through `<fullname>-node-ingress`), so every process on a node's
  host network can reach that unauthenticated API.
- The controller's HTTP API authenticates nothing and shares the workload's
  `httpPort`. This is why `/metrics` gets a listener of its own
  (`config.metricsPort`) and why the optional NetworkPolicy opens only the
  metrics port to Prometheus. The routes that read a body cap it
  (`POST /api/v1/diagnostics` at 64 KiB, `PUT /api/v1/external-checks` at
  8 MiB) and answer 413 past that.
- Agents probe whatever peer list the controller hands them, plus external
  targets gated by the agent-side CIDR allowlist
  (`config.checkers.external.allowedCidrs` / `deniedCidrs`), which is enforced
  on the agent rather than in the Console precisely so a compromised Console
  cannot widen it.
- The agent's UDP echo answers source ports from 1024 up, since a probe
  comes from an unprivileged port. It refuses the advertised address and
  echo port of every registered agent, which the controller sends to each
  agent (with a controller older than 2.5.0, only the planned peers'), and
  its own echo port from its own address or loopback. It does not refuse a
  port by number alone: a NAT on the path can give a probe any unprivileged
  source port, the echo port included. It does not answer datagrams sent to
  `255.255.255.255`, to a subnet broadcast address of the host or to a
  multicast group, and it rate-limits replies per source address and port
  (a burst of 256, refilled at 64 a second), so a forged datagram cannot
  start an endless echo loop between two echo endpoints or reach every agent
  on a segment at once. A loop with a responder that is no registered agent
  ends at that limit. It replies from the address it was probed at.
- The console caps open `/ws` sockets per replica, per client address and per
  user or API token (`websocket.maxConnections`, `maxConnectionsPerAddress`,
  `maxConnectionsPerSubject`; Helm `console.websocket.*`; 1024, 256 and 32 by
  default, 0 turns a cap off). A refused socket is closed with 1013 and counted
  in `<prefix>_console_ws_refused_total{limit}`.
- The console's login limit (`console.rateLimit.loginPerMinute`, 5 by
  default) counts every attempt per username, so anyone who knows a username
  can keep that account locked out of password login with five wrong
  attempts a minute. Open sessions keep working, and the lockout ends a
  minute after the attempts stop. Where that matters, sign in through OIDC
  or header auth instead of local accounts, or limit the source at the
  ingress. The console's own per-source budget, for password login and for
  the OIDC start and callback alike, is twenty times the per-username one,
  and behind an ingress it counts the ingress pod unless
  the console config lists that pod in `clientAddress.trustedProxyCIDRs`
  (Helm `console.clientAddress.trustedProxyCIDRs`, rendered in every auth
  mode; while it is empty the console reads `auth.header.trustedProxyCIDRs`
  instead, as before). See
  [Configuration > Console](docs/configuration.md#console).
- `clientAddress.trustedProxyCIDRs` decides only whose `X-Forwarded-For`
  names the client, for the per-address rate limits, the `/ws` address cap
  and the audit log's address. It never authenticates anyone: header mode
  trusts identity headers only from `auth.header.trustedProxyCIDRs`, which
  should name the authenticating proxy and nothing wider. A pod inside the
  client-address list can still name any client address it likes. The
  anonymous requests it forwards also spend a budget of that pod's own,
  twenty times a single client's. Sign-in has no such shared budget, since
  anyone behind the ingress could spend it for every user: each account's
  per-username limit holds whatever address a request names, starting an
  OIDC sign-in stores nothing on the server (the PKCE verifier and return
  path travel in the `state`, sealed with a key derived from the client
  secret), and password checks run in a fixed number of slots. List the
  ingress controller's addresses, not the whole pod network, where you can.
  IPv6 clients are budgeted per /64.
- An API token holds exactly `auth.defaultRole`: role bindings name users and
  groups, never tokens. With the default (empty) a token is refused on every
  route that needs a permission; setting `auth.defaultRole` gives that role to
  every token and to every signed-in user without a binding.
- The audit log is written through a small buffer on each console replica.
  Rows of failed requests and of anonymous or credential-less callers use at
  most half of it, and one user, token or client address at most a quarter
  of it. On the RBAC, token, user, import, export and audit routes those rows
  have an eighth of their own instead, which no other row takes. Successful
  requests elsewhere use at most three quarters; the rest is kept for
  successful requests on those routes and for successful sign-ins (local and
  OIDC) and password changes, which also wait up to two seconds for room.
  Failed requests that carry no credential at all (a 401, a refused OIDC
  callback, a failed or rate-limited sign-in) write at most 120 rows a
  minute per client address, so the audit log does not record every denial
  of such a flood. A row that finds no room, or no budget, is dropped and
  counted in `<prefix>_console_audit_dropped_total`, which is worth an
  alert. Text PostgreSQL cannot store (invalid UTF-8, an unpaired UTF-16
  surrogate escape) is written as U+FFFD, and a number beyond the range of
  `numeric` as its literal string, so such a request cannot keep its own
  row out of the log.
- The console reads at most 8 KiB of a request body on the routes that need
  no credentials (sign-in, password change) and 16 MiB elsewhere.
- `GET /api/v1/export` needs `settings:write`, and each section of the bundle
  also needs the permission its own read route asks for: webhooks need
  `webhooks:manage`, because a webhook URL is often a credential itself.
  A section the caller may not read is left out and named under `omitted`.
- The console's alerting Role can list every PrometheusRule in its
  namespace, to offer the others for import. Get, create, patch and delete
  are limited to `console.alerting.bundleName`: the console writes with
  server-side apply, which names the object, so create can be scoped too.
  The console refuses to apply over or delete an object of that name that
  does not carry `app.kubernetes.io/managed-by: kconmon-ng-console`.
