import { describe, expect, it } from "vitest";
import type * as echarts from "echarts";
import {
  CURATED_CHARTS,
  formatMillis,
  formatSeconds,
  resolveRangeToken,
  toSeriesOption,
} from "./curated-metrics";
import type { PromResult } from "./types";

/**
 * curated-metrics.hostile.test.ts — what Prometheus actually answers with.
 *
 * Three of the five curated charts are `histogram_quantile` over a rate, and
 * that expression returns NaN whenever the window holds no observations — a
 * quiet pair, a target that just came up, a 15m window over a 5m rate. It also
 * returns +Inf when the top bucket is the only one that filled. Both used to
 * reach a y-axis tick and a tooltip row as the literal text "NaNms".
 */

const SECONDS = CURATED_CHARTS[0];
const RATIO = CURATED_CHARTS[1];

const matrix = (values: [number, string][], metric: Record<string, string> = { host: "h" }): PromResult => ({
  status: "success",
  data: { resultType: "matrix", result: [{ metric, values }] },
});

type Opt = echarts.EChartsOption & {
  yAxis: { axisLabel: { formatter: (v: number) => string } };
  tooltip: { valueFormatter: (v: unknown) => string };
  series: (echarts.LineSeriesOption & { showSymbol?: boolean })[];
};

describe("no curated chart ever renders NaN or Infinity", () => {
  it("formats a non-finite seconds value as the console's own missing-value dash", () => {
    for (const bad of [Number.NaN, Number.POSITIVE_INFINITY, Number.NEGATIVE_INFINITY]) {
      expect(formatSeconds(bad), String(bad)).toBe("—");
      expect(formatMillis(bad), String(bad)).toBe("—");
    }
  });

  it("keeps every finite reading exactly as it was", () => {
    expect(formatSeconds(0.0081)).toBe("8.1ms");
    expect(formatSeconds(0.25)).toBe("250ms");
    expect(formatSeconds(0)).toBe("0.0ms");
    expect(formatMillis(-3.24)).toBe("-3.2ms");
  });

  it("carries the same rule into the y-axis ticks and the tooltip of a seconds chart", () => {
    const opt = toSeriesOption(SECONDS, matrix([[1, "NaN"]]), true) as Opt;
    expect(opt.yAxis.axisLabel.formatter(Number.NaN)).toBe("—");
    expect(opt.tooltip.valueFormatter(Number.NaN)).toBe("—");
    expect(opt.tooltip.valueFormatter("+Inf")).toBe("—");
  });

  it("and into a ratio chart, where a missing loss figure is not a zero loss figure", () => {
    const opt = toSeriesOption(RATIO, matrix([[1, "NaN"]]), true) as Opt;
    expect(opt.yAxis.axisLabel.formatter(Number.NaN)).toBe("—");
    // The distinction that matters: a real zero still reads as a real zero.
    expect(opt.yAxis.axisLabel.formatter(0)).toBe("0.0%");
  });
});

describe("a series with too few points is still visible", () => {
  /* step > range on the Console, and a 15m window on a metric scraped every
     five, both produce a one-point series. With showSymbol off across the board
     a line of one point draws NOTHING: a chart that has data and looks empty. */
  it("draws the marker for a one-point series", () => {
    const opt = toSeriesOption(SECONDS, matrix([[1, "0.01"]]), true) as Opt;
    expect(opt.series[0].showSymbol).toBe(true);
  });

  it("leaves a normal series a clean line", () => {
    const many: [number, string][] = Array.from({ length: 50 }, (_, i) => [i, "0.01"]);
    const opt = toSeriesOption(SECONDS, matrix(many), true) as Opt;
    expect(opt.series[0].showSymbol).toBe(false);
  });
});

describe("toSeriesOption survives a malformed envelope", () => {
  const bad: [string, PromResult][] = [
    ["error envelope", { status: "error", errorType: "bad_data", error: "boom" }],
    ["no data at all", { status: "success" }],
    ["a vector where a matrix was asked for", { status: "success", data: { resultType: "vector", result: [] } }],
    ["an entry with no values", { status: "success", data: { resultType: "matrix", result: [{ metric: {} }] } }],
    ["an entry with no metric", { status: "success", data: { resultType: "matrix", result: [{ values: [] }] } }],
  ];

  for (const [name, res] of bad) {
    it(`returns a drawable option for ${name}`, () => {
      const opt = toSeriesOption(SECONDS, res, true) as Opt;
      expect(Array.isArray(opt.series)).toBe(true);
      for (const s of opt.series) expect(typeof s.name).toBe("string");
    });
  }
});

describe("resolveRangeToken cannot emit an invalid PromQL duration", () => {
  it("substitutes the window everywhere the token appears", () => {
    expect(resolveRangeToken("a[$__range] + b[$__range]", 3600)).toBe("a[3600s] + b[3600s]");
  });

  it("falls back to a legal duration for a nonsense window rather than sending `[NaNs]`", () => {
    for (const bad of [Number.NaN, Number.POSITIVE_INFINITY, -5, 0]) {
      expect(resolveRangeToken("a[$__range]", bad), String(bad)).toBe("a[1s]");
    }
  });

  it("leaves a query with no token completely alone", () => {
    const q = 'up{pod="x"}';
    expect(resolveRangeToken(q, 3600)).toBe(q);
  });
});

/* ── a success envelope that carries no array ────────────────────────────── */

/*
 * `{status:"success",data:{resultType:"matrix",result:null}}` is a shape this console has already
 * been taken down by twice — lib/prom-table.ts and pages/promql-console.tsx each guard it and each
 * cite the page it broke. toSeriesOption spread it unguarded, and `[...null]` throws out of a
 * render-time memo, past every caller's own empty-state check, into the route error boundary, which
 * by its own contract clears only on navigation: Explore could not be recovered without leaving it.
 */
describe("toSeriesOption survives a success envelope whose result is not an array", () => {
  const chart = CURATED_CHARTS[0];

  for (const [name, result] of [
    ["null", null],
    ["an object", { "0": { metric: {}, values: [] } }],
    ["a string", "matrix"],
    ["a number", 42],
  ] as const) {
    it(`renders an empty chart for result = ${name}`, () => {
      const res = { status: "success", data: { resultType: "matrix", result } } as unknown as PromResult;
      const option = toSeriesOption(chart, res, true) as echarts.EChartsOption;
      // No throw, and no series invented out of the malformed payload.
      const series = Array.isArray(option.series) ? option.series : option.series ? [option.series] : [];
      expect(series.filter((s) => Array.isArray((s as { data?: unknown[] }).data) && (s as { data: unknown[] }).data.length > 0)).toHaveLength(0);
    });
  }
});
