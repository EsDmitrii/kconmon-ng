import type * as echarts from "echarts";
import { CHART_FALLBACK, chartColors, seriesColor } from "./chart-theme";
import type { PromResult } from "./types";

export interface CuratedChart {
  id: string;
  title: string;
  unit: "seconds" | "ratio";
  query: string;
}

// Verified against docs/metrics.md + internal/metrics/prometheus.go (metric-name
// allowlist test in curated-metrics.test.ts guards against typos here).
export const CURATED_CHARTS: CuratedChart[] = [
  {
    id: "tcp-p95",
    title: "TCP RTT p95 (worst 5 pairs)",
    unit: "seconds",
    query:
      "topk(5, histogram_quantile(0.95, sum by (source_node, destination_node, le) (rate(kconmon_ng_tcp_total_duration_seconds_bucket[5m]))))",
  },
  {
    id: "udp-loss",
    title: "UDP packet loss (worst 5 pairs)",
    unit: "ratio",
    query: "topk(5, avg by (source_node, destination_node) (kconmon_ng_udp_packet_loss_ratio))",
  },
  {
    id: "icmp-p95",
    title: "ICMP RTT p95 (worst 5 pairs)",
    unit: "seconds",
    query:
      "topk(5, histogram_quantile(0.95, sum by (source_node, destination_node, le) (rate(kconmon_ng_icmp_rtt_seconds_bucket[5m]))))",
  },
  {
    id: "dns-p95",
    title: "DNS resolution p95 by host",
    unit: "seconds",
    query: "histogram_quantile(0.95, sum by (host, le) (rate(kconmon_ng_dns_duration_seconds_bucket[5m])))",
  },
  {
    // Corrected during implementation: the first sketch of this query (a
    // label_replace-free `and on() vector(0) or ...` shape) was flagged as
    // wrong in the task brief. This is the fixed, testable replacement —
    // three plain per-protocol series requested as one PromQL query.
    id: "fail-rate",
    title: "Probe failure rate by protocol",
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
 * formatSeconds prints a seconds value as milliseconds, with ADAPTIVE
 * precision (QA round 2, finding #8).
 *
 * Integer milliseconds is right for a chart plotted in tens or hundreds of ms
 * and wrong below ten, where the y-axis' own tick spacing is finer than the
 * format: an axis stepping 0.0005s produced "1ms, 1ms, 2ms, 2ms, 2ms" — five
 * ticks, three distinct labels, and no way to read a value off it. One decimal
 * under 10ms separates them; above it a decimal is noise on a figure whose
 * useful precision is the millisecond.
 *
 * The threshold is on the RENDERED value, not the input, so the same rule
 * applies to a tooltip's single sample and to an axis tick.
 *
 * EXPORTED for components/investigation-signals.tsx (QA round 3, finding #14),
 * which now names this formatter explicitly on its own y-axis rather than
 * inheriting it from toSeriesOption below. See signalChartOption there for why
 * the signals column states both axis treatments instead of trusting them.
 */
export function formatSeconds(value: number): string {
  return formatMillis(value * 1000);
}

/**
 * formatMillis is formatSeconds' rule with the unit conversion taken out —
 * the SAME adaptive precision, for a series whose values are already
 * milliseconds.
 *
 * It exists because components/mtr-hop-table.tsx's hop-RTT trend is the one
 * chart in the console fed from stored snapshots rather than from Prometheus:
 * its points are millisecond numbers, so it cannot call formatSeconds, and it
 * had grown a private `ms.toFixed(1)` that disagreed with every Prometheus
 * chart above 10ms (QA round 4, finding #11). One rule, two entry points, so
 * "1ms, 1ms, 2ms" on one axis and "1.0ms, 1.5ms, 2.0ms" on another cannot
 * happen again.
 */
export function formatMillis(ms: number): string {
  return `${Math.abs(ms) < 10 ? ms.toFixed(1) : ms.toFixed(0)}ms`;
}

function formatRatio(value: number): string {
  return `${(value * 100).toFixed(1)}%`;
}

// Chart colour now comes from the design-system tokens via lib/chart-theme.ts.
// AXIS_COLOR/SPLIT_COLOR stay exported as thin aliases because
// promql-console.tsx imports them; they resolve to the same documented
// fallback values chart-theme pins.
export const AXIS_COLOR = { dark: CHART_FALLBACK.dark.axis, light: CHART_FALLBACK.light.axis };
export const SPLIT_COLOR = { dark: CHART_FALLBACK.dark.grid, light: CHART_FALLBACK.light.grid };

export function toSeriesOption(chart: CuratedChart, res: PromResult, dark: boolean): echarts.EChartsOption {
  const entries: MatrixSeriesEntry[] =
    res.status === "success" && res.data?.resultType === "matrix"
      ? (res.data.result as MatrixSeriesEntry[])
      : [];
  const fmt = chart.unit === "seconds" ? formatSeconds : formatRatio;
  const colors = chartColors(dark ? "dark" : "light");

  // Stable colour assignment: topk() returns series in an arbitrary order per
  // poll, so colours are assigned by sorted label, not arrival order —
  // otherwise every refresh could repaint the same pair a different colour.
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
      /* hideOverlap drops the ticks that would collide instead of drawing them
         on top of each other (QA round 2, finding #19): under ~700px the time
         axis smeared its own labels into an unreadable band. ECharts thins the
         set out and keeps the ends, which is the readable trade — fewer
         stamps, all legible, rather than every stamp and none. */
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
