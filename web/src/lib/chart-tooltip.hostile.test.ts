import { describe, expect, it } from "vitest";
import {
  capTooltipRows,
  placeTooltip,
  renderAxisTooltip,
  rowValue,
  sharedTooltipOption,
  TOOLTIP_ROW_CAP,
  type AxisTooltipRow,
} from "./chart-tooltip";

/**
 * chart-tooltip.hostile.test.ts — the tooltip layer under a hostile result.
 *
 * A PromQL answer is not a well-behaved number series. `histogram_quantile` over
 * a bucket that never filled returns NaN; a range query over a window a target
 * was down for returns holes; ECharts hands a formatter `'-'` for an empty slot.
 * Every one of those reached the bubble as a NUMBER before it reached a reader,
 * and `Number(null)` is 0 — a gap that printed itself as a real zero reading.
 */

const row = (name: string, value: unknown): AxisTooltipRow => ({
  marker: "<i></i>",
  seriesName: name,
  axisValueLabel: "12:00:00",
  value: [1_700_000_000_000, value],
});

const more = (n: number) => `+${n} more`;
/** The curated charts' own formatter shape: seconds in, milliseconds out. */
const asMillis = (v: unknown) => `${(Number(v) * 1000).toFixed(1)}ms`;

describe("a tooltip row with no reading says so, instead of inventing a zero", () => {
  /* The owner's rule for the whole console: nothing renders NaN, and nothing
     renders a gap as data. `Number(null) === 0`, so a hole in a series used to
     print `0.0ms` — indistinguishable from a genuine zero-latency sample. */
  it("prints the em dash for a hole rather than running the formatter over null", () => {
    const html = renderAxisTooltip([row("node-a", null)], 0, "12:00:00", more, asMillis);
    expect(html).toContain("—");
    expect(html).not.toContain("0.0ms");
  });

  it("does the same for undefined, for ECharts' own '-' empty marker, and for an empty string", () => {
    for (const empty of [undefined, "-", ""]) {
      const html = renderAxisTooltip([row("node-a", empty)], 0, "12:00:00", more, asMillis);
      expect(html, `value=${JSON.stringify(empty)}`).toContain("—");
      expect(html, `value=${JSON.stringify(empty)}`).not.toMatch(/NaN|0\.0ms/);
    }
  });

  /* histogram_quantile with no observations in the window is NaN, and `+Inf`
     arrives as a non-finite number once the page has Number()'d it. Both used to
     reach the bubble as "NaNms" / "Infinityms". */
  it("never renders NaN or Infinity for a non-finite sample", () => {
    for (const bad of [Number.NaN, Number.POSITIVE_INFINITY, Number.NEGATIVE_INFINITY]) {
      const html = renderAxisTooltip([row("node-a", bad)], 0, "12:00:00", more, asMillis);
      expect(html, String(bad)).toContain("—");
      expect(html, String(bad)).not.toMatch(/NaN|Infinity/);
    }
  });

  it("still prints a genuine zero as a zero — the point of telling them apart", () => {
    const html = renderAxisTooltip([row("node-a", 0)], 0, "12:00:00", more, asMillis);
    expect(html).toContain("0.0ms");
    expect(html).not.toContain("—");
  });

  it("leaves a non-numeric label alone: not every axis carries numbers", () => {
    // The MTR hop strip's category rows carry text, and a formatter-less caller
    // must keep getting its own string back verbatim.
    const html = renderAxisTooltip([{ seriesName: "hop", value: "unreachable" }], 0, "hop 3", more);
    expect(html).toContain("unreachable");
  });

  it("escapes a series name that came out of a label value", () => {
    const html = renderAxisTooltip([row('up{pod="<img src=x onerror=alert(1)>"}', 1)], 0, "12:00", more);
    expect(html).not.toContain("<img");
    expect(html).toContain("&lt;img");
  });
});

describe("the row cap holds at every size", () => {
  const rowsFor = (n: number) => Array.from({ length: n }, (_, i) => row(`s${i}`, i));

  it("caps nothing at zero, one, and exactly the cap", () => {
    for (const n of [0, 1, TOOLTIP_ROW_CAP]) {
      const out = capTooltipRows(rowsFor(n), null);
      expect(out.rows, `n=${n}`).toHaveLength(n);
      expect(out.hidden, `n=${n}`).toBe(0);
    }
    // …and the first one over it does cap.
    expect(capTooltipRows(rowsFor(TOOLTIP_ROW_CAP + 1), null).hidden).toBe(1);
  });

  it("survives a thousand series and keeps the ones nearest the cursor", () => {
    const out = capTooltipRows(rowsFor(1000), 500);
    expect(out.rows).toHaveLength(TOOLTIP_ROW_CAP);
    expect(out.hidden).toBe(990);
    expect(out.rows[0].seriesName).toBe("s500");
  });

  it("sorts a valueless row last rather than treating it as the nearest", () => {
    const rows = [row("gap", null), ...Array.from({ length: 20 }, (_, i) => row(`s${i}`, i))];
    const out = capTooltipRows(rows, 0);
    // The gap is not the row closest to a cursor at 0 — s0 is.
    expect(out.rows[0].seriesName).toBe("s0");
    expect(out.rows.map((r) => r.seriesName)).not.toContain("gap");
  });

  it("reads rowValue off a NaN sample as 'no value', not as a number", () => {
    expect(rowValue(row("x", Number.NaN))).toBeNull();
    expect(rowValue(row("x", 0))).toBe(0);
  });
});

describe("placement stays inside the window at every corner", () => {
  const host = { left: 0, top: 0, width: 800, height: 400 };
  const viewport = { width: 1000, height: 800 };
  const content: [number, number] = [300, 200];

  it("never places the tooltip past either edge, wherever the cursor is", () => {
    for (const px of [0, 1, 400, 799, 800]) {
      for (const py of [0, 1, 200, 399, 400]) {
        const [x, y] = placeTooltip({ point: [px, py], content, host, viewport });
        // Host-relative, so viewport coordinates are x + host.left.
        expect(x + host.left, `${px},${py}`).toBeGreaterThanOrEqual(8);
        expect(x + host.left + content[0], `${px},${py}`).toBeLessThanOrEqual(viewport.width - 8);
        expect(y + host.top, `${px},${py}`).toBeGreaterThanOrEqual(8);
        expect(y + host.top + content[1], `${px},${py}`).toBeLessThanOrEqual(viewport.height - 8);
      }
    }
  });

  it("returns finite numbers even when the window is smaller than the bubble", () => {
    const [x, y] = placeTooltip({
      point: [10, 10],
      content: [400, 2000],
      host: { left: 0, top: 0, width: 300, height: 200 },
      viewport: { width: 320, height: 500 },
    });
    expect(Number.isFinite(x)).toBe(true);
    expect(Number.isFinite(y)).toBe(true);
  });
});

describe("sharedTooltipOption leaves what it cannot improve untouched", () => {
  const opts = { cursorValue: () => null, more };

  it("returns the very same object for a chart that declared no tooltip", () => {
    const option = { xAxis: { type: "time" as const }, series: [] };
    expect(sharedTooltipOption(option, () => null, opts)).toBe(option);
  });

  it("returns the very same object for a multi-tooltip option it cannot reason about", () => {
    const option = { tooltip: [{ trigger: "axis" as const }] } as never;
    expect(sharedTooltipOption(option, () => null, opts)).toBe(option);
  });

  it("adds no y-pill formatter to a chart whose axis is deliberately raw", () => {
    // The Console's own plot: an unformatted numeric axis. A pill inventing
    // units there would be worse than the bare number.
    const out = sharedTooltipOption(
      { tooltip: { trigger: "axis" }, yAxis: { type: "value" } },
      () => null,
      opts,
    );
    const yAxis = out.yAxis as { axisPointer?: { label?: { formatter?: unknown } } };
    expect(yAxis?.axisPointer?.label?.formatter).toBeUndefined();
  });

  it("leaves a multi-axis chart's pill alone — there is no single 'the y value'", () => {
    const out = sharedTooltipOption(
      { tooltip: { trigger: "axis" }, yAxis: [{ type: "value" }, { type: "value" }] },
      () => null,
      opts,
    );
    expect(Array.isArray(out.yAxis)).toBe(true);
  });
});
