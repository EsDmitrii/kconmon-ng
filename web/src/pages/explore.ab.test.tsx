import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ThemeProvider } from "@/components/theme-provider";
import { CHART_FALLBACK } from "@/lib/chart-theme";
import { CURATED_CHARTS } from "@/lib/curated-metrics";
import { TimeMachineProvider } from "@/lib/timemachine";
import type { PromResult } from "@/lib/types";
import { ExplorePage, toCompareOption } from "./explore";

/**
 * Explore's A/B compare panel: ONE extra chart at the top of the page that puts
 * a second leg on A's axes. Two exclusive modes — a second curated metric, or a
 * time shift of A against itself — and no free-form PromQL (that is /console).
 *
 * EChart is mocked (echarts.init reaches for a 2d canvas context jsdom does not
 * implement); the mock re-publishes the series names it was handed, which is
 * how a page test can see that both legs landed in the SAME chart with their
 * leg identity intact.
 */
vi.mock("@/components/echart", () => ({
  EChart: ({ option, className }: { option?: { series?: unknown }; className?: string }) => (
    <div
      data-testid="echart"
      className={className}
      data-series={((option?.series ?? []) as { name?: string }[]).map((s) => s.name ?? "").join("|")}
    />
  ),
}));

const AT = "2026-08-01T12:00:00Z";
const AT_MS = Date.parse(AT);
const HOUR_MS = 60 * 60 * 1000;

const CHART_A = CURATED_CHARTS[0]; // TCP RTT p95 — the panel's default leg A.
const CHART_B = CURATED_CHARTS[1]; // UDP packet loss.

const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });

interface RangeBody {
  query: string;
  start: string;
  end: string;
  step: number;
}

function stubFetch() {
  const bodies: RangeBody[] = [];
  const fetchMock = vi.fn((url: string, init?: RequestInit) => {
    if (String(url).includes("/api/v1/promql/query_range")) {
      bodies.push(JSON.parse(String(init?.body)) as RangeBody);
      return Promise.resolve(
        json({
          status: "success",
          data: { resultType: "matrix", result: [{ metric: { protocol: "tcp" }, values: [[1785283200, "1"]] }] },
        }),
      );
    }
    return Promise.resolve(json({}));
  });
  vi.stubGlobal("fetch", fetchMock);
  return { bodies, fetchMock };
}

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <ThemeProvider>
        <TimeMachineProvider>
          <ExplorePage />
        </TimeMachineProvider>
      </ThemeProvider>
    </QueryClientProvider>,
  );
}

/** The compare panel's own chart, told apart from the five curated ones. */
function compareChart(): HTMLElement {
  return within(screen.getByRole("region", { name: "Compare" })).getByTestId("echart");
}

function seriesNames(el: HTMLElement): string[] {
  const raw = el.getAttribute("data-series") ?? "";
  return raw === "" ? [] : raw.split("|");
}

function bodiesFor(bodies: RangeBody[], query: string): RangeBody[] {
  return bodies.filter((b) => b.query === query);
}

function setSelect(label: string, value: string) {
  fireEvent.change(screen.getByLabelText(label), { target: { value } });
}

beforeEach(() => {
  window.history.pushState({}, "", "/explore");
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  window.history.pushState({}, "", "/");
});

/* ------------------------------------------------------------------ */
/* The option builder, as a pure function                              */
/* ------------------------------------------------------------------ */

function matrix(label: string, points: [number, string][]): PromResult {
  return {
    status: "success",
    data: { resultType: "matrix", result: [{ metric: { host: label }, values: points }] },
  };
}

interface BuiltSeries {
  name: string;
  color: string;
  lineStyle: { width: number; type?: string; opacity?: number };
  data: [number, number][];
}

describe("toCompareOption", () => {
  const a = matrix("alpha", [[100, "1"]]);
  const b = matrix("beta", [[100, "2"]]);

  it("keeps leg identity on every series name so the legend can be read", () => {
    const opt = toCompareOption(
      { chart: CHART_A, label: `A: ${CHART_A.title}`, data: a },
      { chart: CHART_B, label: `B: ${CHART_B.title}`, data: b },
      true,
    );
    const names = (opt.series as BuiltSeries[]).map((s) => s.name);
    expect(names).toEqual([`A: ${CHART_A.title} · alpha`, `B: ${CHART_B.title} · beta`]);
  });

  it("mutes the B palette so A stays the subject of the chart", () => {
    const opt = toCompareOption(
      { chart: CHART_A, label: "A", data: a },
      { chart: CHART_B, label: "B", data: b },
      true,
    );
    const [legA, legB] = opt.series as BuiltSeries[];
    expect(legA.color).toBe(CHART_FALLBACK.dark.series[0]);
    expect(legA.lineStyle.opacity ?? 1).toBe(1);
    expect(legA.lineStyle.type ?? "solid").toBe("solid");
    // Same hue, deliberately dimmed and dashed — B reads as the reference leg.
    expect(legB.color).toBe(CHART_FALLBACK.dark.series[0]);
    expect(legB.lineStyle.opacity).toBeLessThan(1);
    expect(legB.lineStyle.type).toBe("dashed");
  });

  it("draws both legs on A's single pair of axes", () => {
    const opt = toCompareOption(
      { chart: CHART_A, label: "A", data: a },
      { chart: CHART_B, label: "B", data: b },
      true,
    );
    expect(Array.isArray(opt.xAxis)).toBe(false);
    expect(Array.isArray(opt.yAxis)).toBe(false);
    expect((opt.series as BuiltSeries[]).every((s) => !("yAxisIndex" in s))).toBe(true);
  });

  it("overlays a time-shifted leg on A's window by adding the shift back to its timestamps", () => {
    const opt = toCompareOption(
      { chart: CHART_A, label: "A: now", data: a },
      { chart: CHART_A, label: "A (24h earlier)", data: matrix("alpha", [[100, "2"]]), shiftMs: 24 * HOUR_MS },
      true,
    );
    const [legA, legB] = opt.series as BuiltSeries[];
    expect(legA.data[0][0]).toBe(100_000);
    // 100s (the sample's own instant) + 24h, so the two legs sit on top of each
    // other instead of B landing a day to the left of the visible window.
    expect(legB.data[0][0]).toBe(100_000 + 24 * HOUR_MS);
  });

  it("renders leg A alone when there is no B — the builder never invents a leg", () => {
    const opt = toCompareOption({ chart: CHART_A, label: "A", data: a }, undefined, true);
    expect((opt.series as BuiltSeries[]).map((s) => s.name)).toEqual(["A · alpha"]);
  });
});

/* ------------------------------------------------------------------ */
/* The panel on the page                                               */
/* ------------------------------------------------------------------ */

describe("ExplorePage compare panel — nothing selected", () => {
  it("fires no compare request and draws no compare chart until a leg B is chosen", async () => {
    const { bodies } = stubFetch();
    renderPage();
    await waitFor(() => expect(bodies.length).toBe(CURATED_CHARTS.length));
    // One request per curated chart and not one more: an idle panel is inert.
    expect(bodies.length).toBe(CURATED_CHARTS.length);
    expect(within(screen.getByRole("region", { name: "Compare" })).queryByTestId("echart")).toBeNull();
  });
});

describe("ExplorePage compare panel — metric-B mode", () => {
  it("asks for B's query over exactly A's window", async () => {
    const { bodies } = stubFetch();
    renderPage();
    await waitFor(() => expect(bodies.length).toBe(CURATED_CHARTS.length));
    setSelect("Compare with metric", CHART_B.id);

    await waitFor(() => expect(bodiesFor(bodies, CHART_B.query).length).toBeGreaterThan(0));
    const legB = bodiesFor(bodies, CHART_B.query).at(-1) as RangeBody;
    const legA = bodiesFor(bodies, CHART_A.query).at(-1) as RangeBody;
    // Default range is 1h, and B rides the same window rather than one of its own.
    expect(Date.parse(legB.end) - Date.parse(legB.start)).toBe(HOUR_MS);
    expect(legB.step).toBe(legA.step);
  });

  it("puts both legs in one chart, each carrying its leg letter and metric", async () => {
    stubFetch();
    renderPage();
    setSelect("Compare with metric", CHART_B.id);
    await waitFor(() => expect(seriesNames(compareChart()).length).toBe(2));
    const names = seriesNames(compareChart());
    expect(names[0]).toContain(`A: ${CHART_A.title}`);
    expect(names[1]).toContain(`B: ${CHART_B.title}`);
  });

  it("follows the metric-A picker, so A is not nailed to the first curated chart", async () => {
    const { bodies } = stubFetch();
    renderPage();
    setSelect("Metric A", CURATED_CHARTS[2].id);
    setSelect("Compare with metric", CHART_B.id);
    await waitFor(() => expect(seriesNames(compareChart()).length).toBe(2));
    expect(seriesNames(compareChart())[0]).toContain(`A: ${CURATED_CHARTS[2].title}`);
    expect(bodiesFor(bodies, CURATED_CHARTS[2].query).length).toBeGreaterThan(0);
  });
});

describe("ExplorePage compare panel — shift mode", () => {
  it("offsets A's OWN window by the shift: end_B = end_A − shift, start_B = start_A − shift", async () => {
    window.history.pushState({}, "", `/explore?at=${AT}`);
    const { bodies } = stubFetch();
    renderPage();
    await waitFor(() => expect(bodies.length).toBe(CURATED_CHARTS.length));
    setSelect("Compare with earlier", "24h");

    await waitFor(() => expect(bodiesFor(bodies, CHART_A.query).length).toBeGreaterThan(1));
    const legs = bodiesFor(bodies, CHART_A.query);
    const shifted = legs.find((b) => Date.parse(b.end) !== AT_MS) as RangeBody;
    expect(shifted).toBeDefined();
    expect(Date.parse(shifted.end)).toBe(AT_MS - 24 * HOUR_MS);
    expect(Date.parse(shifted.start)).toBe(AT_MS - 24 * HOUR_MS - HOUR_MS);
  });

  it("labels the shifted leg as A's own past rather than as a second metric", async () => {
    stubFetch();
    renderPage();
    setSelect("Compare with earlier", "24h");
    await waitFor(() => expect(seriesNames(compareChart()).length).toBe(2));
    const names = seriesNames(compareChart());
    expect(names[0]).toContain(`A: ${CHART_A.title}`);
    expect(names[1]).toContain("A (24h earlier)");
  });

  it("offers exactly none / 1h / 24h / 7d", async () => {
    stubFetch();
    renderPage();
    const select = screen.getByLabelText("Compare with earlier") as HTMLSelectElement;
    expect([...select.options].map((o) => o.value)).toEqual(["", "1h", "24h", "7d"]);
  });
});

describe("ExplorePage compare panel — the two modes are exclusive", () => {
  it("disables the metric-B picker while a shift is chosen and never fetches B", async () => {
    const { bodies } = stubFetch();
    renderPage();
    setSelect("Compare with metric", CHART_B.id);
    await waitFor(() => expect(seriesNames(compareChart()).length).toBe(2));

    const beforeShift = bodiesFor(bodies, CHART_B.query).length;
    setSelect("Compare with earlier", "1h");
    await waitFor(() => expect(seriesNames(compareChart())[1]).toContain("A (1h earlier)"));
    expect(screen.getByLabelText("Compare with metric")).toBeDisabled();
    expect(bodiesFor(bodies, CHART_B.query).length).toBe(beforeShift);
  });

  it("says so in the copy, so the ignored picker is not a mystery", async () => {
    stubFetch();
    renderPage();
    setSelect("Compare with earlier", "24h");
    expect(await screen.findByText(/time shift compares A with itself/i)).toBeInTheDocument();
  });
});

describe("ExplorePage compare panel under the Time Machine", () => {
  it("anchors BOTH legs at t rather than at now", async () => {
    window.history.pushState({}, "", `/explore?at=${AT}`);
    const { bodies } = stubFetch();
    renderPage();
    setSelect("Compare with metric", CHART_B.id);
    await waitFor(() => expect(bodiesFor(bodies, CHART_B.query).length).toBeGreaterThan(0));
    for (const b of bodiesFor(bodies, CHART_A.query)) expect(Date.parse(b.end)).toBe(AT_MS);
    for (const b of bodiesFor(bodies, CHART_B.query)) expect(Date.parse(b.end)).toBe(AT_MS);
  });

  it("keeps the shifted leg measured back from t, not from now", async () => {
    window.history.pushState({}, "", `/explore?at=${AT}`);
    const { bodies } = stubFetch();
    renderPage();
    setSelect("Compare with earlier", "7d");
    await waitFor(() => expect(bodies.some((b) => Date.parse(b.end) !== AT_MS)).toBe(true));
    const shifted = bodies.find((b) => Date.parse(b.end) !== AT_MS) as RangeBody;
    expect(Date.parse(shifted.end)).toBe(AT_MS - 7 * 24 * HOUR_MS);
  });
});
