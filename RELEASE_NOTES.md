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
