# Set up alerting

## Goal

Get paged when the network between nodes degrades — and when the monitor
itself goes quiet — without writing PromQL from scratch. kconmon-ng gives you
two independent layers:

1. **Chart-shipped rules** (`prometheusRule.enabled`): nine built-in alerts
   rendered as one static `PrometheusRule`, versioned in Git with your values.
2. **Console-managed rules** (`console.alerting.enabled`): rules built in the
   UI from typed templates or raw PromQL, stored in PostgreSQL and reconciled
   into a *separate*, console-owned `PrometheusRule` object.

Run both, either, or neither — they never touch each other's objects. In both
cases Prometheus evaluates; kconmon-ng only manages rule objects.

## Enable managed rules

The chart layer is one flag (it needs the Prometheus Operator's
`PrometheusRule` CRD):

```bash
helm upgrade kconmon-ng oci://ghcr.io/esdmitrii/charts/kconmon-ng \
  --reuse-values --set prometheusRule.enabled=true
```

That ships nine rules: `UDPLossHigh`, `TCPChecksFailing`, `PairWentSilent`,
`DNSChecksFailing`, `ExternalChecksFailing`, `ZoneChecksFailing`,
`ZoneLossHigh`, plus two that watch the monitor itself —
`KconmonAgentsMissing` and `KconmonControllerDown`. Every expression, and the
reasoning behind the awkward ones, is in the
[default alerting rules](../metrics.md#default-alerting-rules) reference.

The Console layer needs the console, a database and the same CRD:

```yaml
database:
  existingSecret: kconmon-db-app # Secret holding a postgres:// DSN
console:
  enabled: true
  alerting:
    enabled: true # renders a namespaced Role for prometheusrules
```

Then build rules on the [Alerting](../console/alerting.md) page: six typed
templates (pair loss, failure ratios and friends, taking operator units like
percent) or raw PromQL, with a preview that runs the expression against your
Prometheus *right now* and reports how many series it matches. One caveat that
looks like a bug and is not: your Prometheus must *select* the object the
console writes — a stack that scopes `ruleSelector` by label will show the
rule as `synced` and never fire it until you widen the selector.

## Tune thresholds

Every built-in rule takes `enabled` / `threshold` / `for` / `severity`
(thresholds are ratios in 0.0–1.0):

```yaml
prometheusRule:
  enabled: true
  udpLossHigh:
    threshold: 0.25 # page earlier on UDP loss
    severity: critical
  externalChecksFailing:
    enabled: false # not running external checks
  additionalRules: [] # your own rules, appended verbatim
```

Disabling one removes exactly that rule and nothing else. Defaults worth
knowing before you retune: the failure-ratio rules compare
`rate(fail)/rate(all)` — 5% in-cluster, 10% for external targets — rather
than `rate(fail) > 0`, so one flaky probe in a healthy stream does not page
anyone; and `PairWentSilent` fires on ~15 minutes of *absence* (silence has
to outlast a rollout or a drain). Console-managed rules are tuned in the UI
instead — each rule's threshold, hold and severity are fields on the rule.

## Route notifications

**Chart rules** are ordinary Prometheus alerts: route them with whatever
already handles your alerting — Alertmanager, Grafana Alerting. Labels carry
the pair (`source_node`, `destination_node`, both zones), and the annotations
name the failing pair, direction and measured value, so grouping by pair
works out of the box.

**The Console delivers webhooks itself** — off Prometheus's alert state, no
Alertmanager involved. Configure an encryption key first (it protects the
per-endpoint signing secrets at rest):

```yaml
console:
  webhooks:
    existingSecret: kconmon-webhook-key # key: console-webhooks-encryption-key
```

Then add endpoints under Alerting, subscribed to `alert.fired` /
`alert.resolved` (incident transitions can be delivered too). Payloads are
HMAC-signed (`X-Kconmon-Signature: sha256=…` over the exact body bytes). Two
properties to know before you wire this to a pager: `resolvedAt` is only as
precise as `console.webhooks.alertPollInterval` (30s by default), and with
`console.replicas > 1` **every replica delivers** — dedupe on the payload's
stable `(event, ruleId, labels, firedAt)` tuple.

## Test an alert

The **Test** button on a webhook endpoint answers "can I reach you" — it
sends a probe-shaped payload and records the outcome verbatim on the row.

To test the whole path — rule, Prometheus, delivery — break something real on
a disposable stand: [the demo](../demo/breaking-cni.md) blackholes UDP
between two Minikube nodes, watches `UDPLossHigh` go pending within half a
minute, declares a console rule scoped to that pair with a short `for`, and
receives the signed webhook when it fires. Twenty minutes, and afterwards you
know your paging path works end to end — a much better time to learn that
than during the real thing.
