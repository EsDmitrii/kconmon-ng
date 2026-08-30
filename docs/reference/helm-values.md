# Helm values

The chart installs the **monitor** (agent, controller, console) and nothing
else: PostgreSQL, a Redis-compatible bus and Prometheus are infrastructure it
consumes, each configured by one DSN or URL. The
[full values.yaml below](#full-valuesyaml) documents every key inline and is
the authoritative reference; this table is the short list you will actually
touch.

## Key values at a glance

| Key | Default | What it does |
| --- | --- | --- |
| `config.metricsPrefix` | `kconmon_ng` | Prefix for every exported metric; changing it renames all of them |
| `config.checkers.tcp.enabled` | `true` | TCP checker (interval `5s`, timeout `1s`) |
| `config.checkers.udp.enabled` | `true` | UDP checker (interval `5s`, timeout `250ms`, `packets: 5`) |
| `config.checkers.icmp.enabled` | `true` | ICMP checker (interval `5s`, timeout `1s`); unprivileged socket, no added capabilities |
| `config.checkers.dns.enabled` | `true` | DNS checker (interval `5s`, timeout `5s`) |
| `config.checkers.http.enabled` | `false` | HTTP checker; `targets` are required when enabled |
| `config.checkers.external.enabled` | `false` | Probes to non-peer destinations, gated by `allowedCidrs`; see [External targets](../scenarios/external-targets.md) |
| `config.checkers.mtr.cooldown` | `60s` | Minimum gap between reactive MTR traces for the same (src, dst) pair |
| `config.checkers.mtr.maxHops` | `30` | Traceroute hop ceiling (1–64) |
| `config.controllerAgentTtl` | `30s` | Evict an agent missing heartbeats for this long (min `10s`) |
| `agent.tolerations` | `[{operator: Exists}]` | Run the agent on every node, tainted ones included |
| `agent.metrics.detail` | `full` | Scrape-time cardinality valve: `full` / `counters-only` / `zone-only` (~70 / ~10 / ~0 series per directed pair); needs `serviceMonitor.enabled` |
| `agent.pingGroupRange` | `true` | Render the `net.ipv4.ping_group_range` sysctl the ICMP socket needs |
| `agent.updateStrategy` | `RollingUpdate`, `maxUnavailable: 1` | DaemonSet rollout, passed through verbatim; raise on large fleets |
| `controller.replicaCount` | `1` | Controller replicas; only the leader is active |
| `controller.leaderElection` | `true` | Leader election; `false` also disables zone enrichment and `expected_agents` |
| `controller.events.enabled` | `false` | Domain event stream for the Console's realtime pages (leader-only) |
| `controller.externalGateway.enabled` | `false` | The TLS gateway for [external agents](../external-agents.md): a second gRPC listener, exposed by its own NodePort/LoadBalancer Service |
| `controller.externalGateway.port` | `9443` | Gateway listener; must differ from `config.{httpPort,grpcPort,metricsPort}` |
| `controller.externalGateway.tls.secretName` | `""` | `kubernetes.io/tls` Secret with the serving pair; REQUIRED when enabled. `tls.clientCaKey` names the client-CA bundle key in the same Secret; empty means token-only mode |
| `controller.externalGateway.bootstrapToken.secretName` | `""` | Secret holding the shared bearer token (< 16 chars is refused); REQUIRED when enabled |
| `console.enabled` | `false` | Deploy the [Console](../getting-started/enable-the-console.md) |
| `console.replicas` | `1` | More than 1 REQUIRES `redis.existingSecret`; the chart refuses the combination otherwise |
| `console.prometheus.url` | `""` | Required by the data pages (Matrix, Metrics, PromQL), which answer `503` without it |
| `console.auth.mode` | `anonymous` | `anonymous` / `local` / `header` / `oidc` |
| `console.auth.groupRoles` | `{}` | IdP group → console role map; what makes an `oidc`/`header` install usable from a cold database |
| `console.alerting.enabled` | `false` | Console-managed alert rules, reconciled into one `PrometheusRule`; needs a database and the operator CRD |
| `console.webhooks.existingSecret` | `""` | Secret with the AES-256-GCM key encrypting webhook signing secrets at rest (key `console-webhooks-encryption-key`); empty leaves endpoint create and test at `503`; see [Set up alerting](../scenarios/set-up-alerting.md) |
| `console.scheduler.enabled` | `false` | The schedule/dispatch loop for [scheduled and external checks](../concepts/checks-runs-schedules.md) |
| `console.scheduler.tickInterval` | `5s` | Poll cadence of that loop; must be > 0 when enabled |
| `database.existingSecret` | `""` | Secret holding a `postgres://` DSN; empty means an in-memory console |
| `database.retentionDays` | `90` | Daily prune of stored history; `0` keeps everything |
| `redis.existingSecret` | `""` | Secret holding a `redis://` DSN; empty means the in-process bus (single replica only) |
| `dashboards.enabled` | `false` | Ship the Grafana dashboards as sidecar-labelled ConfigMaps |
| `serviceMonitor.enabled` | `false` | Prometheus Operator `ServiceMonitor` for agents, controller and console |
| `prometheusRule.enabled` | `false` | The nine [built-in alert rules](../metrics.md#default-alerting-rules) as one `PrometheusRule` |
| `prometheusRule.<alertName>` | all enabled | Per-rule `enabled` / `threshold` / `for` / `severity` knobs |
| `prometheusRule.additionalRules` | `[]` | Your rules, appended verbatim |
| `networkPolicy.enabled` | `false` | NetworkPolicy for agent/controller traffic |
| `networkPolicy.prometheusNamespace` | `""` | REQUIRED for the scrape rule to render when policies are on; unset means `up == 0`, visibly |
| `networkPolicy.externalAgentCidrs` | `[]` | Source CIDRs of external agents, opened on the gateway port toward the controller pods alone; REQUIRED when the gateway and this policy are both on (plain CIDR strings). Mind NAT — see [External agents](../external-agents.md) |

Secrets follow one pattern everywhere: `existingSecret` names a Secret you
created (recommended), or a sibling `secret.create: true` block lets the
chart render it, meant for a secrets injector's `${vault:...}` placeholders,
not for literals. The
[chart README](https://github.com/EsDmitrii/kconmon-ng/tree/main/charts/kconmon-ng#chart-managed-secrets)
lists every consumer and its key.

## Full values.yaml

The complete, commented `values.yaml` of the current chart, embedded from
the repo at build time so it cannot drift from what the chart ships:

??? example "charts/kconmon-ng/values.yaml: every key, documented inline"

    ```yaml
    --8<-- "charts/kconmon-ng/values.yaml"
    ```
