# Alerting

Console-managed Prometheus alert rules. The page's own description: "Console-managed Prometheus alert rules, what the
cluster thinks of them, the rules it holds that this console does not own, and the maintenance windows that mute
these signals."

![Alerting](../img/console-alerting.png)

## What this page shows

Three sections: **Alert rules**, **Foreign rules**, **Maintenance windows**. Reading needs `alerts:read` (held by
every built-in role); managing rules needs `alerts:manage`. Rules live in the console database and are applied to the
cluster as **one PrometheusRule object** by a reconciler — which only runs when `console.alerting.enabled` is set in
the [Helm values](../reference/helm-values.md) (off by default: "enabling it lets the console write a cluster
object"). With it off, sync actions answer "Prometheus rule sync is disabled".

## Managed rules and sync status

Each rule row shows its enabled state and the reconciler's verdict as of the stamped instant:

| Status | Meaning |
| --- | --- |
| **synced** | The cluster object matches this rule. |
| **drift** | The cluster object differs from what this console last applied. |
| **error** | The last reconcile failed; the row carries the message. |
| **unsynced** | Not yet applied. |

Row actions: *Details* (rendered expression, `for` duration, last-applied stamp), *Sync now* (requests a reconcile —
"the outcome lands on this row as its sync status"), *Edit*, *Delete* (with confirm).

## Editing thresholds

**New rule** opens the builder. Fields: *Name* (seeds the alert name — CamelCase convention), *Kind*, *Severity*
(`info` / `warning` / `critical` — the label Alertmanager routes on), *For*, per-kind parameters, extra *Labels* and
*Annotations*, *Enabled*.

Rule kinds (templates), as the option list words them:

- `pair-loss` — packet loss between nodes
- `zone-latency` — cross-zone latency quantile
- `dns-failures` — DNS failure share
- `http-ttfb` — HTTP time-to-first-byte
- `agent-missing` — registered agents below expected
- `external-target-down` — external target failing
- `raw` — hand-written PromQL (prototype it on the [PromQL](promql.md) page first)

The **Preview** panel renders the expression and evaluates it — "Matches {series} series right now", with zero named
as an answer, not a failure. An expression Prometheus refuses blocks saving: a bad entry in the bundle would stop the
*other* rules from being applied too.

**Foreign rules** lists PrometheusRule objects in the console's namespace that it does not own. Read-only, with an
*Import* action that **copies** a rule's alerting entries into console-managed rows — the original object is
untouched, and the page warns that the same alerts then exist twice until its owner removes theirs.

## Maintenance windows

The full list of declared windows — including entirely-future ones — with delete actions; this is the only unbounded
view of them in the console. Declaring a window still happens next to the chart it explains, on
[Incidents](incidents.md) or [Metrics](metrics.md#annotations-and-maintenance-windows); managing the list needs
`maintenance:write`.

## Webhooks

Alert-driven notifications are delivered through webhook endpoints configured on
[Settings](settings.md#webhooks); with alerting and webhooks both enabled, alert transitions are polled and
`alert.fired` / `alert.resolved` events are delivered to subscribed endpoints.

## Deep links

- The [Overview](overview.md) *Firing alerts* panel links each firing alert to its rule here (`/alerting?rule=<id>`)
  and offers a per-alert *investigate* into [Incidents](incidents.md).
- Firing state itself is read on Overview and in an investigation's timeline — this page manages the rules.

## Use it when

- You want packet-loss or latency alerts without writing PromQL — pick a kind, set the threshold, preview, save.
- Rules already exist in the cluster and you want the console to own copies (import from foreign rules).
- A planned change is coming and its alerts should be attributable — declare a maintenance window first.

See the walkthrough: [Set up alerting](../scenarios/set-up-alerting.md). Chart-shipped (non-console) Prometheus rules
are documented in [Metrics and alerting](../metrics.md).

Verified against `web/src/pages/alerting.tsx`, `web/src/lib/i18n/dict/alerting.ts`
(API: `/api/v1/alert-rules`, `…/preview`, `…/foreign`, `…/import`, `…/{id}/sync`, `/api/v1/maintenance`), and
`charts/kconmon-ng/values.yaml` (`console.alerting.*`, `console.webhooks.alertPollInterval`).
