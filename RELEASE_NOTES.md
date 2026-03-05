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
