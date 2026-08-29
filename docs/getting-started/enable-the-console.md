# Enable the console

The Console is an optional web UI deployed by the same chart, **off by
default**. It reads the same Prometheus and the same controller you already
run — no second data path, no agent changes.

## What the console adds

The [Matrix](../console/matrix.md) as an N×N heatmap, a
[topology map](../console/topology.md), an [Overview](../console/overview.md)
health summary, curated [metric charts](../console/metrics.md) and a raw
[PromQL page](../console/promql.md) — all with nothing but a Prometheus URL.
Turn on more flags and it grows durable history, [incident
timelines](../console/incidents.md), [MTR path history](../console/routes-mtr.md),
an [alert-rule editor](../console/alerting.md), authentication with RBAC and an
audit log, and the [Time Machine](../console/time-machine.md): a `?at=` on any
URL rewinds every page to a past instant. The
[console guide](../console/overview.md) walks every screen.

## Feature flag matrix: database, redis, events, alerting

The Console degrades honestly: each capability is one flag, and a missing one
disables exactly its own features while everything else keeps working.

| Flag | What it unlocks | While it is off |
| --- | --- | --- |
| `console.prometheus.url` | The data pages: Matrix, Metrics, PromQL | Those API routes answer `503`; topology still works |
| `database.existingSecret` | History, incidents, saved alert rules, audit log, `local`/`oidc` auth, schedules | In-memory console: read-only pages only, nothing survives a restart |
| `redis.existingSecret` | Sessions, rate-limit counters and realtime fan-out shared across replicas | In-process bus; `console.replicas` must stay `1` — the chart refuses more |
| `controller.events.enabled` | Realtime push: the Events feed and matrix updates over a WebSocket | Pages poll every 15s and say so |
| `console.alerting.enabled` | The alert-rule editor, reconciled into one console-owned `PrometheusRule` | The Alerting page cannot sync rules; needs a database and the Prometheus Operator CRD |

Two of those are not `console.*` keys on purpose: PostgreSQL and the
Redis-compatible bus are infrastructure the chart *consumes*, never installs.
Each is a Secret holding one DSN (`postgres://…`, `redis://…`) — any provider
works. The [chart README](https://github.com/EsDmitrii/kconmon-ng/tree/main/charts/kconmon-ng#the-stack-around-it-bring-your-own)
documents the stack it is tested against (CloudNativePG, valkey-helm).

## Minimal enable

It is a flag on the same release. Nothing else in the chart changes:

```bash
helm upgrade --install kconmon-ng oci://ghcr.io/esdmitrii/charts/kconmon-ng \
  --set console.enabled=true \
  --set console.prometheus.url=http://prometheus-operated.monitoring:9090

kubectl port-forward svc/kconmon-ng-console 8081:8080
```

That gets you the read-only pages as an anonymous viewer on
<http://localhost:8081>. Add flags from the matrix above one at a time as you
need them; every knob is documented inline in the
[full values.yaml](../reference/helm-values.md#full-valuesyaml).

## Authentication modes

`console.auth.mode` picks one of four modes; RBAC applies in all of them
(four built-in roles — `viewer`, `operator`, `alert-editor`, `admin` — plus
custom ones).

| Mode | Identity comes from | Requires |
| --- | --- | --- |
| `anonymous` (default) | nobody — every request gets the fixed `console.auth.anonymous.role` (`viewer` by default) | nothing |
| `local` | username/password against the database, with a bootstrap admin created on first start | `database.existingSecret` |
| `header` | a trusted in-cluster reverse proxy setting `X-Remote-User`/`X-Remote-Groups` | non-empty `console.auth.header.trustedProxyCIDRs` — otherwise headers are an auth bypass, and the console refuses to start |
| `oidc` | your identity provider, authorization-code flow with PKCE | `database.existingSecret`; plus `redis.existingSecret` when `console.replicas > 1` |

For `header` and `oidc`, map the groups your identity provider asserts onto
console roles with `console.auth.groupRoles` — that map is what makes a fresh
install usable before anyone can create role bindings through the API. The
[OIDC setup scenario](../scenarios/oidc-setup.md) walks the full flow.

## Next steps

- **[Catch a breakage](catch-a-breakage.md)** — see the Console isolate a
  deliberate failure.
- **[Console guide](../console/overview.md)** — one page per screen.
- **[Set up alerting](../scenarios/set-up-alerting.md)** — chart-shipped rules,
  console-managed rules, and webhooks.
