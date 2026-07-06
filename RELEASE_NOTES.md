## kconmon-ng v1.3.3

> Chart-focused release. The Go agent/controller code is unchanged from v1.3.2;
> the `:1.3.3` images are a version-synchronized rebuild (the release tag drives
> both the chart version and the image tag).

### Fixes

- **ICMP checker on runtimes with a closed `net.ipv4.ping_group_range`** — the ICMP
  checker opens an unprivileged ICMP "ping" socket (`SOCK_DGRAM`), which the kernel
  gates on `net.ipv4.ping_group_range`, not on `NET_RAW`. Some container runtimes
  leave this at the closed kernel default (`1 0`), so the checker failed with
  `socket: permission denied` on those nodes. The agent Pod now sets the safe,
  namespaced sysctl `net.ipv4.ping_group_range=0 2147483647`, so ping sockets work
  regardless of the runtime default.

### Chart

- New `agent.podSecurityContext` value exposes the agent Pod-level `securityContext`
  (defaults to opening `ping_group_range` for the ICMP checker). Set
  `agent.podSecurityContext: {}` to opt out. Documented in the chart README and
  `values.schema.json`.

### Install

```bash
helm upgrade --install kconmon-ng oci://ghcr.io/esdmitrii/charts/kconmon-ng \
  --version 1.3.3 \
  --namespace kconmon-ng \
  --create-namespace
```

kubectl plugin (via krew, from the release manifest):

```bash
kubectl krew install --manifest-url \
  https://github.com/EsDmitrii/kconmon-ng/releases/download/v1.3.3/kconmon.yaml
```

### Images

```
ghcr.io/esdmitrii/kconmon-ng-agent:1.3.3
ghcr.io/esdmitrii/kconmon-ng-controller:1.3.3
```

---

## kconmon-ng v1.3.2

> Note: this is the first fully working release of the on-demand diagnostics
> feature set. v1.3.0 was aborted mid-release (GitHub immutable releases sealed
> it before all assets were attached); v1.3.1 published but shipped a krew
> manifest with an invalid version string, so `kubectl krew install
> --manifest-url` rejected it. v1.3.2 carries the same content with a valid
> krew manifest. The v1.3.0/v1.3.1 tags are retired.

### Features

- **`kubectl-kconmon` plugin (on-demand diagnostics)** — a new kubectl plugin talks to the
  controller's HTTP API through a client-go port-forward, so operators can inspect topology
  (`kubectl kconmon topology` / `agents`) and run one-shot connectivity checks
  (`kubectl kconmon check SRC DST --type …`, `kubectl kconmon mtr SRC DST`) between any two nodes
  without opening Grafana. Table or `-o json` output; a failed check exits `2` (distinct from `1`
  for CLI/API errors) so it composes in shell pipelines. Install via krew from the release manifest
  (see Install below).

- **On-demand diagnostics API** — new `POST /api/v1/diagnostics` controller endpoint runs a single
  check (`tcp`/`udp`/`icmp`/`dns`/`http`/`mtr`) from a source node's agent to a destination and
  returns the `CheckResult` verbatim. Served by the leader only; `?timeout=` caps the wait
  (default 60s, max 120s). This is the endpoint the plugin drives. See `docs/api.md`.

- **Graceful agent deregistration on SIGTERM** — a restarting agent now deregisters from the
  controller on shutdown, so peers drop it immediately instead of waiting out the heartbeat TTL.
  This removes the transient false-loss window that a rolling agent restart used to leave in its
  own metrics.

### Security

- **Toolchain and dependency bumps** — Go toolchain `go1.26.4`; `google.golang.org/grpc`
  1.79.1 → 1.82.0, `golang.org/x/net` 0.51 → 0.56, `golang.org/x/sys` 0.41 → 0.46, and OpenTelemetry
  1.41 → 1.44. This clears the CVE findings behind the previous Artifact Hub security-report grade.
- **`govulncheck` in CI** — a dedicated CI job runs `govulncheck ./...` on every PR and tag;
  Dependabot (gomod / github-actions / docker, weekly) keeps dependencies current so CVE fixes land
  as normal PRs instead of accumulating until the next scan.

### Supply chain

- The Helm chart is now signed with cosign (keyless, by digest) — v1.3.2 is the first signed
  release. Artifact Hub repository metadata continues to be published as an ORAS artifact.

### Docs

- README reworked with an "On-demand diagnostics (kubectl plugin)" section and real command output.
- `docs/api.md` documents the full `POST /api/v1/diagnostics` contract (request fields, status
  codes, `?timeout=` cap, and ICMP / MTR response examples).

### Install

```bash
helm upgrade --install kconmon-ng oci://ghcr.io/esdmitrii/charts/kconmon-ng \
  --version 1.3.2 \
  --namespace kconmon-ng \
  --create-namespace
```

kubectl plugin (via krew, from the release manifest):

```bash
kubectl krew install --manifest-url \
  https://github.com/EsDmitrii/kconmon-ng/releases/download/v1.3.2/kconmon.yaml
```

### Images

```
ghcr.io/esdmitrii/kconmon-ng-agent:1.3.2
ghcr.io/esdmitrii/kconmon-ng-controller:1.3.2
```

---

## kconmon-ng v1.2.0

### Features

- **Automatic zone discovery** — agents no longer need a statically configured zone. The
  controller enriches each agent registration with the node's failure-domain zone taken from its
  node informer (`failureDomainLabel`, default `topology.kubernetes.io/zone`) and returns the
  resolved metadata in `RegisterResponse.agent`; the agent adopts it for all `source_zone` /
  `destination_zone` metric labels. `KCONMON_NG_ZONE` (`agent.zone` in Helm values) remains an
  explicit override and always wins. Node zone relabels propagate to peers via a FULL_SYNC peer
  update; the relabeled node's own `source_zone` refreshes on its next re-registration.
  Per-zone metrics and the Zone Heatmap dashboard now work out of the box on multi-zone clusters.

- **Self-monitoring** — new gauge `kconmon_ng_controller_expected_agents` (count of schedulable
  nodes from the controller's node informer) and two PrometheusRule alerts:
  `KconmonAgentsMissing` (warning: registered < expected for 10m) and `KconmonControllerDown`
  (critical: `absent(kconmon_ng_controller_leader == 1)` for 5m). Degradation of kconmon-ng
  itself now alerts instead of failing silently. Requires `controller.leaderElection: true`
  (default) for the node informer.

### Breaking-ish Changes

- **Strict config parsing** — the application config (ConfigMap / `--config` file) is now decoded
  with unknown-field rejection and per-checker semantic validation (intervals/timeouts > 0 for
  enabled checkers, HTTP target URL scheme/host, DNS resolver host[:port], non-empty DNS hosts).
  A typo'd or invalid config now fails startup and is rejected on hot-reload (the previous config
  stays active) instead of being silently ignored. Review your values overrides before upgrading:
  a config that previously "worked" by accident will now fail loudly. `timeout >= interval` logs
  a warning but does not fail.

### Helm Chart / Artifact Hub

- Chart README is now packaged into the chart archive — the Artifact Hub package page renders
  description, install instructions, values and metrics reference instead of
  "This package version does not provide a README file".
- `home` and `sources` added to `Chart.yaml`; Artifact Hub repository metadata
  (`artifacthub-repo.yml`) is published as an ORAS artifact on release for repository
  verification.
- `agent.zone` is now documented as an optional override (auto-discovery is the default).

### Dashboards

- **Overview / MTR Triggers Count** — switched from `increase(...[$__range])` to a plain
  `sum(...)`: `increase()` misses counter births on freshly restarted agent pods and chronically
  undercounted exactly when MTR fires most (pod churn).

### Local Development

- `hack/local-test.sh` hardening: unique image tag per build (minikube's image-load cache
  silently kept stale same-tag images on re-runs), `set -e`/`pipefail` fixes (`((ok++))`
  pre-increment exit, SIGPIPE on `head`-truncated pipes), port-forward cleanup.

### Upgrade Notes

1. Validate your config overrides against the stricter parser before rolling out (a quick check:
   `helm template ... | <render your config>` and run the controller/agent with `--config` locally,
   or just watch pod readiness on a staging cluster first).
2. If you previously set `agent.zone` to force a zone, you can keep it (it still wins) or drop it
   to switch to automatic discovery.
3. Metric label sets are unchanged; the new alerts ship in the chart's default
   `prometheusRule.rules` and are inert unless `prometheusRule.enabled: true`.

### Install

```bash
helm upgrade --install kconmon-ng oci://ghcr.io/esdmitrii/charts/kconmon-ng \
  --version 1.2.0 \
  --namespace kconmon-ng \
  --create-namespace
```

### Images

```
ghcr.io/esdmitrii/kconmon-ng-agent:1.2.0
ghcr.io/esdmitrii/kconmon-ng-controller:1.2.0
```

---

## kconmon-ng v1.1.0

### Bug Fixes

- **MTR memory leak** — `lastRun` map in `MTRChecker` could grow unboundedly in long-running agents
  on large clusters where node pairs come and go. Expired entries are now purged inline on each
  `TryAcquire` call while the lock is already held, keeping the map size proportional to active
  pairs within the current cooldown window.

- **HTTP body pattern mismatch counted as success** — when a `bodyPattern` check failed, the
  checker set `StatusCode = -1`, which was not caught by the result handler's `>= 400` guard and
  was silently recorded as `result="success"` in Prometheus. The status code field now always
  carries the real HTTP status. A dedicated `BodyMismatch bool` field signals pattern failure, and
  the result handler correctly marks such checks as `result="fail"`.

### Improvements

- **Configurable DNS resolver dial timeout** — the dialer timeout for custom DNS resolvers was
  previously hard-coded to 5 seconds and could not be adjusted for slow or distant resolvers.
  A new `timeout` field has been added to the DNS checker config (default: `5s`). Update your
  Helm values or config file to override:
  ```yaml
  checkers:
    dns:
      timeout: 3s
  ```

- **Jitter in agent re-registration backoff** — when the controller restarts, all agents
  previously retried at exactly the same interval, causing a thundering herd. Up to 25% random
  jitter is now added to each retry wait, spreading reconnect load across agents.

- **MTR buffer allocation** — the 1500-byte read buffer in the traceroute loop was allocated
  once per hop. It is now allocated once per trace, reducing GC pressure under frequent MTR runs.

### Helm Chart

- `config.checkers.dns.timeout` added to `values.yaml` (default: `5s`).

### Tests

- Updated `TestHTTPCheckerBodyPatternMismatch`: verifies `BodyMismatch=true` and real HTTP status
  code instead of the former `-1` sentinel.
- Added `TestHTTPCheckerBodyPatternMatch`: verifies `BodyMismatch=false` on a successful pattern.
- Added `TestDNSCheckerTimeoutPropagated`: verifies the configured timeout is stored on the checker.
- Added `TestMTRCheckerExpiredEntriesPurged`: verifies stale entries are removed from `lastRun`
  after cooldown expiry.

### Upgrade Notes

The `HTTPDetails.StatusCode` field no longer returns `-1` for body pattern mismatches — it now
always holds the actual HTTP response status code. If you have alerting or dashboards that rely
on `statusCode == -1` to detect body mismatch failures, update them to use the new
`bodyMismatch` field in the JSON result or the `result="fail"` label in Prometheus metrics.

### Install

```bash
helm upgrade --install kconmon-ng oci://ghcr.io/esdmitrii/charts/kconmon-ng \
  --version 1.1.0 \
  --namespace kconmon-ng \
  --create-namespace
```

### Images

```
ghcr.io/esdmitrii/kconmon-ng-agent:1.1.0
ghcr.io/esdmitrii/kconmon-ng-controller:1.1.0
```

---

## kconmon-ng v1.0.0 — Initial Release

Kubernetes Node Connectivity Monitor, next-generation rewrite with a gRPC-based agent/controller architecture and rich observability out of the box.

### Features

**Core**
- Agent/controller architecture with gRPC streaming peer updates
- TCP, UDP, ICMP, DNS, and HTTP checkers with configurable timeouts and thresholds
- Per-node and per-zone Prometheus metrics for all check types
- Reactive MTR traceroute on check failure with per-pair cooldown
- Self-probe prevention: peers filtered by agent ID, node name, and pod IP
- Atomic gauge reset on peer topology changes to prevent stale metrics

**Scheduler**
- Pause/resume support, per-check jitter, and NodeLocal checker mode
- NodeWatcher: live Kubernetes node info exposed via `/api/v1/topology`

**Observability**
- Grafana dashboards: Overview, Node Detail, Cross-Zone Heatmap
- Helm chart with ServiceMonitor, PrometheusRule, NetworkPolicy, PDB, and RBAC

**Operations**
- Multi-arch Docker images (linux/amd64, linux/arm64) published to GHCR
- Local dev tooling: `hack/local-test.sh` with Minikube + Prometheus + Grafana stack
- Chaos testing guide with NetworkPolicy example

### Install

```bash
helm install kconmon-ng oci://ghcr.io/esdmitrii/charts/kconmon-ng \
  --version 1.0.0 \
  --namespace kconmon-ng \
  --create-namespace
```

### Images

```
ghcr.io/esdmitrii/kconmon-ng-agent:1.0.0
ghcr.io/esdmitrii/kconmon-ng-controller:1.0.0
```
