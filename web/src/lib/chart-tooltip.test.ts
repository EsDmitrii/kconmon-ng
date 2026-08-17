import { describe, expect, it } from "vitest";
import type * as echarts from "echarts";
import {
  GAP,
  TOOLTIP_ROW_CAP,
  VIEWPORT_MARGIN,
  capTooltipRows,
  clampTooltipOption,
  placeTooltip,
  renderAxisTooltip,
  rowValue,
  sharedTooltipOption,
  type AxisTooltipRow,
} from "./chart-tooltip";

/**
 * The clipping the owner reported: hovering a worst-5 panel near the left of the
 * screen cut the pair names off ("…nmon-prod-m03").
 *
 * ECharts' own edge fix measures the tooltip against the CHART box (api.getWidth
 * / getHeight), not against the window, so a tooltip wider than a half-width
 * panel gets placed at a negative offset — off the panel, and off the screen.
 * placeTooltip is the replacement: it clamps against the VIEWPORT, flips sides
 * first and only shifts when flipping is not enough.
 *
 * Coordinates are HOST-RELATIVE both in and out, which is the contract ECharts'
 * `position` callback works in.
 */

/** A half-width panel sitting near the left edge of a 1280px window. */
const host = { left: 24, top: 200, width: 420, height: 264 };
const viewport = { width: 1280, height: 800 };

/** A tooltip as wide as five pair names make it. */
const content: [number, number] = [360, 140];

const place = (point: [number, number], over: Partial<Parameters<typeof placeTooltip>[0]> = {}) =>
  placeTooltip({ point, content, host, viewport, ...over });

/** The tooltip's box in VIEWPORT coordinates, which is where clipping happens. */
function onScreen(point: [number, number], over: Partial<Parameters<typeof placeTooltip>[0]> = {}) {
  const [x, y] = place(point, over);
  const h = { ...host, ...(over.host ?? {}) };
  const c = over.content ?? content;
  return { left: h.left + x, top: h.top + y, right: h.left + x + c[0], bottom: h.top + y + c[1] };
}

describe("placeTooltip keeps the tooltip on screen", () => {
  it("sits to the RIGHT of the cursor when there is room", () => {
    const [x] = place([60, 100]);
    expect(x).toBe(60 + GAP);
  });

  it("FLIPS to the left rather than running off the right of the window", () => {
    // Cursor near the panel's right edge: right-side placement would end at
    // 24 + 400 + 12 + 360 = 796 inside the window, so widen the host instead.
    const wide = { left: 880, top: 200, width: 380, height: 264 };
    const [x] = place([340, 100], { host: wide });
    expect(x).toBe(340 - GAP - content[0]);
    expect(onScreen([340, 100], { host: wide }).right).toBeLessThanOrEqual(viewport.width - VIEWPORT_MARGIN);
  });

  it("never lets the LEFT edge go off screen — the reported cut", () => {
    // The owner's case: a 360px tooltip, a cursor 40px into a panel that starts
    // 24px from the window's left edge. Flipping left lands at -332.
    const box = onScreen([40, 100]);
    expect(box.left).toBeGreaterThanOrEqual(VIEWPORT_MARGIN);
    expect(box.right).toBeLessThanOrEqual(viewport.width - VIEWPORT_MARGIN);
  });

  it("SHIFTS when neither side fits, instead of picking the lesser clipping", () => {
    // A tooltip wider than the space on either side of the cursor: no flip can
    // help, so it slides into the window and stays whole.
    const narrow = { width: 420, height: 800 };
    const box = onScreen([200, 100], { viewport: narrow, host: { left: 8, top: 0, width: 404, height: 264 } });
    expect(box.left).toBeGreaterThanOrEqual(VIEWPORT_MARGIN);
    expect(box.right).toBeLessThanOrEqual(narrow.width - VIEWPORT_MARGIN);
  });

  it("keeps a tooltip taller than the panel inside the window vertically", () => {
    const box = onScreen([200, 250], { content: [200, 600] });
    expect(box.top).toBeGreaterThanOrEqual(VIEWPORT_MARGIN);
    expect(box.bottom).toBeLessThanOrEqual(viewport.height - VIEWPORT_MARGIN);
  });

  it("gives up gracefully — clamped, never negative — for a tooltip wider than the window", () => {
    const box = onScreen([200, 100], { content: [2000, 140] });
    expect(box.left).toBe(VIEWPORT_MARGIN);
  });
});

describe("placeTooltip never covers the point being read", () => {
  /** Does the placed box contain the cursor itself? */
  function covers(point: [number, number], over: Partial<Parameters<typeof placeTooltip>[0]> = {}) {
    const [x, y] = place(point, over);
    const c = over.content ?? content;
    return point[0] >= x && point[0] <= x + c[0] && point[1] >= y && point[1] <= y + c[1];
  }

  it("clears the cursor when it sits to the right", () => {
    expect(covers([60, 100])).toBe(false);
  });

  it("clears the cursor when it had to flip", () => {
    expect(covers([340, 100], { host: { left: 880, top: 200, width: 380, height: 264 } })).toBe(false);
  });

  it("clears the cursor VERTICALLY when a horizontal shift would have covered it", () => {
    // Narrow window, wide tooltip: the box has to overlap the cursor's column,
    // so it moves off its row instead.
    expect(
      covers([200, 100], { viewport: { width: 420, height: 800 }, host: { left: 8, top: 0, width: 404, height: 264 } }),
    ).toBe(false);
  });
});

describe("clampTooltipOption wires the clamp into an option", () => {
  const hostEl = () => null;

  it("adds a position callback and escapes the panel's own clipping", () => {
    const out = clampTooltipOption({ tooltip: { trigger: "axis" } }, hostEl);
    const tip = out.tooltip as { position?: unknown; appendTo?: unknown; confine?: unknown };
    expect(typeof tip.position).toBe("function");
    // appendTo lifts the tooltip out of the Card, whose overflow was the second
    // half of the clipping; confine would have re-imposed the chart box.
    expect(tip.appendTo).toBe("body");
    expect(tip.confine).toBe(false);
  });

  it("leaves a chart that asked for NO tooltip alone", () => {
    const option = { series: [] };
    expect(clampTooltipOption(option, hostEl)).toBe(option);
  });

  it("keeps whatever the caller already configured", () => {
    const formatter = () => "x";
    const out = clampTooltipOption({ tooltip: { trigger: "axis", valueFormatter: formatter } }, hostEl);
    expect((out.tooltip as { trigger?: string }).trigger).toBe("axis");
    expect((out.tooltip as { valueFormatter?: unknown }).valueFormatter).toBe(formatter);
  });

  it("falls back to the chart's own box when the host has not measured yet", () => {
    const out = clampTooltipOption({ tooltip: {} }, hostEl);
    const position = (out.tooltip as { position: (...a: unknown[]) => [number, number] }).position;
    const at = position([50, 60], {}, undefined, undefined, { contentSize: [100, 50], viewSize: [420, 264] });
    expect(at[0]).toBe(50 + GAP);
  });
});

/* ── the tooltip that tried to name ninety-nine series ───────────────────── */

/**
 * The owner's report: `up` on the PromQL console matches ~99 series, and the
 * hover tooltip listed every one of them, covering the screen. A tooltip is a
 * glance; the full listing is the Table and Raw views under the chart.
 */

const seriesRow = (name: string, y: number): AxisTooltipRow => ({
  marker: `<span data-m="${name}"></span>`,
  seriesName: name,
  axisValueLabel: "12:00:00",
  value: [1_700_000_000_000, y],
});

describe("capTooltipRows", () => {
  it("hands a RICHER row back whole — the cursor readout ranks rows that carry more", () => {
    /* lib/chart-cursor.tsx ranks rows that also carry the series index and the
       sample they were read from, and it has to get those back to place a dot.
       A helper that projected them down to marker/name/value would have made
       the readout keep a parallel list and match it up by name, which two
       series with the same label would have broken. */
    const rich = Array.from({ length: 12 }, (_, i) => ({ ...seriesRow(`s${i}`, i), index: i }));
    const capped = capTooltipRows(rich, null, 3);
    expect(capped.rows.map((r) => r.index)).toEqual([11, 10, 9]);
    expect(capped.hidden).toBe(9);
  });

  it("leaves a short list exactly as the chart drew it — legend order is an order", () => {
    const rows = [seriesRow("a", 3), seriesRow("b", 1), seriesRow("c", 2)];
    const capped = capTooltipRows(rows, null, TOOLTIP_ROW_CAP);
    expect(capped.hidden).toBe(0);
    expect(capped.rows.map((r) => r.seriesName)).toEqual(["a", "b", "c"]);
  });

  it("keeps the rows NEAREST the cursor when the pointer's own value is known", () => {
    const rows = [seriesRow("far", 100), seriesRow("near", 51), seriesRow("mid", 70)];
    const capped = capTooltipRows(rows, 50, 2);
    expect(capped.rows.map((r) => r.seriesName)).toEqual(["near", "mid"]);
    expect(capped.hidden).toBe(1);
  });

  it("falls back to the largest values when there is no cursor to be near", () => {
    const rows = [seriesRow("small", 1), seriesRow("big", 90), seriesRow("mid", 40)];
    expect(capTooltipRows(rows, null, 2).rows.map((r) => r.seriesName)).toEqual(["big", "mid"]);
  });

  it("counts EXACTLY what it left out — the whole point of the line it prints", () => {
    const rows = Array.from({ length: 99 }, (_, i) => seriesRow(`s${i}`, i));
    const capped = capTooltipRows(rows, null, TOOLTIP_ROW_CAP);
    expect(capped.rows).toHaveLength(10);
    expect(capped.hidden).toBe(89);
  });

  it("sinks a row with no number rather than dropping it silently", () => {
    const rows = [{ seriesName: "gap", value: [1, null] }, seriesRow("real", 5), seriesRow("also", 6)];
    expect(capTooltipRows(rows, null, 2).rows.map((r) => r.seriesName)).toEqual(["also", "real"]);
  });

  it("is STABLE across ties, so a mousemove does not reshuffle the bubble", () => {
    const rows = [seriesRow("a", 7), seriesRow("b", 7), seriesRow("c", 7), seriesRow("d", 7)];
    expect(capTooltipRows(rows, 7, 2).rows.map((r) => r.seriesName)).toEqual(["a", "b"]);
  });
});

describe("rowValue", () => {
  it("reads the y out of a time-axis pair and a bare value alike", () => {
    expect(rowValue({ value: [1_700_000_000_000, 42] })).toBe(42);
    expect(rowValue({ value: 42 })).toBe(42);
    expect(rowValue({ value: "42" })).toBe(42);
  });

  it("is null for anything that is not a number, rather than NaN in a sort", () => {
    expect(rowValue({ value: undefined })).toBeNull();
    expect(rowValue({ value: [1, null] })).toBeNull();
    expect(rowValue({ value: "n/a" })).toBeNull();
  });
});

describe("renderAxisTooltip", () => {
  const more = (n: number) => `+${n} more`;

  it("prints the axis label, a row per series and nothing else when nothing was cut", () => {
    const html = renderAxisTooltip([seriesRow("node-a", 1.5)], 0, "12:00:00", more);
    expect(html).toContain("12:00:00");
    expect(html).toContain("node-a");
    expect(html).toContain("1.5");
    expect(html).not.toContain("more");
  });

  it("says how many it left out, in the caller's own words", () => {
    const html = renderAxisTooltip([seriesRow("node-a", 1)], 89, "12:00:00", more);
    expect(html).toContain("+89 more");
  });

  it("honours the surface's valueFormatter — a hop table's milliseconds stay milliseconds", () => {
    const html = renderAxisTooltip([seriesRow("hop-1", 2.5)], 0, "12:00", more, (v) => `${v}ms`);
    expect(html).toContain("2.5ms");
  });

  it("escapes a series name, which on a free-query surface is somebody else's text", () => {
    const row: AxisTooltipRow = { seriesName: '<img src=x onerror="boom">', value: [1, 1] };
    const html = renderAxisTooltip([row], 0, "<b>12:00</b>", more);
    expect(html).not.toContain("<img");
    expect(html).toContain("&lt;img");
    expect(html).not.toContain("<b>12:00</b>");
  });
});

describe("sharedTooltipOption", () => {
  const base: echarts.EChartsOption = {
    xAxis: { type: "time" },
    yAxis: { type: "value" },
    tooltip: { trigger: "axis", axisPointer: { type: "line", lineStyle: { color: "#333" } } },
    series: [],
  };
  const wire = (option: echarts.EChartsOption, cursor: number | null = null) =>
    sharedTooltipOption(option, () => null, { cursorValue: () => cursor, more: (n) => `+${n} more` });

  it("gives the HOVERED chart a cross pointer, so its y is readable without counting gridlines", () => {
    const tooltip = wire(base).tooltip as { axisPointer: { type: string; crossStyle?: unknown } };
    expect(tooltip.axisPointer.type).toBe("cross");
    // The surface's own grid colour travels to the style a cross actually draws with.
    expect(tooltip.axisPointer.crossStyle).toEqual({ color: "#333" });
  });

  it("caps the rows, and composes with the placement clamp rather than replacing it", () => {
    const tooltip = wire(base, 5).tooltip as {
      formatter: (p: unknown) => string;
      position: unknown;
      appendTo: unknown;
    };
    const rows = Array.from({ length: 30 }, (_, i) => seriesRow(`s${i}`, i));
    const html = tooltip.formatter(rows);
    expect(html).toContain("+20 more");
    // ...and the clamp is still the thing placing it.
    expect(typeof tooltip.position).toBe("function");
    expect(tooltip.appendTo).toBe("body");
  });

  it("leaves an ITEM trigger alone — one point is not a list to cap", () => {
    const item = { ...base, tooltip: { trigger: "item" as const } };
    const tooltip = wire(item).tooltip as { formatter?: unknown };
    expect(tooltip.formatter).toBeUndefined();
  });

  it("returns a chart with no tooltip untouched, identity and all", () => {
    const option = { xAxis: { type: "time" as const }, series: [] };
    expect(wire(option)).toBe(option);
  });
});

/* ── the cross's y pill printed the raw number ───────────────────────────── */

/**
 * The owner's screenshot: the horizontal pointer's label read `0.00811` beside a
 * y axis whose own ticks read `8.1ms`. The pill is a reading of that axis, so it
 * takes that axis's formatter rather than a second opinion.
 */
describe("the y-axis pointer pill", () => {
  const wireOption = (option: echarts.EChartsOption) =>
    sharedTooltipOption(option, () => null, { cursorValue: () => null, more: (n) => `+${n} more` });

  const pillOf = (option: echarts.EChartsOption) =>
    (wireOption(option).yAxis as { axisPointer?: { label?: { formatter?: (p: { value: unknown }) => string } } })
      ?.axisPointer?.label?.formatter;

  it("takes the chart's own valueFormatter", () => {
    const pill = pillOf({
      xAxis: { type: "time" },
      yAxis: { type: "value" },
      tooltip: { trigger: "axis", valueFormatter: (v) => `${(Number(v) * 1000).toFixed(1)}ms` },
      series: [],
    });

    expect(pill?.({ value: 0.00811 })).toBe("8.1ms");
  });

  it("falls back to the y axis's OWN label formatter when the tooltip declares none", () => {
    const pill = pillOf({
      xAxis: { type: "time" },
      yAxis: { type: "value", axisLabel: { formatter: (v: number) => `${v}%` } },
      tooltip: { trigger: "axis" },
      series: [],
    });

    expect(pill?.({ value: 42 })).toBe("42%");
  });

  it("leaves a deliberately RAW axis alone — the PromQL console formats nothing", () => {
    const wired = wireOption({
      xAxis: { type: "time" },
      yAxis: { type: "value" },
      tooltip: { trigger: "axis" },
      series: [],
    });

    // No formatter invented: a pill claiming units the axis does not claim
    // would be worse than the bare number.
    expect((wired.yAxis as { axisPointer?: unknown }).axisPointer).toBeUndefined();
  });

  it("does not touch the X pill, which is an instant and not a latency", () => {
    const wired = wireOption({
      xAxis: { type: "time" },
      yAxis: { type: "value" },
      tooltip: { trigger: "axis", valueFormatter: (v) => `${v}ms` },
      series: [],
    });

    // The formatter rides on the Y AXIS. The tooltip's own axisPointer governs
    // BOTH pills, so a formatter there would rewrite the timestamp too.
    const pointer = (wired.tooltip as { axisPointer?: { label?: unknown } }).axisPointer;
    expect(pointer?.label).toBeUndefined();
  });

  it("leaves a MULTI-axis chart alone — there is no single y value to format", () => {
    const wired = wireOption({
      xAxis: { type: "time" },
      yAxis: [{ type: "value" }, { type: "value" }],
      tooltip: { trigger: "axis", valueFormatter: (v) => `${v}ms` },
      series: [],
    });

    expect(Array.isArray(wired.yAxis)).toBe(true);
    expect((wired.yAxis as { axisPointer?: unknown }[])[0].axisPointer).toBeUndefined();
  });
});
