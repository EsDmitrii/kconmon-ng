# PromQL

<!-- screenshot: console-promql.png pending post-redesign reshoot -->

Ad-hoc PromQL dev-tools. It answers: **what does this exact query return, against the same Prometheus the rest of the
console reads from?**

## What this page shows

A CodeMirror PromQL editor and three result views. Queries go through the console's guarded proxy
(`POST /api/v1/promql/query` and `/query_range`), which enforces `console.prometheus.queryTimeout`, `maxRange`
(default `24h`) and a response size cap — see the [Helm values](../reference/helm-values.md). The server gates the
endpoint on the `promql:query` permission.

## Query editor

- **Run** executes the query; so does `Cmd/Ctrl+Enter` in the editor.
- **Query mode** — *Instant* or *Range*.
- *Range* mode adds **Range** (`15m` / `1h` / `6h` / `24h`) and **Step** (`15s` / `30s` / `1m` / `5m` / `15m`) —
  unlike Metrics, the step is yours to control; a suggested step targeting ~240 points per series is preselected.
- The last query is remembered in this browser.
- Inside the editor, Cmd/Ctrl+K still opens the [command palette](command-palette.md) — the page re-binds it away
  from CodeMirror's default.

Result views: **Table** (series, last value, point counts), **Chart** (range queries), **JSON** (the raw Prometheus
envelope). Prometheus's own error text renders verbatim.

With the [Time Machine](time-machine.md) engaged, instant queries are evaluated at the viewed instant and a range
ends there.

## Useful starting queries

The exported series are documented in [Metrics and alerting](../metrics.md). Two seeds:

```promql
# TCP RTT p95 per pair over 5m
histogram_quantile(0.95,
  sum by (source_node, destination_node, le)
    (rate(kconmon_ng_tcp_total_duration_seconds_bucket[5m])))
```

```promql
# UDP packet loss ratio per pair (exported as a gauge)
avg by (source_node, destination_node) (kconmon_ng_udp_packet_loss_ratio)
```

The five curated charts on [Metrics](metrics.md) are themselves plain PromQL (`web/src/lib/curated-metrics.ts`) —
copy one here as a starting point.

## Deep links

- The *raw* alert-rule kind on [Alerting](alerting.md) stores hand-written PromQL — prototype it here first; the
  rule builder's preview runs it through the same proxy.

## Use it when

- A curated chart is close but not quite the cut you need.
- You are drafting an alert expression and want the series count and shape before saving a rule.
- You want to sanity-check what Prometheus actually has, without leaving the console or standing up Grafana.

Verified against `web/src/pages/promql-console.tsx`, `web/src/components/promql-editor.tsx`,
`web/src/lib/i18n/dict/promql-console.ts`, and `internal/console/httpapi/alertrules.go` (permission note).
