import type * as echarts from "echarts";
import { describe, expect, it } from "vitest";
import { CURATED_CHARTS, RANGE_TOKEN, elideSeriesName, legendNamePrefix, resolveRangeToken, toSeriesOption } from "./curated-metrics";
import type { PromResult } from "./types";

// Full metric-name inventory from docs/metrics.md (also verified against
// internal/metrics/prometheus.go). Every CURATED_CHARTS query must reference
// only names from this list — guards against typos in hand-written PromQL.
const ALLOWED_METRICS = [
  "kconmon_ng_tcp_connect_duration_seconds",
  "kconmon_ng_tcp_total_duration_seconds",
  "kconmon_ng_tcp_results_total",
  "kconmon_ng_udp_rtt_seconds",
  "kconmon_ng_udp_jitter_seconds",
  "kconmon_ng_udp_packet_loss_ratio",
  "kconmon_ng_udp_results_total",
  "kconmon_ng_icmp_rtt_seconds",
  "kconmon_ng_icmp_packet_loss_ratio",
  "kconmon_ng_icmp_results_total",
  "kconmon_ng_dns_duration_seconds",
  "kconmon_ng_dns_results_total",
  "kconmon_ng_http_dns_duration_seconds",
  "kconmon_ng_http_connect_duration_seconds",
  "kconmon_ng_http_tls_duration_seconds",
  "kconmon_ng_http_ttfb_seconds",
  "kconmon_ng_http_total_duration_seconds",
  "kconmon_ng_http_results_total",
  "kconmon_ng_mtr_triggered_total",
  "kconmon_ng_mtr_hops",
  "kconmon_ng_mtr_hop_rtt_seconds",
  "kconmon_ng_controller_registered_agents",
  "kconmon_ng_controller_expected_agents",
  "kconmon_ng_controller_grpc_connections",
  "kconmon_ng_controller_peer_updates_total",
  "kconmon_ng_controller_leader",
];

// Histogram queries reference the `_bucket` suffix (and could reference
// `_sum`/`_count`), which aren't metric names in their own right.
function baseMetricNamesIn(query: string): string[] {
  const matches = query.match(/kconmon_ng_[a-z_]+/g) ?? [];
  return [...new Set(matches.map((m) => m.replace(/_bucket$|_sum$|_count$/, "")))];
}

describe("CURATED_CHARTS", () => {
  it("references only known metric names", () => {
    for (const chart of CURATED_CHARTS) {
      for (const name of baseMetricNamesIn(chart.query)) {
        expect(ALLOWED_METRICS, `${chart.id}: unknown metric ${name}`).toContain(name);
      }
    }
  });

  it("has the corrected fail-rate query, not the flagged sketch", () => {
    const failRate = CURATED_CHARTS.find((c) => c.id === "fail-rate")!;
    expect(failRate.query).toBe(
      'sum by (protocol) (label_replace(rate(kconmon_ng_tcp_results_total{result="fail"}[5m]), "protocol", "tcp", "", "") or label_replace(rate(kconmon_ng_udp_results_total{result="fail"}[5m]), "protocol", "udp", "", "") or label_replace(rate(kconmon_ng_icmp_results_total{result="fail"}[5m]), "protocol", "icmp", "", ""))',
    );
    expect(failRate.query).not.toContain("vector(0)");
  });

  /* ---------------------------------------------------------------- */
  /* The topk-in-a-range-query shape                                   */
  /* ---------------------------------------------------------------- */

  /* A bare `topk(N, …)` at the head of a RANGE query is re-evaluated at every
     step, so the chart draws the union of everything that ever led — on a
     10-node mesh that is up to 90 pairs, each a stub. The corrected shape
     picks the N once, pinned at the window's end, and uses it only as a
     membership filter. These tests pin that shape so it cannot regress into a
     leading topk again. */
  const rankedCharts = () => CURATED_CHARTS.filter((c) => c.query.includes("topk("));

  it("never leads a range query with a bare topk", () => {
    for (const chart of CURATED_CHARTS) {
      expect(chart.query.trimStart(), `${chart.id}: bare leading topk`).not.toMatch(/^topk\s*\(/);
    }
  });

  it("uses topk only as an `and on` membership filter, pinned with @ end()", () => {
    expect(rankedCharts().length).toBeGreaterThan(0);
    for (const chart of rankedCharts()) {
      // Exactly one topk, on the right of `and on` — the drawn samples come
      // from the left-hand series, never from the ranker.
      expect(chart.query.match(/topk\(/g), `${chart.id}: more than one topk`).toHaveLength(1);
      expect(chart.query, `${chart.id}: topk not joined with and on`).toContain(
        "and on (source_node, destination_node) topk(",
      );
      expect(chart.query, `${chart.id}: ranker not pinned to the window end`).toContain(
        `[${RANGE_TOKEN}] @ end()`,
      );
    }
  });

  it("bounds every worst-5 chart at the 5 its title promises", () => {
    const worst5 = CURATED_CHARTS.filter((c) => /worst 5/i.test(c.title));
    expect(worst5.map((c) => c.id)).toEqual(["tcp-p95", "udp-loss", "icmp-p95"]);
    for (const chart of worst5) {
      expect(chart.query, `${chart.id}: not a topk(5) chart`).toContain("topk(5,");
    }
  });

  it("leaves no unresolved token in anything the page can post", () => {
    for (const chart of CURATED_CHARTS) {
      expect(resolveRangeToken(chart.query, 3600), `${chart.id}: token survived resolution`).not.toContain("$");
    }
  });
});

describe("resolveRangeToken", () => {
  it("substitutes the drawn window as a PromQL duration", () => {
    expect(resolveRangeToken(`rate(x[${RANGE_TOKEN}] @ end())`, 6 * 60 * 60)).toBe("rate(x[21600s] @ end())");
  });

  it("replaces every occurrence, not just the first", () => {
    expect(resolveRangeToken(`a[${RANGE_TOKEN}] + b[${RANGE_TOKEN}]`, 900)).toBe("a[900s] + b[900s]");
  });

  it("leaves a query without the token untouched", () => {
    const plain = "rate(kconmon_ng_tcp_results_total[5m])";
    expect(resolveRangeToken(plain, 900)).toBe(plain);
  });

  it("never emits a fractional or zero duration Prometheus would reject", () => {
    expect(resolveRangeToken(`x[${RANGE_TOKEN}]`, 90.7)).toBe("x[91s]");
    expect(resolveRangeToken(`x[${RANGE_TOKEN}]`, 0)).toBe("x[1s]");
    expect(resolveRangeToken(`x[${RANGE_TOKEN}]`, -5)).toBe("x[1s]");
  });
});

describe("toSeriesOption", () => {
  const tcpP95 = CURATED_CHARTS.find((c) => c.id === "tcp-p95")!;

  const matrixResult: PromResult = {
    status: "success",
    data: {
      resultType: "matrix",
      result: [
        { metric: { source_node: "a", destination_node: "b" }, values: [[1700000000, "0.215"], [1700000030, "0.3"]] },
        { metric: { source_node: "c", destination_node: "d" }, values: [[1700000000, "0.05"]] },
      ],
    },
  };

  it("maps one line series per result entry with peer-pair naming", () => {
    const option = toSeriesOption(tcpP95, matrixResult, false);
    const series = option.series as echarts.LineSeriesOption[];

    expect(series).toHaveLength(2);
    expect(series[0].name).toBe("a→b");
    expect(series[0].type).toBe("line");
    expect(series[0].data).toEqual([
      [1700000000000, 0.215],
      [1700000030000, 0.3],
    ]);
    expect(series[1].name).toBe("c→d");
  });

  it("sets xAxis to a time axis and animation off", () => {
    const option = toSeriesOption(tcpP95, matrixResult, false);
    expect((option.xAxis as echarts.XAXisComponentOption).type).toBe("time");
    expect(option.animation).toBe(false);
  });

  /* The 24h and the 6h pick drew the SAME axis on a Prometheus holding six
     hours, so the range buttons looked like they only redrew the curves (owner
     report). The axis is the span that was asked for; the data covers what it
     covers, and the empty part is the answer. */
  it("pins the axis to the window it was given, not to the data it received", () => {
    const start = new Date(1699913600000); // a day before the samples above
    const end = new Date(1700000030000);
    const option = toSeriesOption(tcpP95, matrixResult, false, { start, end });
    const xAxis = option.xAxis as echarts.XAXisComponentOption;

    expect(xAxis.min).toBe(start.getTime());
    expect(xAxis.max).toBe(end.getTime());
    // The samples are untouched — pinning the axis moves no data.
    expect((option.series as echarts.LineSeriesOption[])[0].data).toEqual([
      [1700000000000, 0.215],
      [1700000030000, 0.3],
    ]);
  });

  it("leaves the axis to ECharts when no window is given, so other callers are unchanged", () => {
    const xAxis = toSeriesOption(tcpP95, matrixResult, false).xAxis as echarts.XAXisComponentOption;
    expect(xAxis.min).toBeUndefined();
    expect(xAxis.max).toBeUndefined();
  });

  it("formats seconds-unit values as ms on the y axis", () => {
    const option = toSeriesOption(tcpP95, matrixResult, false);
    const yAxis = option.yAxis as echarts.YAXisComponentOption;
    const formatter = yAxis.axisLabel?.formatter as (value: number, index: number) => string;
    expect(formatter(0.215, 0)).toBe("215ms");
  });

  /* One decimal below 10ms separates them; above it the decimal is noise. */
  it("switches to one decimal below 10ms so adjacent ticks stop colliding", () => {
    const option = toSeriesOption(tcpP95, matrixResult, false);
    const yAxis = option.yAxis as echarts.YAXisComponentOption;
    const formatter = yAxis.axisLabel?.formatter as (value: number, index: number) => string;
    expect(formatter(0.0012, 0)).toBe("1.2ms");
    expect(formatter(0.0018, 0)).toBe("1.8ms");
    // The boundary itself is the integer side.
    expect(formatter(0.01, 0)).toBe("10ms");
    expect(formatter(0.0099, 0)).toBe("9.9ms");
  });

  it("lets ECharts thin out colliding time-axis labels", () => {
    const option = toSeriesOption(tcpP95, matrixResult, false);
    const xAxis = option.xAxis as echarts.XAXisComponentOption;
    expect(xAxis.axisLabel?.hideOverlap).toBe(true);
  });

  it("formats ratio-unit values as a percent on the y axis", () => {
    const udpLoss = CURATED_CHARTS.find((c) => c.id === "udp-loss")!;
    const option = toSeriesOption(udpLoss, matrixResult, false);
    const yAxis = option.yAxis as echarts.YAXisComponentOption;
    const formatter = yAxis.axisLabel?.formatter as (value: number, index: number) => string;
    expect(formatter(0.02, 0)).toBe("2.0%");
  });

  it("names series by host when source/destination labels are absent", () => {
    const dnsP95 = CURATED_CHARTS.find((c) => c.id === "dns-p95")!;
    const hostResult: PromResult = {
      status: "success",
      data: { resultType: "matrix", result: [{ metric: { host: "example.com" }, values: [[0, "0.01"]] }] },
    };
    const series = toSeriesOption(dnsP95, hostResult, true).series as echarts.LineSeriesOption[];
    expect(series[0].name).toBe("example.com");
  });

  it("names series by protocol when only that label is present", () => {
    const failRate = CURATED_CHARTS.find((c) => c.id === "fail-rate")!;
    const protoResult: PromResult = {
      status: "success",
      data: { resultType: "matrix", result: [{ metric: { protocol: "tcp" }, values: [[0, "0.02"]] }] },
    };
    const series = toSeriesOption(failRate, protoResult, true).series as echarts.LineSeriesOption[];
    expect(series[0].name).toBe("tcp");
  });

  it("returns no series when the PromResult is a Prometheus error envelope", () => {
    const errorResult: PromResult = { status: "error", errorType: "bad_data", error: "parse error" };
    expect(toSeriesOption(tcpP95, errorResult, false).series).toEqual([]);
  });
});

/* ── M3-6: legend legibility on a real fleet ──────────────────────────────
   Real node names share a long cluster prefix, so a scroll legend printed
   "adm-kuber-0…" five times over and paged through indistinguishable entries.
   The DISPLAY drops the shared prefix down to the distinguishing suffix; the
   series NAME stays whole — it is the identity the axis tooltip, the legend
   tooltip and legend selection all key on. */
describe("legend elision (M3-6)", () => {
  const tcpP95 = CURATED_CHARTS.find((c) => c.id === "tcp-p95")!;

  const fleetResult: PromResult = {
    status: "success",
    data: {
      resultType: "matrix",
      result: [
        {
          metric: { source_node: "adm-kuber-01", destination_node: "adm-kuber-02" },
          values: [[1700000000, "0.2"]],
        },
        {
          metric: { source_node: "adm-kuber-03", destination_node: "adm-kuber-04" },
          values: [[1700000000, "0.3"]],
        },
      ],
    },
  };

  type Legend = {
    formatter?: (name: string) => string;
    tooltip?: { show?: boolean };
  };

  it("computes the shared name prefix over the arrow-separated halves", () => {
    expect(legendNamePrefix(["adm-kuber-01→adm-kuber-02", "adm-kuber-03→adm-kuber-04"])).toBe("adm-kuber-");
    // Names that agree on no separator-terminated prefix stay whole.
    expect(legendNamePrefix(["a→b", "c→d"])).toBe("");
    expect(legendNamePrefix(["tcp", "udp", "icmp"])).toBe("");
    // A single host name has nothing to compare against.
    expect(legendNamePrefix(["example.com"])).toBe("");
  });

  it("elides each half down to its distinguishing suffix, display only", () => {
    expect(elideSeriesName("adm-kuber-01→adm-kuber-02", "adm-kuber-")).toBe("…01→…02");
    // A compare-panel name keeps its leg label and still elides the node halves.
    expect(elideSeriesName("A: TCP RTT p95 · adm-kuber-01→adm-kuber-02", "adm-kuber-")).toBe(
      "A: TCP RTT p95 · …01→…02",
    );
    expect(elideSeriesName("a→b", "")).toBe("a→b");
  });

  it("wires the elision into the legend and keeps the full name as identity", () => {
    const option = toSeriesOption(tcpP95, fleetResult, false);
    const series = option.series as echarts.LineSeriesOption[];
    expect(series.map((s) => s.name)).toEqual(["adm-kuber-01→adm-kuber-02", "adm-kuber-03→adm-kuber-04"]);

    const legend = option.legend as Legend;
    expect(legend.formatter?.("adm-kuber-01→adm-kuber-02")).toBe("…01→…02");
    // The full name stays one hover away, on the legend entry itself.
    expect(legend.tooltip?.show).toBe(true);
  });

  it("leaves a legend without a qualifying shared prefix untouched", () => {
    const shortResult: PromResult = {
      status: "success",
      data: {
        resultType: "matrix",
        result: [
          { metric: { source_node: "a", destination_node: "b" }, values: [[0, "1"]] },
          { metric: { source_node: "c", destination_node: "d" }, values: [[0, "2"]] },
        ],
      },
    };
    const legend = toSeriesOption(tcpP95, shortResult, false).legend as Legend;
    expect(legend.formatter?.("a→b")).toBe("a→b");
  });
});
