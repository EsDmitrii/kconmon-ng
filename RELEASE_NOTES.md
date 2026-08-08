## kconmon-ng v1.7.0

> Console release. MTR Explorer with durable path history and optional hop
> enrichment, the Time Machine global time context, and chart annotations
> (M5). Everything is off by default or read-only-additive; a 1.6.0 install
> that upgrades and changes nothing renders the same manifests.

### Added

- **MTR path history** — every MTR result the Console records is projected
  into `mtr_path_snapshots`: hop lists content-hashed per (source,
  destination) route, deduplicated at ingest, with first/last-seen and trace
  counts. `kconmon_ng_console_mtr_snapshots_total{result="new-path"}` is the
  "route changed" alerting primitive.
- **MTR Explorer** — `/mtr` three panes (destinations → path history →
  trace detail), client-side path diff between any two snapshots, a "path
  changes" timeline overlaid with the pair's loss series, per-hop RTT
  trends, and a Runner tab (gated on `runs:create`).
- **Hop enrichment** — `console.mtr.enrichment.*` (off by default): reverse
  DNS and/or MaxMind GeoLite2 ASN/City mmdb files mounted read-only at
  `/geoip` from an operator-supplied volume; resolved server-side into a
  TTL cache (`mtr_hop_enrichment`), air-gap friendly, per-source
  degradation.
- **Time Machine** — a top-bar control and shareable `?at=` URL state:
  topology reconstructed from `topology_events` up to `t` (`GET
  /api/v1/topology?at=`), PromQL surfaces evaluated at `t`, the Live feed
  becomes scrollback, and every mutating control is disabled behind the
  banner while engaged. Note: today's controller does not yet attribute
  `topology_changed` events to nodes, so historical topology reconstruction
  reports honestly-empty results until it does (named deferral).
- **Annotations** — notes pinned to instants or time ranges
  (`/api/v1/annotations`), rendered as chart markers on Explore and the
  object cards and inline in the Live scrollback. `annotations:write` stops
  at operator/admin; reads are telemetry (every role).
- **Explore A/B** — a Compare panel: a second curated metric on the same
  axes, or the same metric time-shifted (1h/24h/7d) against itself.
- **Permissions** — `mtr:read`, `annotations:read` (all built-in roles) and
  `annotations:write` (operator/admin).

### Chart

- 1.6.0 → 1.7.0. New value blocks: `console.mtr.enrichment.*` (rendered
  into the console ConfigMap only when enabled; mmdb volume via an opaque
  `geoip.volume` VolumeSource passthrough with a render-time guard). An
  eighth ci profile (`console-mtr-values.yaml`). Default render is
  key-identical to 1.6.0.

## kconmon-ng v1.6.0

> Console release. External probe targets, saved check definitions and
> schedules, continuous external checks with agent-side CIDR enforcement,
> and diagnostics v2 (M4). Off by default throughout: without
> `console.scheduler.enabled`, `config.checkers.external.enabled` or a
> database, a 1.5.0 install behaves identically.

### Added

- **Targets, checks, schedules** — CRUD `/api/v1/{targets,checks,schedules}`
  (PostgreSQL-backed, 503 without a database), with a cardinality projection
  guard: an enabled definition may project at most 400 series, enforced
  server-side and mirrored live in the UI.
- **Console scheduler** — `console.scheduler.{enabled,tickInterval}` (off by
  default): `once`/`interval` schedules fire diagnostics runs from exactly
  one replica under a PostgreSQL advisory lock; a stuck-run reaper finishes
  abandoned runs as `cancelled`; `POST /api/v1/runs/{id}/cancel` cancels an
  in-flight run.
- **Continuous external checks** — check definitions of kind `continuous`
  are reconciled to the controller (`PUT /api/v1/external-checks`,
  `WatchExternalChecks` stream) and probed by agents on their own cadence.
  The **agent** is authoritative: `config.checkers.external.enabled` plus a
  mandatory `allowedCidrs` allowlist (deny-wins, resolved-IP matching,
  re-checked on every probe); an agent without the opt-in refuses external
  work and the controller answers 501 for it.
- **External metrics** — the `kconmon_ng_external_*` family
  (`{source_node,source_zone,target,target_kind}` labels; the target NAME,
  never an address) plus `ExternalChecksFailing` in the default rules.
- **Diagnostics v2** — `POST /api/v1/runs` accepts
  `destinationKind=node|target|adhoc`; the diagnostics form grows a
  destination selector and Save-as-definition.
- **Rate limits** — `console.rateLimit.{runsPerMinute,loginPerMinute}`
  (fixed-window on the shared KV; fail-open on a Valkey outage; the login
  limit runs before argon2id).
- **OpenAPI** — a committed spec (`docs/console-api.yaml`), generated TS
  types, and a router-walking test that fails on any drift in either
  direction.
- **UI** — the Targets & Schedules page and the Target card
  (`/targets/{id}`).

### Chart

- 1.5.0 → 1.6.0. New value blocks: `config.checkers.external.*` (agent,
  rendered only when enabled), `console.scheduler.*`,
  `console.rateLimit.*`; a seventh ci profile; the external-mode Valkey
  egress NetworkPolicy now derives its port from the configured address.

## kconmon-ng v1.5.0

> Console release. Adds durable persistence, authentication/RBAC, and an
> on-demand diagnostics runner (M3) on top of the M1/M2 Console. Off by
> default (`console.enabled: false`, `console.database.mode: disabled`,
> `console.auth.mode: anonymous`); existing installs — including existing
> Console installs on `anonymous` auth — are functionally unaffected until
> you turn these on.

### Added

- **PostgreSQL persistence** — `console.database.mode=cnpg|external|disabled`
  (ADR-001): CloudNativePG-provisioned or externally-supplied PostgreSQL for
  event history, RBAC, audit, and diagnostics run history. `GET /api/v1/events`
  now serves durable scrollback for the Live page, backed by `topology_events`
  (all five WebSocket event types, not only topology ones).
- **Authentication and RBAC** — `console.auth.mode=anonymous|local|header|oidc`:
  local users (PostgreSQL, argon2id), header-based trusted-proxy auth (explicit
  CIDR opt-in), and OIDC (code flow + PKCE). Built-in roles
  (`viewer`/`operator`/`alert-editor`/`admin`) are compiled-in and work with
  `database.mode=disabled`; a custom-role admin API layers on top when a
  database is configured. `__Host-` session cookies, CSRF double-submit for
  cookie-authenticated mutations, and an async best-effort audit log
  (`GET /api/v1/audit`).
- **API tokens (PATs)** — `/api/v1/tokens`: SHA-256-hashed bearer tokens that
  work in every auth mode. A PAT is **not** individually scoped in M3: its
  effective permissions are exactly `auth.defaultRole`, deployment-wide — a
  token subject resolves no role bindings of its own, and token-kind bindings
  are rejected by the RBAC API by design (they would silently grant nothing).
  Where a database is configured, disabling a **local** user's account revokes
  the tokens they own on that user's next request; subjects created by header
  or OIDC mode have no `users` row, so their disable state lives upstream at
  the proxy/IdP and only `DELETE /api/v1/tokens/{id}` revokes their tokens.
- **Diagnostics runner** — `POST`/`GET /api/v1/runs`, `GET /api/v1/runs/{id}`:
  bounded on-demand check fan-out (up to 400 pairs) with persisted run history
  and shareable permalinks. Live per-pair progress streams over a new
  ephemeral `run:{id}` WebSocket topic, opened per run, with an automatic
  REST-polling fallback on any console replica other than the one executing
  the run.
- **Object cards v1** — Node and Pair cards with a shared "Recent changes"
  event rail, linked from Topology and the Matrix heatmap.
- **NetworkPolicy** — opens console↔database (CNPG pod-selector rule, or a
  namespace-wide default for `mode=external`) and console→OIDC IdP on both
  layers, alongside the existing console→controller/Valkey rules.

### Fixed

- Chart mounts every console secret (database DSN, local-admin bootstrap
  password, OIDC client secret) under one sibling directory,
  `/etc/kconmon-ng-console-secrets/`, group-readable (`0440`) with
  `console.podSecurityContext.fsGroup` matching the distroless nonroot gid —
  the milestone's originally planned nested path and owner-only mode were
  both unworkable (a read-only ConfigMap volume cannot host a mountpoint
  inside itself, and `0400` is unreadable by the nonroot process without
  matching UID ownership).

### Upgrade Notes

1. This release is safe to roll out with every new feature left at its
   default (`console.database.mode: disabled`, `console.auth.mode: anonymous`):
   the M1/M2 surface behaves identically.
2. **Every existing Console install rolls once on upgrade, even one that
   changes nothing in `values.yaml`.** `auth.defaultRole` and the
   `auth.session.*` block are now rendered into the console ConfigMap
   unconditionally (previously absent keys), so the ConfigMap's content —
   and therefore its checksum annotation on the Deployment's pod template —
   changes for every install, triggering one rollout. There is no data or
   config migration behind it; it is a one-time restart.
3. To turn on persistence and auth, set `console.database.mode` to `cnpg` (this
   chart does **not** install the CloudNativePG operator or its CRDs —
   install those first, or `helm install` fails with a clear error) or
   `external` (supply `console.database.existingSecret`), then set
   `console.auth.mode` and its mode-specific block. See
   `docs/console/architecture/CONFIG.md` and `SECURITY.md` for the full
   validation matrix and secret-mount layout.

### Install

```bash
helm upgrade --install kconmon-ng oci://ghcr.io/esdmitrii/charts/kconmon-ng \
  --version 1.5.0 \
  --namespace kconmon-ng \
  --create-namespace
```

### Images

```
ghcr.io/esdmitrii/kconmon-ng-agent:1.5.0
ghcr.io/esdmitrii/kconmon-ng-controller:1.5.0
ghcr.io/esdmitrii/kconmon-ng-console:1.5.0
```

---

## kconmon-ng v1.4.0

> Console release. Adds an optional read-only web Console (M1) with a realtime
> event pipeline (M2). The Console is off by default (`console.enabled: false`);
> existing agent/controller installs are unaffected until it is turned on.

### Added

- **Console web UI** — new `kconmon-ng-console` binary and image with an embedded
  SPA: Overview, Matrix, Topology, Explore, PromQL console and a Live event feed.
  Read-only in this release (anonymous viewer role, banner shown in the UI).
- **Realtime pipeline** — the controller exposes a leader-gated
  `EventStream.WatchEvents` gRPC stream (`controller.events.enabled`); the console
  ingests it and fans events out to browsers over WebSocket (`/ws`) with per-topic
  sequencing, snapshot replay and duplicate suppression. The matrix switches from
  polling to push when the stream is healthy and falls back to REST automatically.
- **Cross-replica fan-out via Valkey** — `console.valkey.mode` supports
  `off` (in-process, single replica), `bundled` (ephemeral Valkey Deployment, no
  PVC by design) and `external`. NetworkPolicies open console→controller gRPC and
  console→Valkey on both sides.
- **Chart** — new `console.*` values (deployment, service, ingress, PDB,
  NetworkPolicy, bundled Valkey), documented in `docs/configuration.md` and
  covered by `values.schema.json` and a `ci/console-values.yaml` lint profile.

### Fixed

- **Controller graceful shutdown hang with active streaming subscribers** —
  `WatchEvents`/`WatchTasks`/`WatchPeers` handlers now terminate on shutdown and
  `GracefulStop` is bounded with a hard-stop fallback (the Tasks/Peers case was
  latent since v1.3.0, masked by Kubernetes killing the pod after the grace
  period).
- **Fleet-safe events config** — the `events` key is omitted from the controller
  ConfigMap when disabled, so controller images without M2 support keep starting
  under strict config parsing.

### Security

- grpc-go bumped to v1.82.1 (GO-2026-6061, reachable via the event stream).

### Install

```bash
helm upgrade --install kconmon-ng oci://ghcr.io/esdmitrii/charts/kconmon-ng \
  --version 1.4.0 \
  --namespace kconmon-ng \
  --create-namespace
```

kubectl plugin (via krew, from the release manifest):

```bash
kubectl krew install --manifest-url \
  https://github.com/EsDmitrii/kconmon-ng/releases/download/v1.4.0/kconmon.yaml
```

### Images

```
ghcr.io/esdmitrii/kconmon-ng-agent:1.4.0
ghcr.io/esdmitrii/kconmon-ng-controller:1.4.0
ghcr.io/esdmitrii/kconmon-ng-console:1.4.0
```

---

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
- `values.schema.json`: HTTP target field corrected from `expectedStatus` to
  `expectStatus` to match the checker's config (the schema key never matched the
  code, so a schema-guided value was silently ignored).

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
