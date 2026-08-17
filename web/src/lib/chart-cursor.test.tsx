import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type * as echarts from "echarts";
import {
  ChartCursorProvider,
  READOUT_ROW_CAP,
  createCursorGroup,
  isTimeSeriesOption,
  nearestSample,
  pickReadoutRows,
  readoutSeries,
  useChartCursor,
  type ReadoutSeries,
} from "./chart-cursor";

/**
 * The Grafana mechanic the owner asked for: hovering one panel puts a time
 * cursor on every other panel of the same page, so a spike on one metric can be
 * read against its neighbours at the same INSTANT.
 *
 * The group is the page. It carries a timestamp, never a pixel — the panels'
 * axes differ — and it notifies at most once per frame, because a mousemove
 * fires far more often than a screen redraws.
 */

afterEach(cleanup);

/** A group whose frames this test drives by hand. */
function manual() {
  const frames: (() => void)[] = [];
  const group = createCursorGroup((fn) => {
    frames.push(fn);
    return frames.length;
  });
  return { group, flush: () => frames.splice(0).forEach((f) => f()) };
}

describe("createCursorGroup carries an instant, not a pixel", () => {
  it("hands every subscriber the timestamp that was published", () => {
    const { group, flush } = manual();
    const seen: (number | null)[] = [];
    group.subscribe((at) => seen.push(at));

    group.set(1_700_000_000_000, "chart-a");
    flush();
    expect(seen).toEqual([1_700_000_000_000]);
  });

  it("names the SOURCE, so the chart being hovered can skip drawing twice", () => {
    const { group, flush } = manual();
    const from: (string | undefined)[] = [];
    group.subscribe((_at, source) => from.push(source));

    group.set(42, "chart-a");
    flush();
    expect(from).toEqual(["chart-a"]);
  });

  it("clears with null when the pointer leaves", () => {
    const { group, flush } = manual();
    const seen: (number | null)[] = [];
    group.subscribe((at) => seen.push(at));

    group.set(42, "a");
    flush();
    group.set(null, "a");
    flush();
    expect(seen).toEqual([42, null]);
  });

  it("remembers the last instant for a chart that mounts mid-hover", () => {
    const { group, flush } = manual();
    group.set(99, "a");
    flush();
    expect(group.current()).toBe(99);
  });
});

describe("the group is CHEAP — one notification per frame, whatever the mouse does", () => {
  it("coalesces a burst of moves into a single subscriber call", () => {
    const { group, flush } = manual();
    const notified = vi.fn();
    group.subscribe(notified);

    // A real mousemove burst: sixty positions inside one frame.
    for (let i = 0; i < 60; i++) group.set(1000 + i, "a");
    expect(notified).not.toHaveBeenCalled();
    flush();

    expect(notified).toHaveBeenCalledTimes(1);
    expect(notified).toHaveBeenCalledWith(1059, "a");
  });

  it("schedules exactly one frame for that burst", () => {
    const schedule = vi.fn(() => 1);
    const group = createCursorGroup(schedule);
    for (let i = 0; i < 60; i++) group.set(i, "a");
    expect(schedule).toHaveBeenCalledTimes(1);
  });

  it("stops notifying an unsubscribed chart", () => {
    const { group, flush } = manual();
    const notified = vi.fn();
    const off = group.subscribe(notified);
    off();
    group.set(1, "a");
    flush();
    expect(notified).not.toHaveBeenCalled();
  });

  it("survives a subscriber that throws, so one broken chart cannot freeze the page", () => {
    const { group, flush } = manual();
    const good = vi.fn();
    group.subscribe(() => {
      throw new Error("boom");
    });
    group.subscribe(good);
    group.set(7, "a");
    expect(flush).not.toThrow();
    expect(good).toHaveBeenCalledWith(7, "a");
  });
});

describe("the provider scopes a group to ONE page", () => {
  function Probe({ label }: { label: string }) {
    const group = useChartCursor();
    return <span data-testid={label}>{group === null ? "none" : "group"}</span>;
  }

  it("hands every chart under it the same group", () => {
    let a: unknown;
    let b: unknown;
    function Capture({ into }: { into: (g: unknown) => void }) {
      into(useChartCursor());
      return null;
    }
    render(
      <ChartCursorProvider>
        <Capture into={(g) => (a = g)} />
        <Capture into={(g) => (b = g)} />
      </ChartCursorProvider>,
    );
    expect(a).toBe(b);
    expect(a).not.toBeNull();
  });

  it("gives two pages two groups", () => {
    let a: unknown;
    let b: unknown;
    function Capture({ into }: { into: (g: unknown) => void }) {
      into(useChartCursor());
      return null;
    }
    render(
      <>
        <ChartCursorProvider>
          <Capture into={(g) => (a = g)} />
        </ChartCursorProvider>
        <ChartCursorProvider>
          <Capture into={(g) => (b = g)} />
        </ChartCursorProvider>
      </>,
    );
    expect(a).not.toBe(b);
  });

  it("answers null outside a provider, so a stray chart is simply not synced", () => {
    render(<Probe label="orphan" />);
    expect(screen.getByTestId("orphan")).toHaveTextContent("none");
  });
});

describe("isTimeSeriesOption picks the charts a TIME cursor means something on", () => {
  it("accepts a time axis", () => {
    expect(isTimeSeriesOption({ xAxis: { type: "time" } })).toBe(true);
  });

  it("accepts a time axis declared as the first of several", () => {
    expect(isTimeSeriesOption({ xAxis: [{ type: "time" }, { type: "value" }] })).toBe(true);
  });

  it("rejects a category axis — an MTR hop number is not an instant", () => {
    expect(isTimeSeriesOption({ xAxis: { type: "category" } })).toBe(false);
  });

  it("rejects an option with no axis at all", () => {
    expect(isTimeSeriesOption({})).toBe(false);
  });
});

/* ── the neighbour READOUT ───────────────────────────────────────────────── */

/**
 * The owner's second telling of the same complaint: on Investigate → Signals he
 * hovers Packet loss and the RTT p95 panel below it draws a BARE vertical line.
 * That line says WHEN and nothing else, and "when" is the half he already knew.
 *
 * So the neighbours get a readout: at the shared instant, each series it can
 * honestly read gets a dot ON ITS OWN POINT and a row naming its OWN value in
 * that chart's OWN units. What they still do not get is a horizontal line —
 * a 53.9% height carried over from the loss panel points at a meaningless
 * millisecond on the RTT panel, and that is the whole reason it was left out.
 *
 * The three properties this half of the file pins are the honest ones: the
 * value comes from a sample the series ACTUALLY has, a hole reads as nothing,
 * and the row says which sample it read.
 */

/** A series sampled every minute, values 0,1,2,… from `from`. */
function evenly(name: string, from: number, count: number, step = 60_000): echarts.LineSeriesOption {
  return {
    name,
    type: "line",
    color: "#abc",
    data: Array.from({ length: count }, (_, i) => [from + i * step, i]),
  };
}

const oneSeries = (option: echarts.EChartsOption): ReadoutSeries => readoutSeries(option)[0];

describe("readoutSeries reads a chart's own series out of the option it was handed", () => {
  it("carries each series' name, its colour and its points", () => {
    const model = readoutSeries({ series: [evenly("rtt p95", 1_000, 3)] });
    expect(model).toHaveLength(1);
    expect(model[0].name).toBe("rtt p95");
    expect(model[0].color).toBe("#abc");
    expect(model[0].points).toEqual([
      [1_000, 0],
      [61_000, 1],
      [121_000, 2],
    ]);
  });

  it("drops the marker-host series the annotation overlay adds — `data: []` is not a reading", () => {
    const model = readoutSeries({
      series: [evenly("loss", 0, 3), { name: "Annotations", type: "line", data: [] }],
    });
    expect(model.map((s) => s.name)).toEqual(["loss"]);
  });

  it("drops a HOLE rather than reading it as a zero", () => {
    // lib/chart-tooltip.ts already refuses to print `0.0ms` for a gap; the same
    // gap must not become a dot sitting on the axis either.
    const model = oneSeries({
      series: [{ name: "loss", type: "line", data: [[0, 0.5], [60_000, null], [120_000, 0.7]] }],
    });
    expect(model.points).toEqual([
      [0, 0.5],
      [120_000, 0.7],
    ]);
  });

  it("reads the {value:[t,v]} point form as well as the bare pair", () => {
    const model = oneSeries({
      series: [{ name: "loss", type: "line", data: [{ value: [0, 1] }, { value: [60_000, 2] }] }],
    });
    expect(model.points).toEqual([
      [0, 1],
      [60_000, 2],
    ]);
  });

  it("takes the colour from itemStyle, then from lineStyle, when no `color` was declared", () => {
    const item = oneSeries({
      series: [{ name: "a", type: "line", itemStyle: { color: "#111" }, data: [[0, 1]] }],
    });
    const line = oneSeries({
      series: [{ name: "b", type: "line", lineStyle: { color: "#222" }, data: [[0, 1]] }],
    });
    expect(item.color).toBe("#111");
    expect(line.color).toBe("#222");
  });

  it("answers a null colour for a series that declared none — the dot is not worth a guess", () => {
    expect(oneSeries({ series: [{ name: "a", type: "line", data: [[0, 1]] }] }).color).toBeNull();
  });

  it("sorts the points ascending whatever order the option listed them in", () => {
    const model = oneSeries({
      series: [{ name: "a", type: "line", data: [[120_000, 3], [0, 1], [60_000, 2]] }],
    });
    expect(model.points.map(([t]) => t)).toEqual([0, 60_000, 120_000]);
  });

  it("measures each series' OWN sampling step as the median gap between its points", () => {
    // 60s, 60s, 300s: the median is the 60s the series is actually scraped at,
    // not the 140s an average would report because of one hole.
    const model = oneSeries({
      series: [{ name: "a", type: "line", data: [[0, 1], [60_000, 2], [120_000, 3], [420_000, 4]] }],
    });
    expect(model.step).toBe(60_000);
  });

  it("lends the CHART's median step to a series too short to have one of its own", () => {
    const model = readoutSeries({
      series: [evenly("a", 0, 4), { name: "b", type: "line", data: [[90_000, 9]] }],
    });
    expect(model[1].step).toBe(60_000);
  });

  it("returns nothing for a chart with no series data at all", () => {
    expect(readoutSeries({ series: [{ type: "line", data: [] }] })).toEqual([]);
    expect(readoutSeries({})).toEqual([]);
  });
});

describe("nearestSample never invents a value between two samples", () => {
  const series = oneSeries({ series: [evenly("a", 0, 5)] });

  it("snaps to the NEARER of the two samples the instant falls between", () => {
    // 20s past the first of two samples a minute apart.
    expect(nearestSample(series, 20_000)).toEqual({ t: 0, v: 0 });
    expect(nearestSample(series, 40_000)).toEqual({ t: 60_000, v: 1 });
  });

  it("reports the SAMPLE's own timestamp, not the instant it was asked about", () => {
    // This is what lets the row say which reading it is showing.
    expect(nearestSample(series, 20_000)?.t).toBe(0);
  });

  it("only ever returns a value the series actually has", () => {
    const values = new Set(series.points.map(([, v]) => v));
    for (let at = -10_000; at <= 250_000; at += 1_000) {
      const hit = nearestSample(series, at);
      if (hit) expect(values.has(hit.v)).toBe(true);
    }
  });

  it("says NOTHING in the middle of a hole, and still reads at its EDGE", () => {
    /* The edge half of this caught a first draft that rejected the whole gap:
       130_000 is ten seconds from a real sample and must read, while 300_000 is
       three minutes from anything and must not. */
    const gapped = oneSeries({
      series: [{ name: "a", type: "line", data: [[0, 1], [60_000, 2], [120_000, 3], [600_000, 4]] }],
    });
    expect(gapped.step).toBe(60_000);
    expect(nearestSample(gapped, 300_000)).toBeNull();
    expect(nearestSample(gapped, 130_000)).toEqual({ t: 120_000, v: 3 });
  });

  it("reads the last sample up to one step past the end of the series", () => {
    expect(nearestSample(series, 240_000 + 30_000)).toEqual({ t: 240_000, v: 4 });
    expect(nearestSample(series, 240_000 + 90_000)).toBeNull();
  });

  it("reads the first sample up to one step before the start, and nothing earlier", () => {
    expect(nearestSample(series, -30_000)).toEqual({ t: 0, v: 0 });
    expect(nearestSample(series, -90_000)).toBeNull();
  });

  it("reads only an EXACT hit when the chart gave it no step to judge by", () => {
    const lone = oneSeries({ series: [{ name: "a", type: "line", data: [[5_000, 7]] }] });
    expect(lone.step).toBe(0);
    expect(nearestSample(lone, 5_000)).toEqual({ t: 5_000, v: 7 });
    expect(nearestSample(lone, 5_001)).toBeNull();
  });
});

describe("pickReadoutRows keeps the box readable on a worst-5 panel", () => {
  const chartOf = (count: number) =>
    readoutSeries({ series: Array.from({ length: count }, (_, i) => evenly(`pair-${i}`, 0, 5)) });

  it("shows every series of a worst-5 panel — five rows is the whole point of the cap", () => {
    expect(READOUT_ROW_CAP).toBe(5);
    expect(pickReadoutRows(chartOf(5), 20_000).rows).toHaveLength(5);
    expect(pickReadoutRows(chartOf(5), 20_000).hidden).toBe(0);
  });

  it("caps a ninety-nine series chart and counts the rest, the way the tooltip does", () => {
    const { rows, hidden } = pickReadoutRows(chartOf(99), 20_000);
    expect(rows).toHaveLength(5);
    expect(hidden).toBe(94);
  });

  it("keeps the chart's OWN series order among the rows it kept", () => {
    /* Which series are worth a row is decided at the instant and can change as
       the cursor travels; the ORDER they are drawn in never does, or a panel
       nobody is pointing at would reshuffle itself under the mouse. */
    const model = readoutSeries({
      series: [
        { name: "low", type: "line", data: [[0, 1], [60_000, 1]] },
        { name: "high", type: "line", data: [[0, 9], [60_000, 9]] },
        { name: "mid", type: "line", data: [[0, 5], [60_000, 5]] },
      ],
    });
    const { rows } = pickReadoutRows(model, 10_000, 2);
    // Ranked by value ("high", "mid"), drawn in the chart's order.
    expect(rows.map((r) => r.series.name)).toEqual(["high", "mid"]);
  });

  it("drops a series that has no sample near the instant and keeps the ones that do", () => {
    const model = readoutSeries({
      series: [evenly("live", 0, 5), { name: "stale", type: "line", data: [[0, 1], [60_000, 2]] }],
    });
    expect(pickReadoutRows(model, 240_000).rows.map((r) => r.series.name)).toEqual(["live"]);
  });

  it("carries each row's own sample and the index of the series it came from", () => {
    const model = chartOf(2);
    const { rows } = pickReadoutRows(model, 40_000);
    expect(rows[1].index).toBe(1);
    expect(rows[1].sample).toEqual({ t: 60_000, v: 1 });
  });

  it("is empty for a null instant — nothing hovered is nothing to read", () => {
    expect(pickReadoutRows(chartOf(3), null)).toEqual({ rows: [], hidden: 0 });
  });
});

describe("the readout under an option nobody would write on purpose", () => {
  it("reads a series declared as ONE object rather than as a list of one", () => {
    expect(readoutSeries({ series: { name: "a", type: "line", data: [[0, 1]] } })[0].name).toBe("a");
  });

  it("steps over a null entry in the series list", () => {
    const model = readoutSeries({
      series: [null, evenly("a", 0, 3), undefined] as unknown as echarts.SeriesOption[],
    });
    expect(model.map((s) => s.name)).toEqual(["a"]);
  });

  it("drops points that are not points at all", () => {
    const model = oneSeries({
      series: [
        {
          name: "a",
          type: "line",
          data: ["nonsense", 42, [], [0], [Number.NaN, 1], ["not-a-date", 2], [0, 1]],
        } as unknown as echarts.SeriesOption,
      ],
    });
    expect(model.points).toEqual([[0, 1]]);
  });

  it("survives a series whose every sample carries the SAME timestamp", () => {
    // No positive gap anywhere, so there is no step to judge distance by, and
    // the honest answer is to read only an exact hit.
    const model = oneSeries({
      series: [{ name: "a", type: "line", data: [[7, 1], [7, 2], [7, 3]] }],
    });
    expect(model.step).toBe(0);
    expect(nearestSample(model, 8)).toBeNull();
    expect(nearestSample(model, 7)).not.toBeNull();
  });

  it("finds the right sample in a series far longer than a chart would draw", () => {
    const long = oneSeries({ series: [evenly("a", 0, 5_000, 15_000)] });
    // Straight down the middle of two samples, at the far end of the series.
    expect(nearestSample(long, 4_000 * 15_000 + 7_000)).toEqual({ t: 4_000 * 15_000, v: 4_000 });
    expect(nearestSample(long, 4_000 * 15_000 + 8_000)).toEqual({ t: 4_001 * 15_000, v: 4_001 });
  });

  it("refuses a NaN instant rather than reading the first sample it can reach", () => {
    expect(nearestSample(oneSeries({ series: [evenly("a", 0, 3)] }), Number.NaN)).toBeNull();
  });

  it("is empty for a chart whose every series is a marker host", () => {
    expect(
      pickReadoutRows(readoutSeries({ series: [{ type: "line", data: [] }, { type: "line", data: [] }] }), 0),
    ).toEqual({ rows: [], hidden: 0 });
  });
});
