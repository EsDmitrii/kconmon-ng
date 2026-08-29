import type * as echarts from "echarts";
import { CHART_FALLBACK, chartColors, seriesColor } from "./chart-theme";
import { NO_VALUE } from "./chart-tooltip";
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

/** resolveRangeToken swaps RANGE_TOKEN for the drawn window as a PromQL duration.
 *
 *  Whatever comes in, what goes out is a duration Prometheus can parse: an
 *  interpolated `[NaNs]` is not a slow query, it is a 400 from a chart that had
 *  no business asking. */
export function resolveRangeToken(query: string, rangeSeconds: number): string {
  const seconds = Number.isFinite(rangeSeconds) ? Math.max(1, Math.round(rangeSeconds)) : 1;
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
function labelForMetric(metric: Record<string, string> = {}): string {
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
 *
 * The non-finite gate is not defensive padding — it is the ANSWER three of the
 * five curated queries actually give. `histogram_quantile` over a window with no
 * observations is NaN, and over a saturated top bucket it is +Inf; both used to
 * reach a y-axis tick and a tooltip row as the literal text "NaNms". A reading
 * that does not exist is written the way the rest of the console writes it.
 */
export function formatMillis(ms: number): string {
  if (!Number.isFinite(ms)) return NO_VALUE;
  return `${Math.abs(ms) < 10 ? ms.toFixed(1) : ms.toFixed(0)}ms`;
}

function formatRatio(value: number): string {
  // Same rule, and here it matters twice over: a MISSING loss figure and a zero
  // loss figure are opposite readings of a link.
  if (!Number.isFinite(value)) return NO_VALUE;
  return `${(value * 100).toFixed(1)}%`;
}

// Chart colour now comes from the design-system tokens via lib/chart-theme.ts.
export const AXIS_COLOR = { dark: CHART_FALLBACK.dark.axis, light: CHART_FALLBACK.light.axis };
export const SPLIT_COLOR = { dark: CHART_FALLBACK.dark.grid, light: CHART_FALLBACK.light.grid };

/* ── legend elision (M3-6) ──────────────────────────────────────────────── */

/**
 * legendNamePrefix is the prefix every arrow-separated half of every series name shares, cut back
 * to a "-" or "." separator — pages/matrix.tsx's sharedNamePrefix rule applied to legend entries.
 * Re-stated here rather than imported because lib code must not depend on a page module.
 */
export function legendNamePrefix(names: readonly string[]): string {
  const segments = names.flatMap((n) => n.split("→"));
  if (segments.length < 2) return "";
  let prefix = segments[0];
  for (const s of segments.slice(1)) {
    let i = 0;
    while (i < prefix.length && i < s.length && prefix[i] === s[i]) i++;
    prefix = prefix.slice(0, i);
    if (prefix === "") return "";
  }
  const cut = Math.max(prefix.lastIndexOf("-"), prefix.lastIndexOf("."));
  if (cut < 3) return "";
  prefix = prefix.slice(0, cut + 1);
  const shortest = Math.min(...segments.map((s) => s.length));
  return shortest - prefix.length >= 2 ? prefix : "";
}

/**
 * elideSeriesName is the legend DISPLAY of one series: each name segment that carries the shared
 * prefix loses it to an ellipsis, so five entries reading "adm-kuber-0…" become "…01→…02" and the
 * scroll legend pages through suffixes a reader can tell apart. Splitting keeps the "→" and the
 * compare panel's " · " joins, so a leg-labelled name elides its node halves too.
 */
export function elideSeriesName(name: string, prefix: string): string {
  if (!prefix) return name;
  return name
    .split(/(→| · )/)
    .map((part) =>
      part !== "→" && part !== " · " && part.startsWith(prefix) && part.length > prefix.length
        ? `…${part.slice(prefix.length)}`
        : part,
    )
    .join("");
}

/**
 * PlotWindow is the span the reader ASKED for. Passed in, the axis is pinned to it instead of to
 * whatever the data happened to cover: a 24h pick over a Prometheus holding six hours drew a
 * six-hour axis and looked identical to the 6h pick, so the range buttons appeared to change
 * nothing but the curves (owner report). Pinned, the gap is the answer.
 */
export interface PlotWindow {
  start: Date;
  end: Date;
}

export function toSeriesOption(
  chart: CuratedChart,
  res: PromResult,
  dark: boolean,
  window?: PlotWindow,
): echarts.EChartsOption {
  /* Array.isArray, not just a status check. A `success` envelope whose `result` is null is a shape
     Prometheus proxies and mocks really produce, and `[...null]` throws — out of a render-time memo,
     past every empty-state check the callers do afterwards, and into the route error boundary, which
     only clears on navigation. lib/prom-table.ts and pages/promql-console.tsx already guard this
     way, each citing a page it took down. */
  const entries: MatrixSeriesEntry[] =
    res.status === "success" && res.data?.resultType === "matrix" && Array.isArray(res.data.result)
      ? (res.data.result as MatrixSeriesEntry[])
      : [];
  const fmt = chart.unit === "seconds" ? formatSeconds : formatRatio;
  const colors = chartColors(dark ? "dark" : "light");

  // Stable colour assignment: Prometheus returns a matrix in an arbitrary order per poll (the
  // ranked queries above no more than any other).
  const sorted = [...entries].sort((a, b) =>
    labelForMetric(a.metric).localeCompare(labelForMetric(b.metric)),
  );

  /* The DISPLAY prefix the legend drops; the series names themselves stay whole
     (identity for legend selection, and what both tooltips print). */
  const namePrefix = legendNamePrefix(sorted.map((e) => labelForMetric(e.metric)));

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
      /* Display only: the fleet prefix goes, the distinguishing suffix stays,
         and the full name is one hover away on the entry itself. */
      formatter: (name: string) => elideSeriesName(name, namePrefix),
      tooltip: { show: true },
    },
    tooltip: {
      trigger: "axis",
      /* Type set by lib/chart-tooltip.ts; the colour is this chart's own. */
      axisPointer: { lineStyle: { color: colors.grid } },
      valueFormatter: (value) => fmt(Number(value)),
    },
    xAxis: {
      type: "time",
      /* The asked-for span, when the caller knows it. Without it ECharts fits
         the axis to the data and a window with no samples in it silently
         disappears from the picture. */
      ...(window ? { min: window.start.getTime(), max: window.end.getTime() } : {}),
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
        /* A line of ONE point draws nothing at all with symbols off — a chart
           that has data and looks empty. A 15m window over a metric scraped
           every five minutes gives exactly that, and so does any step the
           reader picks larger than half the range. */
        showSymbol: (entry.values ?? []).length < 2,
        smooth: false,
        color: seriesColor(colors, i),
        lineStyle: { width: 2 },
        data: (entry.values ?? []).map(([ts, v]) => [ts * 1000, Number(v)]),
      }),
    ),
  };
}
