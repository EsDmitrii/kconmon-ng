import type * as echarts from "echarts";
import { describe, expect, it } from "vitest";
import { CURATED_CHARTS, toSeriesOption } from "./curated-metrics";
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

  it("formats seconds-unit values as ms on the y axis", () => {
    const option = toSeriesOption(tcpP95, matrixResult, false);
    const yAxis = option.yAxis as echarts.YAXisComponentOption;
    const formatter = yAxis.axisLabel?.formatter as (value: number, index: number) => string;
    expect(formatter(0.215, 0)).toBe("215ms");
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
