import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type * as echarts from "echarts";

/**
 * The one mount point every chart in this console goes through.
 *
 * echarts itself is stubbed: jsdom implements no 2d canvas context, so a real
 * echarts.init throws, which is why every page test in this repo mocks the
 * component wholesale. This file mocks the LIBRARY instead, which leaves the
 * component's own wiring — init, setOption, resize, dispose, the tooltip clamp
 * and the shared time cursor — observable.
 */

const resize = vi.fn();
const setOption = vi.fn();
const dispose = vi.fn();

/** zrender's event bus, per chart instance, so a test can fire a hover. */
interface Zr {
  handlers: Map<string, ((e: { offsetX: number }) => void)[]>;
  on: (name: string, fn: (e: { offsetX: number }) => void) => void;
  off: (name: string, fn: (e: { offsetX: number }) => void) => void;
}

interface FakeChart {
  zr: Zr;
  /** The chart's OWN event bus (legendselectchanged), distinct from zrender's. */
  events: Map<string, ((e: unknown) => void)[]>;
  on: (name: string, fn: (e: unknown) => void) => void;
  /** ms per pixel is 1000 here, and each chart gets its own origin — the point
   *  of a cursor that travels by TIME is that the axes need not agree. */
  origin: number;
  resize: typeof resize;
  setOption: typeof setOption;
  dispose: typeof dispose;
  getZr: () => Zr;
  getWidth: () => number;
  getHeight: () => number;
  /** A bare instant converts to an x; a [t, v] pair converts to the point's own
   *  [x, y], which is what the neighbour readout puts its dots on. */
  convertToPixel: (finder: unknown, value: number | number[]) => number | number[];
  convertFromPixel: (finder: unknown, px: number) => number;
}

const charts: FakeChart[] = [];

function makeChart(): FakeChart {
  const handlers = new Map<string, ((e: { offsetX: number }) => void)[]>();
  const zr: Zr = {
    handlers,
    on: (name, fn) => handlers.set(name, [...(handlers.get(name) ?? []), fn]),
    off: (name, fn) => handlers.set(name, (handlers.get(name) ?? []).filter((f) => f !== fn)),
  };
  const origin = charts.length === 0 ? 1_000_000 : 1_500_000;
  const events = new Map<string, ((e: unknown) => void)[]>();
  const chart: FakeChart = {
    zr,
    events,
    on: (name, fn) => events.set(name, [...(events.get(name) ?? []), fn]),
    origin,
    resize,
    setOption,
    dispose,
    getZr: () => zr,
    getWidth: () => 1000,
    getHeight: () => 200,
    /* y is inverted the way a chart's is: a bigger value sits higher up. */
    convertToPixel: (_finder, value) =>
      Array.isArray(value) ? [(value[0] - origin) / 1000, 200 - value[1]] : (value - origin) / 1000,
    convertFromPixel: (_finder, px) => origin + px * 1000,
  };
  charts.push(chart);
  return chart;
}

const init = vi.fn(() => makeChart());

vi.mock("echarts", () => ({ init: (...args: unknown[]) => init(...(args as [])) }));

/** The observers the component constructs, so a test can fire one by hand. */
const observers: { cb: () => void; targets: Element[] }[] = [];

/** Frames the cursor group booked, drained on demand. */
const frames: (() => void)[] = [];

beforeEach(() => {
  observers.length = 0;
  charts.length = 0;
  frames.length = 0;
  vi.clearAllMocks();
  // vitest.setup.ts installs a no-op double; this one records, so the callback
  // the component registered can actually be invoked.
  Object.defineProperty(globalThis, "ResizeObserver", {
    configurable: true,
    value: class {
      private entry: { cb: () => void; targets: Element[] };
      constructor(cb: () => void) {
        this.entry = { cb, targets: [] };
        observers.push(this.entry);
      }
      observe(el: Element) {
        this.entry.targets.push(el);
      }
      unobserve() {}
      disconnect() {
        observers.splice(observers.indexOf(this.entry), 1);
      }
    },
  });
  /* The group coalesces onto animation frames; holding them here makes the
     "one notification per burst" property observable instead of timing-dependent. */
  Object.defineProperty(globalThis, "requestAnimationFrame", {
    configurable: true,
    value: (fn: () => void) => {
      frames.push(fn);
      return frames.length;
    },
  });
});

afterEach(cleanup);

const flush = () => frames.splice(0).forEach((f) => f());

const { EChart } = await import("@/components/echart");
const { ChartCursorProvider } = await import("@/lib/chart-cursor");

const TIME_OPTION: echarts.EChartsOption = {
  xAxis: { type: "time" },
  yAxis: { type: "value" },
  tooltip: { trigger: "axis" },
  series: [{ type: "line", data: [] }],
};

/** Fires a zrender mousemove on the chart at `index`. */
function hover(index: number, offsetX: number) {
  for (const fn of charts[index].zr.handlers.get("mousemove") ?? []) fn({ offsetX });
}

function leave(index: number) {
  for (const fn of charts[index].zr.handlers.get("globalout") ?? []) fn({ offsetX: 0 });
}

const crosshairs = () => screen.getAllByTestId("chart-crosshair");

describe("EChart resizes with its CONTAINER, not just with the window (QA scope 3, finding #12)", () => {
  it("observes the host element it drew into", () => {
    const { container } = render(<EChart option={{}} />);
    expect(observers.length).toBe(1);
    // The host is the chart's own box inside the wrapper that carries the
    // caller's className and holds the cursor line.
    expect(observers[0].targets[0]).toBe((container.firstChild as HTMLElement).firstChild);
  });

  it("resizes the chart when the box changes with the viewport standing still", () => {
    render(<EChart option={{}} />);
    resize.mockClear();
    // A sidebar collapsing, a rail wrapping, the grid dropping from two columns
    // to one at lg — none of these fire a window resize, and every one of them
    // used to leave the canvas drawn at its old width inside a moved box.
    observers[0].cb();
    expect(resize).toHaveBeenCalledTimes(1);
  });

  it("still answers a window resize — the two are belt and braces, not a swap", () => {
    render(<EChart option={{}} />);
    resize.mockClear();
    window.dispatchEvent(new Event("resize"));
    expect(resize).toHaveBeenCalledTimes(1);
  });

  it("disconnects the observer and disposes the chart on unmount", () => {
    const { unmount } = render(<EChart option={{}} />);
    unmount();
    expect(observers.length).toBe(0);
    expect(dispose).toHaveBeenCalledTimes(1);
  });

  it("mounts at all where ResizeObserver does not exist", () => {
    // Older embedded browsers, and any environment that never defined it: the
    // window listener alone must still carry the chart.
    Object.defineProperty(globalThis, "ResizeObserver", { configurable: true, value: undefined });
    expect(() => render(<EChart option={{}} />)).not.toThrow();
  });
});

/* ── the tooltip that used to be cut off ────────────────────────────────── */

describe("every chart's tooltip is clamped to the window", () => {
  it("hands echarts a position callback instead of leaving the panel to decide", () => {
    render(<EChart option={TIME_OPTION} />);
    const passed = setOption.mock.calls[0][0] as { tooltip: { position?: unknown; appendTo?: unknown } };
    expect(typeof passed.tooltip.position).toBe("function");
    expect(passed.tooltip.appendTo).toBe("body");
  });

  it("leaves a chart with no tooltip exactly as its caller built it", () => {
    const option = { xAxis: { type: "time" as const }, series: [] };
    render(<EChart option={option} />);
    expect(setOption.mock.calls[0][0]).toBe(option);
  });
});

/* ── the crosshair: a cross where the mouse is, a line everywhere else ───── */

/**
 * The owner asked for Grafana's crosshair: on the chart under the pointer, the
 * full cross — vertical for the instant, horizontal for the value — so the y can
 * be read off without counting gridlines. On the page's OTHER panels the shared
 * cursor stays vertical-only, because their y-axes are not this one's and a
 * horizontal line across them would be a lie.
 */
describe("the hovered chart gets a CROSS, the rest keep a vertical line", () => {
  it("hands echarts a cross axis pointer, which it only ever draws on the hovered chart", () => {
    render(<EChart option={TIME_OPTION} />);
    const passed = setOption.mock.calls[0][0] as { tooltip: { axisPointer?: { type?: string } } };
    expect(passed.tooltip.axisPointer?.type).toBe("cross");
  });

  it("keeps the SYNCED line vertical — one element, full height, one pixel wide", () => {
    render(
      <ChartCursorProvider>
        <EChart option={TIME_OPTION} />
        <EChart option={TIME_OPTION} />
      </ChartCursorProvider>,
    );
    hover(0, 540);
    flush();
    const line = crosshairs()[1];
    expect(line.style.opacity).toBe("1");
    expect(line.className).toMatch(/w-px/);
    expect(line.className).toMatch(/inset-y-0/);
    // No second, horizontal line on a panel whose y-axis is not the hovered one's.
    expect(crosshairs()).toHaveLength(2);
  });

  it("still costs ONE frame for a whole mousemove burst, cross or no cross", () => {
    render(
      <ChartCursorProvider>
        <EChart option={TIME_OPTION} />
        <EChart option={TIME_OPTION} />
      </ChartCursorProvider>,
    );
    setOption.mockClear();
    for (let px = 500; px < 560; px++) hover(0, px);
    expect(frames).toHaveLength(1);
    flush();
    // The cross is declared once in the option; it is NOT re-set per pixel.
    expect(setOption).not.toHaveBeenCalled();
  });
});

/* ── the tooltip that tried to name ninety-nine series ───────────────────── */

describe("every chart's tooltip is capped", () => {
  const rowsFor = (n: number) =>
    Array.from({ length: n }, (_, i) => ({
      marker: "",
      seriesName: `series-${i}`,
      axisValueLabel: "12:00:00",
      value: [1_700_000_000_000, i],
    }));

  const formatterOf = () =>
    (setOption.mock.calls[0][0] as { tooltip: { formatter: (p: unknown) => string } }).tooltip.formatter;

  it("names ten of ninety-nine series and says how many it did not", () => {
    render(<EChart option={TIME_OPTION} />);
    const html = formatterOf()(rowsFor(99));
    expect(html).toContain("series-98");
    expect(html).toContain("+89 more");
    expect((html.match(/series-/g) ?? []).length).toBe(10);
  });

  it("leaves a handful of series alone — there is nothing to cap", () => {
    render(<EChart option={TIME_OPTION} />);
    const html = formatterOf()(rowsFor(3));
    expect((html.match(/series-/g) ?? []).length).toBe(3);
    expect(html).not.toContain("more");
  });
});

/* ── the shared time cursor ─────────────────────────────────────────────── */

describe("hovering one chart draws the time cursor on every OTHER chart of the page", () => {
  function TwoCharts() {
    return (
      <ChartCursorProvider>
        <EChart option={TIME_OPTION} />
        <EChart option={TIME_OPTION} />
      </ChartCursorProvider>
    );
  }

  it("maps the cursor by TIMESTAMP, not by pixel — the two axes disagree on purpose", () => {
    render(<TwoCharts />);
    // Chart A's origin is 1_000_000 and 1px is 1000ms, so 40px is t=1_040_000.
    hover(0, 40);
    flush();
    // Chart B's origin is 1_500_000, so that same INSTANT is off its left edge…
    expect(crosshairs()[1].style.opacity).toBe("0");

    // …while an instant inside B's window lands at B's own pixel for it.
    hover(0, 540);
    flush();
    expect(crosshairs()[1].style.opacity).toBe("1");
    expect(crosshairs()[1].style.transform).toBe("translateX(40px)");
  });

  it("leaves the HOVERED chart to its own axis pointer rather than doubling the line", () => {
    render(<TwoCharts />);
    hover(0, 540);
    flush();
    expect(crosshairs()[0].style.opacity).toBe("0");
  });

  it("clears every panel when the pointer leaves the one being hovered", () => {
    render(<TwoCharts />);
    hover(0, 540);
    flush();
    expect(crosshairs()[1].style.opacity).toBe("1");

    leave(0);
    flush();
    expect(crosshairs()[1].style.opacity).toBe("0");
  });

  it("works in the other direction too — every chart both publishes and listens", () => {
    render(<TwoCharts />);
    // 40px on B is t=1_540_000, which is 540px on A.
    hover(1, 40);
    flush();
    expect(crosshairs()[0].style.transform).toBe("translateX(540px)");
  });

  it("costs ONE frame for a whole mousemove burst, not one redraw per pixel", () => {
    render(<TwoCharts />);
    setOption.mockClear();
    for (let px = 500; px < 560; px++) hover(0, px);
    // Sixty positions, one booked frame, and not a single setOption between them.
    expect(frames).toHaveLength(1);
    flush();
    expect(setOption).not.toHaveBeenCalled();
    expect(crosshairs()[1].style.transform).toBe("translateX(59px)");
  });

  it("does not sync a chart whose x-axis is not time", () => {
    render(
      <ChartCursorProvider>
        <EChart option={TIME_OPTION} />
        <EChart option={{ xAxis: { type: "category", data: [] }, yAxis: {}, series: [] }} />
      </ChartCursorProvider>,
    );
    hover(0, 540);
    flush();
    expect(crosshairs()[1].style.opacity).toBe("0");
  });

  it("leaves a chart mounted OUTSIDE a page group entirely alone", () => {
    render(<EChart option={TIME_OPTION} />);
    /* The zr hover IS wired — an ungrouped chart still needs to know where its
       own pointer is, for the tooltip's row cap — but with nothing subscribed
       there is nothing to publish to and no line to draw. */
    hover(0, 540);
    flush();
    expect(frames).toHaveLength(0);
    expect(crosshairs()[0].style.opacity).toBe("0");
  });

  it("catches up a panel that mounted mid-hover", () => {
    const { rerender } = render(
      <ChartCursorProvider>
        <EChart option={TIME_OPTION} />
      </ChartCursorProvider>,
    );
    hover(0, 540);
    flush();

    rerender(
      <ChartCursorProvider>
        <EChart option={TIME_OPTION} />
        <EChart option={TIME_OPTION} />
      </ChartCursorProvider>,
    );
    // The second chart reads the group's standing instant on mount rather than
    // staying blank until the mouse moves again.
    expect(crosshairs()[1].style.transform).toBe("translateX(40px)");
  });
});

/* ── what the line was missing: WHERE the neighbour reads ────────────────── */

/**
 * The owner raised this twice, which is the tell that the vertical line alone
 * was the wrong answer. On Investigate → Signals (Packet loss above, RTT p95
 * below) and on Explore he hovers one panel to read the SAME INSTANT on its
 * neighbours; a bare line tells him only where in time he already knew he was.
 *
 * So each neighbour marks its own curve: a dot on every series' own sample at
 * that instant. Not a box of numbers — the third owner report on this mechanic
 * was that a readout box on every panel covers the very data it annotates, and
 * that the values belong to the tooltip of the panel actually under the mouse.
 * One tooltip, many dots.
 *
 * They still do NOT get the horizontal half of the cross: their y-axes are other
 * quantities, and a "53.9%" height dragged down from the loss panel points at a
 * meaningless millisecond on the RTT one.
 */

/** Packet loss, a ratio printed as a percentage, sampled every 100s. */
const LOSS_PANEL: echarts.EChartsOption = {
  xAxis: { type: "time" },
  yAxis: { type: "value" },
  tooltip: { trigger: "axis", valueFormatter: (v) => `${(Number(v) * 100).toFixed(1)}%` },
  series: [
    {
      name: "prod-m03",
      type: "line",
      color: "#4d8",
      data: Array.from({ length: 11 }, (_, k) => [1_000_000 + k * 100_000, k / 100]),
    },
  ],
};

/** RTT p95, in milliseconds, over a window that starts 500s later than loss'. */
const RTT_PANEL: echarts.EChartsOption = {
  xAxis: { type: "time" },
  yAxis: { type: "value" },
  tooltip: { trigger: "axis", valueFormatter: (v) => `${Number(v).toFixed(1)}ms` },
  series: [
    {
      name: "prod-m03",
      type: "line",
      color: "#48d",
      data: Array.from({ length: 11 }, (_, k) => [1_500_000 + k * 100_000, 8 + k]),
    },
  ],
};

const panels = () => screen.getAllByTestId("chart-panel");

/** The neighbour panel's readout, whichever chart is second on the page. */
const neighbour = () => within(panels()[1]);

function Signals() {
  return (
    <ChartCursorProvider>
      <EChart option={LOSS_PANEL} />
      <EChart option={RTT_PANEL} />
    </ChartCursorProvider>
  );
}

describe("a neighbour marks its own curve at the shared instant", () => {
  it("puts the dot on the SAMPLE rather than on the cursor line", () => {
    render(<Signals />);
    // 540px on the loss panel is t=1_540_000, which is 40px into the RTT panel.
    hover(0, 540);
    flush();
    // The shared instant is 40px into this panel; the sample it honestly reads
    // sits at 0px, and the gap between the two is visible rather than papered
    // over with an interpolated point on the line.
    expect(crosshairs()[1].style.transform).toBe("translateX(40px)");
    expect(neighbour().getByTestId("chart-readout-dot").style.transform).toBe("translate(0px, 192px)");
    expect(neighbour().getByTestId("chart-readout-dot").style.opacity).toBe("1");
  });

  it("puts NO box of numbers on any panel — the values are the hovered tooltip's job", () => {
    /* The owner's third report on this mechanic: a readout box on every
       neighbour sat on top of the curves it was annotating, so a page of panels
       answered a glance with a search. The panel under the pointer already has
       a tooltip listing every series and its value; the neighbours answer
       "where on my own curve", which is what they alone can say. */
    render(<Signals />);
    hover(0, 540);
    flush();
    expect(screen.queryAllByTestId("chart-readout")).toHaveLength(0);
    expect(screen.queryAllByTestId("chart-readout-row")).toHaveLength(0);
    expect(screen.queryAllByTestId("chart-readout-value")).toHaveLength(0);
    // What it does have is the line and the dot.
    expect(crosshairs()[1].style.opacity).toBe("1");
    expect(neighbour().getByTestId("chart-readout-dot").style.opacity).toBe("1");
  });

  it("wears the series' own colour, so two curves' dots are told apart", () => {
    render(<Signals />);
    hover(0, 540);
    flush();
    expect(neighbour().getByTestId("chart-readout-dot").style.background).toBe("rgb(68, 136, 221)");
  });

  it("marks NOTHING on a neighbour whose series has a hole at the shared instant", () => {
    const gapped: echarts.EChartsOption = {
      ...RTT_PANEL,
      series: [
        {
          name: "prod-m03",
          type: "line",
          data: [[1_200_000, 1], [1_300_000, 2], [1_400_000, 3], [1_800_000, 4], [1_900_000, 5]],
        },
      ],
    };
    render(
      <ChartCursorProvider>
        <EChart option={LOSS_PANEL} />
        <EChart option={gapped} />
      </ChartCursorProvider>,
    );
    hover(0, 540);
    flush();
    // The line still says WHEN — it is the point that would have been invented.
    expect(crosshairs()[1].style.opacity).toBe("1");
    expect(neighbour().getByTestId("chart-readout-dot").style.opacity).toBe("0");
  });

  it("caps the dots at five — a ninety-nine series panel is not ninety-nine dots", () => {
    const many: echarts.EChartsOption = {
      ...RTT_PANEL,
      series: Array.from({ length: 8 }, (_, i) => ({
        name: `pair-${i}`,
        type: "line" as const,
        data: Array.from({ length: 11 }, (_, k) => [1_500_000 + k * 100_000, i]),
      })),
    };
    render(
      <ChartCursorProvider>
        <EChart option={LOSS_PANEL} />
        <EChart option={many} />
      </ChartCursorProvider>,
    );
    hover(0, 540);
    flush();
    expect(neighbour().getAllByTestId("chart-readout-dot")).toHaveLength(5);
  });

  it("leaves the HOVERED panel to its own tooltip — no dots under the mouse", () => {
    render(<Signals />);
    hover(0, 540);
    flush();
    expect(within(panels()[0]).getByTestId("chart-readout-dot").style.opacity).toBe("0");
  });

  it("clears every dot when the pointer leaves the page's charts", () => {
    render(<Signals />);
    hover(0, 540);
    flush();
    expect(neighbour().getByTestId("chart-readout-dot").style.opacity).toBe("1");

    leave(0);
    flush();
    expect(neighbour().getByTestId("chart-readout-dot").style.opacity).toBe("0");
  });

  it("still costs ONE frame for a whole mousemove burst, and never a setOption", () => {
    render(<Signals />);
    setOption.mockClear();
    for (let px = 500; px < 560; px++) hover(0, px);
    expect(frames).toHaveLength(1);
    flush();
    expect(setOption).not.toHaveBeenCalled();
    // The dots booked no frame of their own inside that one.
    expect(frames).toHaveLength(0);
    // 1_559_000 is nearer the 1_600_000 sample (v=9) than the 1_500_000 one.
    expect(neighbour().getByTestId("chart-readout-dot").style.transform).toBe("translate(100px, 191px)");
  });

  it("draws no dot at all on a chart that has no data to read", () => {
    render(
      <ChartCursorProvider>
        <EChart option={LOSS_PANEL} />
        <EChart option={TIME_OPTION} />
      </ChartCursorProvider>,
    );
    hover(0, 540);
    flush();
    expect(neighbour().queryByTestId("chart-readout-dot")).toBeNull();
  });

  it("draws no dot at all on a category chart — a hop number is not an instant", () => {
    render(
      <ChartCursorProvider>
        <EChart option={LOSS_PANEL} />
        <EChart
          option={{
            xAxis: { type: "category", data: ["1", "2"] },
            yAxis: {},
            series: [{ type: "bar", data: [1, 2] }],
          }}
        />
      </ChartCursorProvider>,
    );
    hover(0, 540);
    flush();
    expect(neighbour().queryByTestId("chart-readout-dot")).toBeNull();
  });

  it("leaves the hovered chart's y pill exactly as it was — it reads the POINTER", () => {
    /* The pill prints the height the mouse is at, in the axis' units, with no
       series name beside it; it is a claim about the CURSOR, not about the data,
       and nothing in this change touches it. */
    render(<EChart option={LOSS_PANEL} />);
    const passed = setOption.mock.calls[0][0] as {
      yAxis: { axisPointer: { label: { formatter: (p: { value: unknown }) => string } } };
    };
    expect(passed.yAxis.axisPointer.label.formatter({ value: 0.539 })).toBe("53.9%");
  });
});

describe("the dots of a page that is still being assembled", () => {
  it("catches a panel up on mount, dot and all", () => {
    const { rerender } = render(
      <ChartCursorProvider>
        <EChart option={LOSS_PANEL} />
      </ChartCursorProvider>,
    );
    hover(0, 540);
    flush();

    rerender(
      <ChartCursorProvider>
        <EChart option={LOSS_PANEL} />
        <EChart option={RTT_PANEL} />
      </ChartCursorProvider>,
    );
    /* Explore's second panel finishing its fetch mid-hover: it reads the group's
       standing instant on mount rather than staying blank until the mouse moves
       again — and the dot has to arrive with the line, not a frame later. */
    expect(crosshairs()[1].style.transform).toBe("translateX(40px)");
    expect(neighbour().getByTestId("chart-readout-dot").style.transform).toBe("translate(0px, 192px)");
  });

  it("re-reads a panel whose data was replaced under a standing cursor", () => {
    /* Explore polls every thirty seconds. The crosshair deliberately survives
       that (it would otherwise clear on every page of charts mid-hover), so the
       DOTS beside it must be moved onto the new option's points rather than left
       sitting where the last poll's curve was. */
    const later: echarts.EChartsOption = {
      ...RTT_PANEL,
      series: [
        {
          name: "prod-m03",
          type: "line",
          color: "#48d",
          data: Array.from({ length: 11 }, (_, k) => [1_500_000 + k * 100_000, 40 + k]),
        },
      ],
    };
    const { rerender } = render(
      <ChartCursorProvider>
        <EChart option={LOSS_PANEL} />
        <EChart option={RTT_PANEL} />
      </ChartCursorProvider>,
    );
    hover(0, 540);
    flush();
    expect(neighbour().getByTestId("chart-readout-dot").style.transform).toBe("translate(0px, 192px)");

    rerender(
      <ChartCursorProvider>
        <EChart option={LOSS_PANEL} />
        <EChart option={later} />
      </ChartCursorProvider>,
    );
    // No mouse moved and no frame was booked: the option effect repainted.
    expect(frames).toHaveLength(0);
    expect(neighbour().getByTestId("chart-readout-dot").style.transform).toBe("translate(0px, 160px)");
  });
});

/* ── a series the reader switched off ────────────────────────────────────── */

// A legend click removes a curve from the chart. A dot left on its sample is the shared crosshair
// marking a line that is not drawn — a reading of something the reader deliberately took away.
describe("the neighbour's dots follow its legend", () => {
  const twoSeries: echarts.EChartsOption = {
    xAxis: { type: "time" },
    yAxis: { type: "value" },
    tooltip: { trigger: "axis" },
    series: [
      {
        name: "keep",
        type: "line",
        data: Array.from({ length: 11 }, (_, k) => [1_500_000 + k * 100_000, 8]),
      },
      {
        name: "drop",
        type: "line",
        data: Array.from({ length: 11 }, (_, k) => [1_500_000 + k * 100_000, 9]),
      },
    ],
  };

  it("hides the dot of a series switched off in the legend, and keeps the rest", () => {
    render(
      <ChartCursorProvider>
        <EChart option={LOSS_PANEL} />
        <EChart option={twoSeries} />
      </ChartCursorProvider>,
    );
    hover(0, 540);
    flush();
    expect(neighbour().getAllByTestId("chart-readout-dot").map((d) => d.style.opacity)).toEqual(["1", "1"]);

    // The chart's own legend event, which is the ONLY place this state exists — the option the
    // readout is built from does not carry it.
    charts[1].events.get("legendselectchanged")?.forEach((fn) => fn({ selected: { keep: true, drop: false } }));
    flush();
    expect(neighbour().getAllByTestId("chart-readout-dot").map((d) => d.style.opacity)).toEqual(["1", "0"]);
  });

  it("forgets that selection when the data is replaced — notMerge puts every series back", () => {
    const { rerender } = render(
      <ChartCursorProvider>
        <EChart option={LOSS_PANEL} />
        <EChart option={twoSeries} />
      </ChartCursorProvider>,
    );
    hover(0, 540);
    flush();
    charts[1].events.get("legendselectchanged")?.forEach((fn) => fn({ selected: { keep: true, drop: false } }));
    flush();

    rerender(
      <ChartCursorProvider>
        <EChart option={LOSS_PANEL} />
        <EChart option={{ ...twoSeries }} />
      </ChartCursorProvider>,
    );
    // setOption(notMerge) re-selects everything, so a remembered "off" would hide a curve that is
    // back on the screen.
    expect(neighbour().getAllByTestId("chart-readout-dot").map((d) => d.style.opacity)).toEqual(["1", "1"]);
  });
});
