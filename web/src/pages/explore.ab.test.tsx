import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ThemeProvider } from "@/components/theme-provider";
import { CHART_FALLBACK } from "@/lib/chart-theme";
import { CURATED_CHARTS, RANGE_TOKEN } from "@/lib/curated-metrics";
import { LOCALE_STORAGE_KEY, LocaleProvider, type Locale } from "@/lib/i18n";
import { TimeMachineProvider } from "@/lib/timemachine";
import type { PromResult } from "@/lib/types";
import { ExplorePage, exploreWindow, toCompareOption } from "./explore";

/** Explore's A/B compare panel: ONE extra chart at the top of the page that puts a second leg on A's axes. */
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

/** `locale` mounts a <LocaleProvider> above the page. Absent — every case but
 *  the ru smoke pin at the bottom of this file — there is no provider at all,
 *  which lib/i18n defines as English. */
function renderPage(locale?: Locale) {
  if (locale !== undefined) localStorage.setItem(LOCALE_STORAGE_KEY, locale);
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const page = <ExplorePage />;
  return render(
    <QueryClientProvider client={qc}>
      <ThemeProvider>
        <TimeMachineProvider>
          {locale === undefined ? page : <LocaleProvider>{page}</LocaleProvider>}
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

/** Explore resolves lib/curated-metrics' RANGE_TOKEN into the drawn window before it posts. */
function bodiesFor(bodies: RangeBody[], query: string): RangeBody[] {
  const head = query.split(RANGE_TOKEN)[0];
  return bodies.filter((b) => b.query.startsWith(head));
}

function setSelect(label: string, value: string) {
  fireEvent.change(screen.getByLabelText(label), { target: { value } });
}

beforeEach(() => {
  window.history.pushState({}, "", "/explore");
});

/** Switches the compare panel to "itself, earlier". */
const pickSelfMode = () => fireEvent.click(screen.getByRole("radio", { name: "itself, earlier" }));

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  window.history.pushState({}, "", "/");
  /* vitest.setup.ts backs localStorage with one Map per test FILE — a locale
     left behind would flip every later case in this one. */
  localStorage.removeItem(LOCALE_STORAGE_KEY);
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

/** Picking "7d earlier" on a Prometheus whose retention is 24h drew leg A alone — no legend entry for B. */
describe("ExplorePage compare panel — a shifted leg that has no data", () => {
  /** Answers the SHIFTED window (any request whose end is older than now by
   *  roughly the shift) with an empty matrix, and everything else normally. */
  function stubShiftedEmpty(shiftMs: number) {
    const fetchMock = vi.fn((url: string, init?: RequestInit) => {
      if (String(url).includes("/api/v1/promql/query_range")) {
        const body = JSON.parse(String(init?.body)) as RangeBody;
        const age = Date.now() - Date.parse(body.end);
        const empty = age > shiftMs / 2;
        return Promise.resolve(
          json({
            status: "success",
            data: {
              resultType: "matrix",
              result: empty ? [] : [{ metric: { protocol: "tcp" }, values: [[1785283200, "1"]] }],
            },
          }),
        );
      }
      return Promise.resolve(json({}));
    });
    vi.stubGlobal("fetch", fetchMock);
  }

  it("says so, and names the distance, instead of drawing leg A alone in silence", async () => {
    stubShiftedEmpty(7 * 24 * HOUR_MS);
    renderPage();
    pickSelfMode();
    setSelect("Compare with earlier", "7d");

    expect(
      await screen.findByText("No data 7d ago — Prometheus's retention does not reach that far back."),
    ).toBeInTheDocument();
  });

  it("keeps leg A drawn — it is real data, and the note is about B", async () => {
    stubShiftedEmpty(7 * 24 * HOUR_MS);
    renderPage();
    pickSelfMode();
    setSelect("Compare with earlier", "7d");

    await screen.findByText(/retention does not reach/);
    // Leg A is real data and stays drawn; in self-shift mode it is named by its
    // clock rather than by a title both legs share.
    expect(seriesNames(compareChart()).some((n) => n.startsWith("A · now"))).toBe(true);
  });

  it("says nothing when the shifted leg DOES come back with data", async () => {
    stubFetch();
    renderPage();
    pickSelfMode();
    setSelect("Compare with earlier", "24h");

    await waitFor(() => expect(seriesNames(compareChart()).length).toBe(2));
    expect(screen.queryByText(/retention does not reach/)).not.toBeInTheDocument();
  });

  it("says nothing in metric-B mode — an empty second METRIC is a different fact", async () => {
    // Every window comes back empty here, shifted or not.
    vi.stubGlobal(
      "fetch",
      vi.fn((url: string) =>
        Promise.resolve(
          String(url).includes("/api/v1/promql/query_range")
            ? json({ status: "success", data: { resultType: "matrix", result: [] } })
            : json({}),
        ),
      ),
    );
    renderPage();
    setSelect("Compare with metric", CHART_B.id);

    await waitFor(() => expect(screen.getAllByText(/no series returned/i).length).toBeGreaterThan(0));
    expect(screen.queryByText(/retention does not reach/)).not.toBeInTheDocument();
  });
});

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
    pickSelfMode();
    pickSelfMode();
    setSelect("Compare with earlier", "24h");

    await waitFor(() => expect(bodiesFor(bodies, CHART_A.query).length).toBeGreaterThan(1));
    const legs = bodiesFor(bodies, CHART_A.query);
    const shifted = legs.find((b) => Date.parse(b.end) !== AT_MS) as RangeBody;
    expect(shifted).toBeDefined();
    expect(Date.parse(shifted.end)).toBe(AT_MS - 24 * HOUR_MS);
    expect(Date.parse(shifted.start)).toBe(AT_MS - 24 * HOUR_MS - HOUR_MS);
  });

  it("labels the two legs plainly — now against that same metric earlier", async () => {
    stubFetch();
    renderPage();
    pickSelfMode();
    pickSelfMode();
    setSelect("Compare with earlier", "24h");
    await waitFor(() => expect(seriesNames(compareChart()).length).toBe(2));
    const names = seriesNames(compareChart());
    /* Not "A: <long metric title>" against "A (24h earlier)": in self-shift mode
       both legs ARE the same metric, so the only thing worth naming is which
       clock each one is on — and which stroke it was drawn with, because the
       two share a colour. */
    expect(names[0]).toContain("A · now (solid)");
    expect(names[1]).toContain("A · 24h earlier (dashed)");
  });

  it("offers exactly none / 1h / 24h / 7d", async () => {
    stubFetch();
    renderPage();
    pickSelfMode();
    const select = screen.getByLabelText("Compare with earlier") as HTMLSelectElement;
    expect([...select.options].map((o) => o.value)).toEqual(["", "1h", "24h", "7d"]);
  });
});

/**
 * The owner's report: choosing "1h earlier" silently dropped metric B and its
 * series — «куда делись ноды» — and the only thing that had said so was a line
 * of fine print under the chart. The mode is now a control, so the panel shows
 * one picker at a time and the choice is visible before it costs anything.
 */
describe("ExplorePage compare panel — the mode is a control, not fine print", () => {
  it("offers the two modes as a segment, opening on metric-B", async () => {
    stubFetch();
    renderPage();
    const group = screen.getByRole("radiogroup", { name: "Compare A with" });
    expect(within(group).getAllByRole("radio").map((r) => r.textContent)).toEqual([
      "another metric",
      "itself, earlier",
    ]);
    expect(within(group).getByRole("radio", { name: "another metric" })).toBeChecked();
  });

  it("shows ONE picker at a time — the other is gone, not greyed out beside it", async () => {
    stubFetch();
    renderPage();
    expect(screen.getByLabelText("Compare with metric")).toBeInTheDocument();
    expect(screen.queryByLabelText("Compare with earlier")).not.toBeInTheDocument();

    pickSelfMode();
    expect(screen.getByLabelText("Compare with earlier")).toBeInTheDocument();
    expect(screen.queryByLabelText("Compare with metric")).not.toBeInTheDocument();
  });

  it("never fetches B while comparing A with itself", async () => {
    const { bodies } = stubFetch();
    renderPage();
    setSelect("Compare with metric", CHART_B.id);
    await waitFor(() => expect(seriesNames(compareChart()).length).toBe(2));

    const beforeShift = bodiesFor(bodies, CHART_B.query).length;
    pickSelfMode();
    pickSelfMode();
    setSelect("Compare with earlier", "1h");
    await waitFor(() => expect(seriesNames(compareChart())[1]).toContain("1h earlier"));
    expect(bodiesFor(bodies, CHART_B.query).length).toBe(beforeShift);
  });

  it("REMEMBERS the B metric across a trip through self-shift mode", async () => {
    stubFetch();
    renderPage();
    setSelect("Compare with metric", CHART_B.id);
    await waitFor(() => expect(seriesNames(compareChart()).length).toBe(2));

    pickSelfMode();
    pickSelfMode();
    setSelect("Compare with earlier", "1h");
    await waitFor(() => expect(seriesNames(compareChart())[1]).toContain("1h earlier"));

    // Back again: the metric the reader picked is still picked. Losing it was
    // the whole of "куда делись ноды".
    fireEvent.click(screen.getByRole("radio", { name: "another metric" }));
    expect((screen.getByLabelText("Compare with metric") as HTMLSelectElement).value).toBe(CHART_B.id);
    await waitFor(() => expect(seriesNames(compareChart())[1]).toContain(`B: ${CHART_B.title}`));
  });

  it("drops the fine print — the controls say it structurally now", async () => {
    stubFetch();
    renderPage();
    pickSelfMode();
    pickSelfMode();
    setSelect("Compare with earlier", "24h");
    await waitFor(() => expect(seriesNames(compareChart()).length).toBe(2));
    expect(screen.queryByText(/time shift compares A with itself/i)).not.toBeInTheDocument();
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
    pickSelfMode();
    setSelect("Compare with earlier", "7d");
    await waitFor(() => expect(bodies.some((b) => Date.parse(b.end) !== AT_MS)).toBe(true));
    const shifted = bodies.find((b) => Date.parse(b.end) !== AT_MS) as RangeBody;
    expect(Date.parse(shifted.end)).toBe(AT_MS - 7 * 24 * HOUR_MS);
  });
});

/* the Russian is wired ONE smoke pin. */
describe("ExplorePage — Russian", () => {
  it("names the page and the compare panel, and keeps the no-request promise", async () => {
    stubFetch();
    renderPage("ru");

    expect(await screen.findByRole("heading", { name: "Метрики" })).toBeInTheDocument();
    expect(screen.getByRole("region", { name: "Сравнение" })).toBeInTheDocument();

    const idle = screen.getByText(/Выберите вторую метрику или прошлое окно/);
    expect(idle.textContent).toMatch(/ни один запрос не уходит/);

    // The mode segment names both choices, and each mode shows its own picker.
    const modes = screen.getByRole("radiogroup", { name: "Сравнить A" });
    expect(within(modes).getAllByRole("radio").map((r) => r.textContent)).toEqual([
      "с другой метрикой",
      "с собой раньше",
    ]);
    expect(within(screen.getByLabelText("Сравнить с метрикой")).getByText("Без второй метрики")).toBeInTheDocument();

    fireEvent.click(within(modes).getByRole("radio", { name: "с собой раньше" }));
    expect(within(screen.getByLabelText("Сравнить с прошлым")).getByText("Без сдвига")).toBeInTheDocument();
  });
});

/* ── the shifted leg the tooltip could not see ───────────────────────────── */

/**
 * The owner's report: at the cursor the compare tooltip listed ONLY the
 * "A (1h earlier)" row — the current leg's row was missing at the very instant
 * the two are supposed to be read against each other.
 *
 * The cause is not the tooltip. ECharts' axis trigger collects the series that
 * have a point AT the hovered axis value, and the two legs never shared one:
 * each leg anchored its window on its own `new Date()` inside its own queryFn,
 * so the two Prometheus grids were offset by however many milliseconds apart
 * the two fetches happened to fire. Flooring the anchor onto the step grid makes
 * the two windows land on the same sample instants by construction.
 */
describe("exploreWindow — the two legs share one grid", () => {
  const STEP = 15;
  const RANGE = 3600;
  const SHIFT = 3600;

  /** Prometheus emits samples at start + k·step; this is that grid. */
  const grid = (w: { start: Date; end: Date }, stepSeconds: number) => {
    const out: number[] = [];
    for (let ts = w.start.getTime(); ts <= w.end.getTime(); ts += stepSeconds * 1000) out.push(ts);
    return out;
  };

  it("floors the anchor onto the step, so a mid-step 'now' does not move the grid", () => {
    const messy = exploreWindow(Date.parse("2026-08-01T12:00:07.412Z"), RANGE, 0, STEP);
    const clean = exploreWindow(Date.parse("2026-08-01T12:00:00.000Z"), RANGE, 0, STEP);
    expect(messy.end.getTime()).toBe(clean.end.getTime());
  });

  it("puts the SHIFTED leg's samples exactly on the current leg's, once shifted back", () => {
    // Two fetches milliseconds apart, which is what actually happens.
    const now = exploreWindow(Date.parse("2026-08-01T12:00:07.412Z"), RANGE, 0, STEP);
    const earlier = exploreWindow(Date.parse("2026-08-01T12:00:07.583Z"), RANGE, SHIFT, STEP);

    const shiftedBack = grid(earlier, STEP).map((ts) => ts + SHIFT * 1000);
    // Sample for sample, not merely overlapping: this is what makes a vertical
    // read of the two lines an honest comparison.
    expect(shiftedBack).toEqual(grid(now, STEP));
  });

  it("ends the shifted window exactly one shift before the current one", () => {
    const now = exploreWindow(AT_MS, RANGE, 0, STEP);
    const earlier = exploreWindow(AT_MS, RANGE, SHIFT, STEP);
    expect(now.end.getTime() - earlier.end.getTime()).toBe(SHIFT * 1000);
    expect(now.end.getTime() - now.start.getTime()).toBe(RANGE * 1000);
    expect(earlier.end.getTime() - earlier.start.getTime()).toBe(RANGE * 1000);
  });
});

describe("toCompareOption — both legs land on the same instants", () => {
  const STEP = 15;
  const RANGE = 3600;
  const SHIFT = 3600;

  const samplesFor = (w: { start: Date; end: Date }, v: number): [number, string][] => {
    const out: [number, string][] = [];
    for (let ts = w.start.getTime(); ts <= w.end.getTime(); ts += STEP * 1000) {
      out.push([ts / 1000, String(v)]);
    }
    return out;
  };

  it("re-timestamps the earlier leg onto the current window, sample for sample", () => {
    const now = exploreWindow(Date.parse("2026-08-01T12:00:07.412Z"), RANGE, 0, STEP);
    const earlier = exploreWindow(Date.parse("2026-08-01T12:00:07.583Z"), RANGE, SHIFT, STEP);

    const opt = toCompareOption(
      { chart: CHART_A, label: "A · now", data: matrix("alpha", samplesFor(now, 1)) },
      {
        chart: CHART_A,
        label: "A · 1h earlier",
        data: matrix("alpha", samplesFor(earlier, 2)),
        shiftMs: SHIFT * 1000,
      },
      true,
    );

    const [legNow, legEarlier] = opt.series as BuiltSeries[];
    const xs = (s: BuiltSeries) => s.data.map(([ts]) => ts);
    expect(xs(legEarlier)).toEqual(xs(legNow));
    // Same count, so no sample was dropped or invented by the shift.
    expect(legEarlier.data).toHaveLength(legNow.data.length);
    // ...and the VALUES stayed each leg's own — only the clock moved.
    expect(legNow.data[0][1]).toBe(1);
    expect(legEarlier.data[0][1]).toBe(2);
  });
});

/*
The shift is self-compare mode's control, but it stayed applied to leg B after a switch back to
metric-B mode: B was fetched from a window an hour (or a week) in the past and drawn under a label
that named the metric and said nothing about time. The labels were already gated on `shifted`; the
query was not, so the picture and its legend disagreed.
*/
describe("Compare — the shift belongs to self-compare mode", () => {
  it("fetches leg B over the CURRENT window after a trip through self-shift", async () => {
    const { bodies } = stubFetch();
    renderPage();

    setSelect("Compare with metric", CHART_B.id);
    await waitFor(() => expect(seriesNames(compareChart()).length).toBe(2));

    pickSelfMode();
    setSelect("Compare with earlier", "24h");
    await waitFor(() => expect(seriesNames(compareChart())[1]).toContain("24h earlier"));

    fireEvent.click(screen.getByRole("radio", { name: "another metric" }));
    await waitFor(() => expect(seriesNames(compareChart())[1]).toContain(`B: ${CHART_B.title}`));

    /* The B request that came back, against the window leg A is on. Compared to
       A's own end rather than to a clock: both are floored onto the step grid,
       and it is the AGREEMENT that matters. A shifted B ends 24h earlier.
       Matched on the METRIC NAME because the recorded query has already had its
       range token resolved. */
    const metricOf = (chart: { query: string }) => chart.query.match(/kconmon_ng_\w+/)![0];
    const endOf = (chart: { query: string }) =>
      bodies.filter((b) => b.query.includes(metricOf(chart))).at(-1)?.end;
    await waitFor(() => expect(endOf(CHART_B)).toBeDefined());
    /* The newest end any leg asked for IS the current window; leg A's own last
       request cannot be the reference, because in self mode leg A is also the
       shifted leg. A B still carrying the shift ends 24h before this. */
    const newestEnd = bodies.map((b) => b.end).sort().at(-1);
    expect(endOf(CHART_B)).toBe(newestEnd);
  });
});
