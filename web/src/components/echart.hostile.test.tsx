import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type * as echarts from "echarts";

/**
 * echart.hostile.test.tsx — the shared chart layer with charts appearing and
 * disappearing under the pointer.
 *
 * Explore polls every thirty seconds and its compare panel swaps legs on a
 * segment click, so a page of charts is not a fixed set: a card can unmount
 * mid-hover because its poll came back empty, or because the reader flipped a
 * control with the mouse still resting on the panel next door. The cursor group
 * has to survive that without stranding — or clearing — the line every OTHER
 * panel is drawing.
 *
 * Same scaffolding as echart.test.tsx: the LIBRARY is stubbed, the component's
 * own wiring stays observable.
 */

const resize = vi.fn();
const setOption = vi.fn();
const dispose = vi.fn();

interface Zr {
  handlers: Map<string, ((e: { offsetX: number; offsetY?: number }) => void)[]>;
  on: (name: string, fn: (e: { offsetX: number; offsetY?: number }) => void) => void;
  off: (name: string, fn: (e: { offsetX: number; offsetY?: number }) => void) => void;
}

interface FakeChart {
  zr: Zr;
  origin: number;
  resize: typeof resize;
  setOption: typeof setOption;
  dispose: typeof dispose;
  getZr: () => Zr;
  getWidth: () => number;
  getHeight: () => number;
  convertToPixel: (finder: unknown, value: number | number[]) => number | number[];
  convertFromPixel: (finder: unknown, px: number) => number;
}

const charts: FakeChart[] = [];

/** Every chart shares one axis here: what is under test is the group, not the
 *  per-chart conversion echart.test.tsx already pins. */
function makeChart(): FakeChart {
  const handlers = new Map<string, ((e: { offsetX: number; offsetY?: number }) => void)[]>();
  const zr: Zr = {
    handlers,
    on: (name, fn) => handlers.set(name, [...(handlers.get(name) ?? []), fn]),
    off: (name, fn) => handlers.set(name, (handlers.get(name) ?? []).filter((f) => f !== fn)),
  };
  const origin = 1_000_000;
  const chart: FakeChart = {
    zr,
    origin,
    resize,
    setOption,
    dispose,
    getZr: () => zr,
    getWidth: () => 1000,
    getHeight: () => 200,
    convertToPixel: (_finder, value) =>
      Array.isArray(value) ? [(value[0] - origin) / 1000, 200 - value[1]] : (value - origin) / 1000,
    convertFromPixel: (_finder, px) => origin + px * 1000,
  };
  charts.push(chart);
  return chart;
}

const init = vi.fn(() => makeChart());
vi.mock("echarts", () => ({ init: (...args: unknown[]) => init(...(args as [])) }));

const frames: (() => void)[] = [];

beforeEach(() => {
  charts.length = 0;
  frames.length = 0;
  vi.clearAllMocks();
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
const { ChartCursorProvider, createCursorGroup } = await import("@/lib/chart-cursor");

const TIME_OPTION: echarts.EChartsOption = {
  xAxis: { type: "time" },
  yAxis: { type: "value" },
  tooltip: { trigger: "axis" },
  series: [{ type: "line", data: [] }],
};

function hover(index: number, offsetX: number) {
  for (const fn of charts[index].zr.handlers.get("mousemove") ?? []) fn({ offsetX, offsetY: 10 });
}

const crosshairs = () => screen.getAllByTestId("chart-crosshair");

describe("a panel leaving the page must not take the cursor with it", () => {
  /* The finding: EChart's cleanup published `null` unconditionally, so ANY
     chart unmounting cleared the shared instant — including a chart nobody was
     pointing at. On Explore that is a thirty-second poll away: a curated card
     whose query comes back empty unmounts its EChart, and the crosshair the
     reader was tracking across the other four panels vanished under the mouse. */
  it("keeps the line on the surviving panels when a THIRD chart unmounts mid-hover", () => {
    const { rerender } = render(
      <ChartCursorProvider>
        <EChart option={TIME_OPTION} />
        <EChart option={TIME_OPTION} />
        <EChart option={TIME_OPTION} />
      </ChartCursorProvider>,
    );
    hover(0, 540);
    flush();
    expect(crosshairs()[1].style.opacity).toBe("1");

    rerender(
      <ChartCursorProvider>
        <EChart option={TIME_OPTION} />
        <EChart option={TIME_OPTION} />
      </ChartCursorProvider>,
    );
    flush();

    // The pointer never moved. The panel it was published from is still here.
    expect(crosshairs()[1].style.opacity).toBe("1");
    expect(crosshairs()[1].style.transform).toBe("translateX(540px)");
  });

  it("still clears everything when the chart that PUBLISHED the instant goes away", () => {
    // The other half of the rule: a cursor from a panel that no longer exists
    // is a line pointing at nothing.
    const { rerender } = render(
      <ChartCursorProvider>
        <EChart option={TIME_OPTION} />
        <EChart option={TIME_OPTION} />
      </ChartCursorProvider>,
    );
    hover(0, 540);
    flush();
    expect(crosshairs()[1].style.opacity).toBe("1");

    rerender(
      <ChartCursorProvider>
        <EChart option={TIME_OPTION} />
      </ChartCursorProvider>,
    );
    flush();
    expect(crosshairs()[0].style.opacity).toBe("0");
  });
});

describe("the group under abuse", () => {
  it("a provider with no charts in it at all is simply inert", () => {
    expect(() =>
      render(
        <ChartCursorProvider>
          <p>nothing to sync</p>
        </ChartCursorProvider>,
      ),
    ).not.toThrow();
  });

  it("coalesces a thousand publishes into ONE notification", () => {
    const seen: (number | null)[] = [];
    const queue: (() => void)[] = [];
    const group = createCursorGroup((fn) => queue.push(fn));
    group.subscribe((at) => seen.push(at));
    for (let i = 0; i < 1000; i++) group.set(i, "a");
    expect(queue).toHaveLength(1);
    queue.splice(0).forEach((f) => f());
    // One frame, and it carries the LATEST position, not the first.
    expect(seen).toEqual([999]);
  });

  it("keeps notifying the rest when one subscriber throws", () => {
    const queue: (() => void)[] = [];
    const group = createCursorGroup((fn) => queue.push(fn));
    const good = vi.fn();
    group.subscribe(() => {
      throw new Error("one panel's converter blew up");
    });
    group.subscribe(good);
    group.set(1, "a");
    expect(() => queue.splice(0).forEach((f) => f())).not.toThrow();
    expect(good).toHaveBeenCalledOnce();
  });

  it("reports who published, so a chart can tell its own echo from a neighbour's", () => {
    const group = createCursorGroup(() => {});
    expect(group.currentSource()).toBe("");
    group.set(42, "chart-a");
    expect(group.current()).toBe(42);
    expect(group.currentSource()).toBe("chart-a");
  });

  it("does not deliver to a subscriber that unsubscribed before the frame ran", () => {
    const queue: (() => void)[] = [];
    const group = createCursorGroup((fn) => queue.push(fn));
    const gone = vi.fn();
    const off = group.subscribe(gone);
    group.set(1, "a");
    off();
    queue.splice(0).forEach((f) => f());
    expect(gone).not.toHaveBeenCalled();
  });
});

/* ── the READOUT while the page is rearranging itself ────────────────────── */

/**
 * The neighbour dots are the crosshair's other half — one on each series' own
 * sample at the shared instant — and they inherit the same hazard: a page of
 * charts is not a fixed set. A dot left standing on a curve that is gone is
 * worse than a line left standing on its own, because a dot claims to be ON
 * something.
 */
const WITH_DATA: echarts.EChartsOption = {
  xAxis: { type: "time" },
  yAxis: { type: "value" },
  tooltip: { trigger: "axis", valueFormatter: (v) => `${Number(v).toFixed(1)}ms` },
  series: [
    {
      name: "prod-m03",
      type: "line",
      color: "#48d",
      data: Array.from({ length: 11 }, (_, k) => [1_000_000 + k * 100_000, 8 + k]),
    },
  ],
};

const panels = () => screen.getAllByTestId("chart-panel");

describe("a dot must not outlive the data it was read from", () => {
  it("keeps the surviving panels' dots when a THIRD chart unmounts mid-hover", () => {
    const { rerender } = render(
      <ChartCursorProvider>
        <EChart option={WITH_DATA} />
        <EChart option={WITH_DATA} />
        <EChart option={WITH_DATA} />
      </ChartCursorProvider>,
    );
    hover(0, 540);
    flush();
    expect(within(panels()[1]).getByTestId("chart-readout-dot").style.transform).toBe("translate(500px, 187px)");

    rerender(
      <ChartCursorProvider>
        <EChart option={WITH_DATA} />
        <EChart option={WITH_DATA} />
      </ChartCursorProvider>,
    );
    flush();
    expect(within(panels()[1]).getByTestId("chart-readout-dot").style.opacity).toBe("1");
    expect(within(panels()[1]).getByTestId("chart-readout-dot").style.transform).toBe("translate(500px, 187px)");
  });

  it("clears the dots when the panel that PUBLISHED the instant goes away", () => {
    const { rerender } = render(
      <ChartCursorProvider>
        <EChart option={WITH_DATA} />
        <EChart option={WITH_DATA} />
      </ChartCursorProvider>,
    );
    hover(0, 540);
    flush();
    expect(within(panels()[1]).getByTestId("chart-readout-dot").style.opacity).toBe("1");

    rerender(
      <ChartCursorProvider>
        <EChart option={WITH_DATA} />
      </ChartCursorProvider>,
    );
    flush();
    expect(within(panels()[0]).getByTestId("chart-readout-dot").style.opacity).toBe("0");
  });

  it("takes the dots away entirely when a poll comes back with no points at all", () => {
    /* Explore's thirty-second poll returning an empty result is the case that
       matters: the chart stays mounted and the crosshair legitimately survives,
       so a dot that merely stopped being MOVED would sit there marking a point
       on a curve the panel no longer draws. */
    const { rerender } = render(
      <ChartCursorProvider>
        <EChart option={WITH_DATA} />
        <EChart option={WITH_DATA} />
      </ChartCursorProvider>,
    );
    hover(0, 540);
    flush();
    expect(within(panels()[1]).getByTestId("chart-readout-dot").style.transform).toBe("translate(500px, 187px)");

    rerender(
      <ChartCursorProvider>
        <EChart option={WITH_DATA} />
        <EChart option={{ ...WITH_DATA, series: [{ name: "prod-m03", type: "line", data: [] }] }} />
      </ChartCursorProvider>,
    );
    expect(within(panels()[1]).queryByTestId("chart-readout-dot")).toBeNull();
    // …and the line stays, because the INSTANT is still a fact about this panel.
    expect(crosshairs()[1].style.opacity).toBe("1");
  });
});

describe("a category chart neither publishes nor draws", () => {
  it("stays out of the group even while its neighbours are being hovered", () => {
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

  it("and hovering the category chart itself publishes nothing to the time charts", () => {
    render(
      <ChartCursorProvider>
        <EChart option={{ xAxis: { type: "category", data: [] }, yAxis: {}, series: [] }} />
        <EChart option={TIME_OPTION} />
      </ChartCursorProvider>,
    );
    hover(0, 540);
    flush();
    expect(crosshairs()[1].style.opacity).toBe("0");
  });
});
