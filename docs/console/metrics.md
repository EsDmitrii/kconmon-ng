# Metrics

<!-- screenshot: console-metrics.png pending post-redesign reshoot -->

Curated metric charts. It answers: **how do the fleet's key series look over time, and how do they compare with each
other or with an earlier window?**

!!! note
    This page documents the console's **Metrics** screen. The reference for the exported Prometheus metrics and the
    chart-shipped alert rules is [Metrics and alerting](../metrics.md).

## What this page shows

Five curated charts "across TCP/UDP/ICMP/DNS, recomputed from Prometheus every 30s":

- **TCP RTT p95 (worst 5 pairs)**
- **UDP packet loss (worst 5 pairs)**
- **ICMP RTT p95 (worst 5 pairs)**
- **DNS resolution p95 by host**
- **Probe failure rate by protocol**

A **Time range** switch (`15m` / `1h` / `6h` / `24h`) applies to every chart. All queries go through the console's
guarded Prometheus proxy (`POST /api/v1/promql/query_range`). With the [Time Machine](time-machine.md) engaged, the
window ends at the viewed instant and the range is measured back from there.

## Built-in charts

Each chart is a fixed PromQL query over the exported `kconmon_ng_*` series — the exact expressions live in
`web/src/lib/curated-metrics.ts`, and each can be reproduced or modified freely on the [PromQL](promql.md) page. An
empty chart says so honestly: "No series returned for this range — try a longer window above."

## Choosing series

The **Compare** panel overlays a reference leg on metric A's axes:

- **Compare A with** — *another metric* (pick **Metric A** and **Compare with metric**) or *itself, earlier*
  (**Compare with earlier**: `1h` / `24h` / `7d`). Self-comparison draws "A · now (solid)" against
  "A · {shift} earlier (dashed)".
- Units are never silently mixed: "B is a ratio on A's seconds axis — read its shape, not its height."
- A shift past Prometheus's retention answers plainly: "No data {shift} ago — Prometheus's retention does not reach
  that far back."

## Annotations and maintenance windows

Two bars ride under the charts, scoped to the plotted window:

- **＋ annotate** — drop a note at a moment or over a range ("Rolled the gateway"). Needs `annotations:write`.
  Annotations also appear interleaved in [Events](events.md) and as timeline rows in [Incidents](incidents.md).
- **＋ maintenance** — declare a maintenance window (start, end, reason). Needs `maintenance:write`. Declared windows
  mute alert noise attribution and annotate every chart they cover; the full list is managed on
  [Alerting](alerting.md#maintenance-windows).

Both need the database.

## Deep links

- *Compare in Explore* from [Incidents](incidents.md) opens this page (the A/B slots stay bound to curated metrics —
  choose the window here; the page says so rather than dropping half the promise).
- The command palette's *Add an annotation…* and *Declare a maintenance window…* actions land here.

## Use it when

- You want trend context around an incident — the worst pairs' p95 an hour before vs now.
- You suspect a regression after a change: compare a chart with itself 24h or 7d earlier.
- You are annotating operational events so the next investigation reads them in place.

Verified against `web/src/pages/explore.tsx`, `web/src/lib/curated-metrics.ts`, `web/src/lib/i18n/dict/explore.ts`,
`web/src/components/annotations.tsx`, `web/src/components/maintenance.tsx`.
