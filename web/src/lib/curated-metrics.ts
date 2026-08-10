import type * as echarts from "echarts";
import { CHART_FALLBACK, chartColors, seriesColor } from "./chart-theme";
import type { Locale } from "./i18n";
import type { PromResult } from "./types";

export interface CuratedChart {
  id: string;
  /** The ENGLISH title, and the SOURCE field: it is what curated-metrics.test.ts
   *  and pages/explore.tsx's own tests read as a fixture, and the fallback for
   *  any chart built at runtime (the signals column, the MTR loss strip) that
   *  never had a Russian half. Display goes through chartTitle. */
  title: string;
  /** The Russian half, optional for exactly the runtime-built charts above. */
  titleRu?: string;
  unit: "seconds" | "ratio";
  query: string;
}

/**
 * chartTitle is what a curated chart READS AS — the one place the display language is decided; `id`
 * remains the identity everywhere (the A/B selects' values, the query keys, the React keys).
 */
export function chartTitle(chart: CuratedChart, locale: Locale): string {
  return locale === "ru" ? (chart.titleRu ?? chart.title) : chart.title;
}

/**
 * RANGE_TOKEN is this console's equivalent of Grafana's `$__range`; so there is no dashboard
 * variable to interpolate a window.
 */
export const RANGE_TOKEN = "$__range";

/** resolveRangeToken swaps RANGE_TOKEN for the drawn window as a PromQL duration. */
export function resolveRangeToken(query: string, rangeSeconds: number): string {
  const seconds = Math.max(1, Math.round(rangeSeconds));
  return query.split(RANGE_TOKEN).join(`${seconds}s`);
}

// Pinning the ranker with `@ end` picks the five ONCE, and `and on` uses that set as a membership
// filter.
export const CURATED_CHARTS: CuratedChart[] = [
  {
    id: "tcp-p95",
    title: "TCP RTT p95 (worst 5 pairs)",
    titleRu: "TCP RTT p95 (худшие 5 пар)",
    unit: "seconds",
    query:
      "histogram_quantile(0.95, sum by (source_node, destination_node, le) (rate(kconmon_ng_tcp_total_duration_seconds_bucket[5m])))" +
      ` and on (source_node, destination_node) topk(5, histogram_quantile(0.95, sum by (source_node, destination_node, le) (rate(kconmon_ng_tcp_total_duration_seconds_bucket[${RANGE_TOKEN}] @ end()))))`,
  },
  {
    // The ranker is avg_over_time, not the instantaneous gauge: loss is spiky
    // and mostly zero, so ranking on a single sample would re-pick five
    // different pairs on every poll even though the shape below is stable.
    id: "udp-loss",
    title: "UDP packet loss (worst 5 pairs)",
    titleRu: "UDP: потери пакетов (худшие 5 пар)",
    unit: "ratio",
    query:
      "avg by (source_node, destination_node) (kconmon_ng_udp_packet_loss_ratio)" +
      ` and on (source_node, destination_node) topk(5, avg by (source_node, destination_node) (avg_over_time(kconmon_ng_udp_packet_loss_ratio[${RANGE_TOKEN}] @ end())))`,
  },
  {
    id: "icmp-p95",
    title: "ICMP RTT p95 (worst 5 pairs)",
    titleRu: "ICMP RTT p95 (худшие 5 пар)",
    unit: "seconds",
    query:
      "histogram_quantile(0.95, sum by (source_node, destination_node, le) (rate(kconmon_ng_icmp_rtt_seconds_bucket[5m])))" +
      ` and on (source_node, destination_node) topk(5, histogram_quantile(0.95, sum by (source_node, destination_node, le) (rate(kconmon_ng_icmp_rtt_seconds_bucket[${RANGE_TOKEN}] @ end()))))`,
  },
  {
    id: "dns-p95",
    title: "DNS resolution p95 by host",
    titleRu: "DNS: разрешение имён, p95 по хостам",
    unit: "seconds",
    query: "histogram_quantile(0.95, sum by (host, le) (rate(kconmon_ng_dns_duration_seconds_bucket[5m])))",
  },
  {
    // Corrected during implementation: the first sketch of this query (a label_replace-free `and on
    // vector(0) or ...` shape) was flagged as wrong in the task brief.
    id: "fail-rate",
    title: "Probe failure rate by protocol",
    titleRu: "Частота сбоев зондов по протоколам",
    unit: "ratio",
    query:
      'sum by (protocol) (label_replace(rate(kconmon_ng_tcp_results_total{result="fail"}[5m]), "protocol", "tcp", "", "") or label_replace(rate(kconmon_ng_udp_results_total{result="fail"}[5m]), "protocol", "udp", "", "") or label_replace(rate(kconmon_ng_icmp_results_total{result="fail"}[5m]), "protocol", "icmp", "", ""))',
  },
];

// Prometheus range-query matrix result entry (Prometheus's own envelope shape,
// narrowed from PromResult.data.result's `unknown[]`).
interface MatrixSeriesEntry {
  metric: Record<string, string>;
  values: [number, string][];
}

// Compact series label: peer pair, then host, then bare protocol — the three
// label shapes the curated queries above actually produce.
function labelForMetric(metric: Record<string, string>): string {
  if (metric.source_node && metric.destination_node) return `${metric.source_node}→${metric.destination_node}`;
  if (metric.host) return metric.host;
  if (metric.protocol) return metric.protocol;
  const values = Object.values(metric);
  return values.length > 0 ? values.join(",") : "series";
}

/**
 * formatSeconds prints a seconds value as milliseconds, with ADAPTIVE precision; EXPORTED for
 * components/investigation-signals.tsx.
 */
export function formatSeconds(value: number): string {
  return formatMillis(value * 1000);
}

/**
 * formatMillis is formatSeconds' rule with the unit conversion taken out; it exists because
 * components/mtr-hop-table.tsx's hop-RTT trend is the one chart in the console fed from stored
 * snapshots rather than from Prometheus.
 */
export function formatMillis(ms: number): string {
  return `${Math.abs(ms) < 10 ? ms.toFixed(1) : ms.toFixed(0)}ms`;
}

function formatRatio(value: number): string {
  return `${(value * 100).toFixed(1)}%`;
}

// Chart colour now comes from the design-system tokens via lib/chart-theme.ts.
export const AXIS_COLOR = { dark: CHART_FALLBACK.dark.axis, light: CHART_FALLBACK.light.axis };
export const SPLIT_COLOR = { dark: CHART_FALLBACK.dark.grid, light: CHART_FALLBACK.light.grid };

export function toSeriesOption(chart: CuratedChart, res: PromResult, dark: boolean): echarts.EChartsOption {
  const entries: MatrixSeriesEntry[] =
    res.status === "success" && res.data?.resultType === "matrix"
      ? (res.data.result as MatrixSeriesEntry[])
      : [];
  const fmt = chart.unit === "seconds" ? formatSeconds : formatRatio;
  const colors = chartColors(dark ? "dark" : "light");

  // Stable colour assignment: Prometheus returns a matrix in an arbitrary order per poll (the
  // ranked queries above no more than any other).
  const sorted = [...entries].sort((a, b) =>
    labelForMetric(a.metric).localeCompare(labelForMetric(b.metric)),
  );

  return {
    animation: false,
    textStyle: { color: colors.axis },
    /* The bottom band is reserved for the scrollable legend so it can never
       collide with the y-axis labels again. */
    grid: { left: 56, right: 16, top: 12, bottom: 46 },
    legend: {
      bottom: 0,
      type: "scroll",
      icon: "roundRect",
      itemWidth: 10,
      itemHeight: 2,
      textStyle: { color: colors.axis, fontSize: 11 },
      pageIconColor: colors.axis,
      pageTextStyle: { color: colors.axis },
    },
    tooltip: {
      trigger: "axis",
      axisPointer: { type: "line", lineStyle: { color: colors.grid } },
      valueFormatter: (value) => fmt(Number(value)),
    },
    xAxis: {
      type: "time",
      axisLine: { lineStyle: { color: colors.grid } },
      /* hideOverlap drops the ticks that would collide instead of drawing them on top of each other. */
      axisLabel: { color: colors.axis, hideOverlap: true },
      splitLine: { show: false },
    },
    yAxis: {
      type: "value",
      axisLabel: { color: colors.axis, formatter: (value: number) => fmt(value) },
      splitLine: { lineStyle: { color: colors.grid } },
    },
    series: sorted.map(
      (entry, i): echarts.LineSeriesOption => ({
        name: labelForMetric(entry.metric),
        type: "line",
        showSymbol: false,
        smooth: false,
        color: seriesColor(colors, i),
        lineStyle: { width: 2 },
        data: (entry.values ?? []).map(([ts, v]) => [ts * 1000, Number(v)]),
      }),
    ),
  };
}
