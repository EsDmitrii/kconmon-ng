import { describe, expect, it } from "vitest";
import { CHART_FALLBACK, chartColors, seriesColor } from "./chart-theme";
import { CURATED_CHARTS, toSeriesOption } from "./curated-metrics";
import type { PromResult } from "./types";

describe("chartColors", () => {
  it("falls back to the documented token values when CSS vars are absent (jsdom)", () => {
    // jsdom resolves custom properties to "", so this exercises the fallback.
    expect(chartColors("dark")).toEqual(CHART_FALLBACK.dark);
    expect(chartColors("light")).toEqual(CHART_FALLBACK.light);
  });

  it("folds a 6th+ series into the muted other colour instead of cycling", () => {
    const colors = CHART_FALLBACK.dark;
    expect(seriesColor(colors, 0)).toBe(colors.series[0]);
    expect(seriesColor(colors, 4)).toBe(colors.series[4]);
    expect(seriesColor(colors, 5)).toBe(colors.other);
    expect(seriesColor(colors, 17)).toBe(colors.other);
  });
});

describe("toSeriesOption chart geometry", () => {
  const res: PromResult = {
    status: "success",
    data: {
      resultType: "matrix",
      result: [
        // Arrival order is deliberately NOT alphabetical: colour assignment
        // must be stable across polls, i.e. by sorted label.
        { metric: { source_node: "zeta", destination_node: "b" }, values: [[1, "1"]] },
        { metric: { source_node: "alpha", destination_node: "b" }, values: [[1, "2"]] },
      ],
    },
  };

  it("reserves the bottom band for a scrollable legend", () => {
    const opt = toSeriesOption(CURATED_CHARTS[0], res, true);
    const legend = opt.legend as { bottom: number; type: string };
    expect(legend.bottom).toBe(0);
    expect(legend.type).toBe("scroll");
    const grid = opt.grid as { bottom: number; top: number };
    expect(grid.bottom).toBeGreaterThanOrEqual(40); // legend space, no y-axis collision
  });

  it("assigns ramp colours by sorted label, not arrival order", () => {
    const opt = toSeriesOption(CURATED_CHARTS[0], res, true);
    const series = opt.series as { name: string; color: string }[];
    expect(series[0].name).toBe("alpha→b");
    expect(series[0].color).toBe(CHART_FALLBACK.dark.series[0]);
    expect(series[1].name).toBe("zeta→b");
    expect(series[1].color).toBe(CHART_FALLBACK.dark.series[1]);
  });
});
