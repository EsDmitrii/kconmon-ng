import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { useState } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ThemeProvider } from "@/components/theme-provider";
import { CHART_FALLBACK } from "@/lib/chart-theme";
import { LOCALE_STORAGE_KEY, LocaleProvider, type Locale } from "@/lib/i18n";
import { TimeMachineProvider } from "@/lib/timemachine";
import type { PromResult } from "@/lib/types";
import { PromQLConsolePage, ResultTabs, toChartModel } from "./promql-console";

/* The page mounts CodeMirror and ECharts; neither renders honestly in jsdom
   (no layout, no canvas), and neither is what these cases are about. Both are
   stubbed the way components/mtr-changes-timeline.test.tsx stubs EChart. */
vi.mock("@/components/promql-editor", () => ({
  PromQLEditor: () => <div data-testid="promql-editor" />,
}));
/* The option the page hands ECharts is where the legend rule and the series
   NAMES live, and neither is observable through a rendered canvas. Captured
   rather than re-derived, so these cases pin what the chart is actually told. */
const captured = vi.hoisted(() => ({ options: [] as Record<string, unknown>[] }));
vi.mock("@/components/echart", () => ({
  EChart: ({ option, className }: { option: Record<string, unknown>; className?: string }) => {
    captured.options.push(option);
    return <div data-testid="echart" className={className} />;
  },
}));

const lastOption = () => captured.options.at(-1) as {
  legend?: { show?: boolean };
  series?: { name: string }[];
  grid?: { bottom?: number };
};

/**
 * The Console's result switcher is the one place in this codebase that declared role="tablist"
 * without honouring the pattern.
 */

type Tab = "table" | "chart" | "json";

/** The strip is controlled, so a pin needs the state its page holds. */
function Harness({ chartDisabled = false }: { chartDisabled?: boolean }) {
  const [active, setActive] = useState<Tab>("table");
  return (
    <ResultTabs
      active={active}
      onChange={setActive}
      isDisabled={(id) => chartDisabled && id === "chart"}
    />
  );
}

const tab = (name: string) => screen.getByRole("tab", { name });

const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });

interface PromCall {
  url: string;
  body: { query: string; time?: string; start?: string; end?: string; step?: number };
}

/** The whole page, with the two heavy children stubbed out. `at` engages the
 *  Time Machine through the URL, which is the only carrier it has. */
function renderConsole(
  opts: {
    answer?: unknown;
    at?: string;
    httpStatus?: number;
    /** Mounts a <LocaleProvider> above the page. Absent — every case but the ru
     *  smoke pin at the bottom of this file — there is no provider at all,
     *  which lib/i18n defines as English. */
    locale?: Locale;
  } = {},
) {
  const {
    answer = { status: "success", data: { resultType: "vector", result: [] } },
    at,
    httpStatus = 200,
    locale,
  } = opts;
  if (locale !== undefined) localStorage.setItem(LOCALE_STORAGE_KEY, locale);
  window.history.pushState({}, "", at ? `/console?at=${at}` : "/console");
  const calls: PromCall[] = [];
  const fetchMock = vi.fn((url: string, init?: RequestInit) => {
    const href = String(url);
    if (href.includes("/api/v1/promql/")) {
      calls.push({ url: href, body: JSON.parse(String(init?.body ?? "{}")) });
      if (httpStatus !== 200) {
        return Promise.resolve(
          new Response(JSON.stringify({ type: "about:blank", title: "proxy refused", status: httpStatus, detail: "query too expensive" }), {
            status: httpStatus,
            headers: { "Content-Type": "application/problem+json" },
          }),
        );
      }
      return Promise.resolve(json(answer));
    }
    return Promise.resolve(json({}));
  });
  vi.stubGlobal("fetch", fetchMock);
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const page = <PromQLConsolePage />;
  const utils = render(
    <QueryClientProvider client={qc}>
      <ThemeProvider>
        <TimeMachineProvider>
          {locale === undefined ? page : <LocaleProvider>{page}</LocaleProvider>}
        </TimeMachineProvider>
      </ThemeProvider>
    </QueryClientProvider>,
  );
  return { ...utils, calls };
}

const run = () => fireEvent.click(screen.getByRole("button", { name: "Run" }));
const pickRange = () => fireEvent.click(screen.getByRole("radio", { name: "Range" }));

afterEach(() => {
  cleanup();
  captured.options.length = 0;
  vi.unstubAllGlobals();
  window.history.pushState({}, "", "/");
  /* vitest.setup.ts backs localStorage with one Map per test FILE — a locale
     left behind would flip every later case in this one. */
  localStorage.removeItem(LOCALE_STORAGE_KEY);
});

describe("Console result tabs", () => {
  it("is ONE tab stop: only the selected tab is reachable by Tab", () => {
    render(<Harness />);
    expect(tab("Table")).toHaveAttribute("tabindex", "0");
    expect(tab("Chart")).toHaveAttribute("tabindex", "-1");
    expect(tab("JSON")).toHaveAttribute("tabindex", "-1");
  });

  it("names the panel each tab reveals", () => {
    render(<Harness />);
    expect(tab("Table")).toHaveAttribute("aria-controls", "promql-result-panel-table");
    expect(tab("Chart")).toHaveAttribute("aria-controls", "promql-result-panel-chart");
    expect(tab("JSON")).toHaveAttribute("aria-controls", "promql-result-panel-json");
    // The other half of the wiring: the panel points back at its own tab, so
    // the id the tab claims has to be the id the tab carries.
    expect(tab("Table")).toHaveAttribute("id", "promql-result-tab-table");
  });

  it("moves selection AND focus with the arrow keys", () => {
    render(<Harness />);
    fireEvent.keyDown(tab("Table"), { key: "ArrowRight" });
    expect(tab("Chart")).toHaveAttribute("aria-selected", "true");
    expect(tab("Chart")).toHaveFocus();
    expect(tab("Table")).toHaveAttribute("tabindex", "-1");
  });

  it("wraps at both ends", () => {
    render(<Harness />);
    fireEvent.keyDown(tab("Table"), { key: "ArrowLeft" });
    expect(tab("JSON")).toHaveAttribute("aria-selected", "true");
    fireEvent.keyDown(tab("JSON"), { key: "ArrowRight" });
    expect(tab("Table")).toHaveAttribute("aria-selected", "true");
  });

  it("honours Home and End", () => {
    render(<Harness />);
    fireEvent.keyDown(tab("Table"), { key: "End" });
    expect(tab("JSON")).toHaveAttribute("aria-selected", "true");
    fireEvent.keyDown(tab("JSON"), { key: "Home" });
    expect(tab("Table")).toHaveAttribute("aria-selected", "true");
  });

  /* Chart is disabled for instant queries. A disabled button cannot take
     focus, so an arrow key that selected it would move selection to an element
     the keyboard can never reach — the strip steps over it instead. */
  it("steps over a disabled tab instead of stranding focus on it", () => {
    render(<Harness chartDisabled />);
    expect(tab("Chart")).toBeDisabled();
    fireEvent.keyDown(tab("Table"), { key: "ArrowRight" });
    expect(tab("JSON")).toHaveAttribute("aria-selected", "true");
    expect(tab("JSON")).toHaveFocus();
    expect(tab("Chart")).toHaveAttribute("aria-selected", "false");
  });

  it("still switches on click", () => {
    render(<Harness />);
    fireEvent.click(tab("JSON"));
    expect(tab("JSON")).toHaveAttribute("aria-selected", "true");
  });
});

/** Prometheus's own error envelope resolves rather than throws (lib/api's `handle`). */
describe("Console empty notes vs. a failed query", () => {
  const errorEnvelope = { status: "error", errorType: "bad_data", error: "parse error: unexpected end of input" };

  it("shows the error alone on the Table tab — never alongside a No-data note", async () => {
    renderConsole({ answer: errorEnvelope });

    run();
    expect(await screen.findByText(/parse error: unexpected end of input/)).toBeInTheDocument();
    expect(screen.queryByText(/no data/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/run a query to see results/i)).not.toBeInTheDocument();
  });

  it("shows the error alone on the Chart tab too", async () => {
    renderConsole({ answer: errorEnvelope });

    pickRange();
    run();
    await screen.findByText(/parse error: unexpected end of input/);
    fireEvent.click(tab("Chart"));
    expect(screen.queryByText(/no series to chart/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/run a range query/i)).not.toBeInTheDocument();
  });

  it("does the same for a transport-level failure, which has no envelope at all", async () => {
    renderConsole({ httpStatus: 422 });

    run();
    expect(await screen.findByRole("alert")).toBeInTheDocument();
    expect(screen.queryByText(/no data/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/run a query to see results/i)).not.toBeInTheDocument();
  });

  it("still says No data for a genuinely empty SUCCESS envelope", async () => {
    renderConsole({ answer: { status: "success", data: { resultType: "vector", result: [] } } });

    run();
    expect(await screen.findByText(/no data — the query returned an empty result/i)).toBeInTheDocument();
  });
});

/* QA scope 4, finding #16: Instant and Range answer different SHAPES of
   question, so the result on screen stops describing what the controls say the
   moment the mode changes — and nothing said so. */
describe("Console mode switch", () => {
  const oneSample = {
    status: "success",
    data: { resultType: "vector", result: [{ metric: { __name__: "up", node: "node-a" }, value: [1_754_000_000, "1"] }] },
  };

  it("drops the instant result when the mode flips to Range, rather than leaving it unmarked", async () => {
    renderConsole({ answer: oneSample });

    run();
    expect(await screen.findByText("node-a")).toBeInTheDocument();

    pickRange();
    await waitFor(() => expect(screen.queryByText("node-a")).not.toBeInTheDocument());
    // Back to the idle state, which is the honest one: nothing has been asked
    // in this mode yet.
    expect(screen.getByText(/run a query to see results/i)).toBeInTheDocument();
  });

  it("drops a range result on the way back to Instant too", async () => {
    renderConsole({ answer: oneSample });

    pickRange();
    run();
    expect(await screen.findByText("node-a")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("radio", { name: "Instant" }));
    await waitFor(() => expect(screen.queryByText("node-a")).not.toBeInTheDocument());
  });
});

/**
 * QA round 4, finding #3. /console was the one data surface that ignored `?at=`
 * entirely: engaged at an incident it kept answering with NOW.
 */
describe("Console under the Time Machine", () => {
  const AT = "2026-08-01T02:14:00Z";

  it("sends time=at for an instant query", async () => {
    const { calls } = renderConsole({ at: AT });

    run();
    await waitFor(() => expect(calls).toHaveLength(1));
    expect(calls[0].url).toContain("/api/v1/promql/query");
    expect(calls[0].body.time).toBe(new Date(AT).toISOString());
  });

  it("anchors a range query's END at `at`, measuring the picked window back from there", async () => {
    const { calls } = renderConsole({ at: AT });

    pickRange();
    run();
    await waitFor(() => expect(calls).toHaveLength(1));
    const { start, end } = calls[0].body;
    expect(end).toBe(new Date(AT).toISOString());
    // The default range is 1h.
    expect(new Date(end as string).getTime() - new Date(start as string).getTime()).toBe(60 * 60 * 1000);
  });

  it("says in the header which instant it is answering as of", async () => {
    renderConsole({ at: AT });

    expect(
      await screen.findByText(new RegExp(`as of ${new Date(AT).toLocaleString(undefined, { hour12: false })}`)),
    ).toBeInTheDocument();
  });

  it("sends no time at all while Live", async () => {
    const { calls } = renderConsole();

    run();
    await waitFor(() => expect(calls).toHaveLength(1));
    expect(calls[0].body.time).toBeUndefined();
  });
});

/* the Russian is wired ONE smoke pin. */
/* ── the results the owner sent back ─────────────────────────────────────── */

/**
 * Three faults in one screen, all of them the same fault: the Chart tab showed a
 * picture and nothing else, the only listing of what was IN the picture was an
 * ECharts legend that had turned into a "1/86" one-name-at-a-time pager, and
 * every name it did show was the whole label set truncated mid-label.
 *
 * The model is Grafana Explore: chart on top, the series listed underneath it at
 * all times, and every name cut down to what actually distinguishes it.
 */

/** N kubelet `up` series over a range, differing only by pod — the stand's shape. */
const fleetMatrix = (n: number) => ({
  status: "success",
  data: {
    resultType: "matrix",
    result: Array.from({ length: n }, (_, i) => ({
      metric: {
        __name__: "up",
        container: "agent",
        endpoint: "http",
        job: "kconmon-agent",
        namespace: "kconmon",
        pod: `kconmon-agent-${String(i).padStart(3, "0")}`,
      },
      values: [
        [1_754_000_000, "1"],
        [1_754_000_015, String(i)],
      ],
    })),
  },
});

async function runRange(answer: unknown) {
  const utils = renderConsole({ answer });
  // Range first — a mode switch drops whatever the previous mode answered.
  pickRange();
  fireEvent.click(tab("Chart"));
  run();
  await screen.findByTestId("echart");
  return utils;
}

const rawRows = () => screen.getAllByTestId("raw-row");

describe("Console Chart view — the chart AND the listing, at the same time", () => {
  it("draws the raw table under the chart rather than instead of it", async () => {
    await runRange(fleetMatrix(3));

    expect(screen.getByTestId("echart")).toBeInTheDocument();
    // The listing the Chart tab had none of: same panel, below the picture.
    expect(screen.getByTestId("raw-table")).toBeInTheDocument();
    expect(rawRows()).toHaveLength(3);
  });

  it("names every row by what DISTINGUISHES it, not by the whole label set", async () => {
    await runRange(fleetMatrix(2));

    expect(rawRows()[0]).toHaveTextContent('up{pod="kconmon-agent-000"}');
    // The five labels all 86 series share are not repeated on any row.
    expect(rawRows()[0]).not.toHaveTextContent("namespace");
  });

  it("says the shared part ONCE, above the listing", async () => {
    await runRange(fleetMatrix(2));

    expect(screen.getByTestId("raw-shared")).toHaveTextContent(
      'up{container="agent", endpoint="http", job="kconmon-agent", namespace="kconmon"}',
    );
  });

  it("carries each series' last value, which is what the row is for", async () => {
    await runRange(fleetMatrix(2));
    expect(rawRows()[1]).toHaveTextContent("1");
  });

  it("keeps the FULL labels reachable — on the row's title and behind its expander", async () => {
    await runRange(fleetMatrix(2));

    const full = 'up{container="agent", endpoint="http", job="kconmon-agent", namespace="kconmon", pod="kconmon-agent-000"}';
    expect(screen.getAllByTestId("raw-identity")[0]).toHaveAttribute("title", full);

    const expander = screen.getAllByRole("button", { name: /show all labels/i })[0];
    expect(expander).toHaveAttribute("aria-expanded", "false");
    fireEvent.click(expander);
    expect(expander).toHaveAttribute("aria-expanded", "true");
    expect(screen.getByTestId("raw-full-labels")).toHaveTextContent(full);
  });

  /* M4 typography, pinned once: identities and figures wear the data face, and
     the expanded label dump is primary content the reader ASKED for, so it must
     not carry the muted caption colour. */
  it("sets identities and the expanded labels in mono-data, and keeps the dump unmuted", async () => {
    await runRange(fleetMatrix(2));

    expect(screen.getAllByTestId("raw-identity")[0].className).toContain("mono-data");
    fireEvent.click(screen.getAllByRole("button", { name: /show all labels/i })[0]);
    const dump = screen.getByTestId("raw-full-labels");
    expect(dump.className).toContain("mono-data");
    expect(dump.className).not.toContain("text-muted-foreground");
  });

  it("pages the listing at ten, like every other list in the console", async () => {
    await runRange(fleetMatrix(86));

    expect(rawRows()).toHaveLength(10);
    expect(screen.getByTestId("pager-showing")).toHaveTextContent("Showing 10 of 86 series");
    fireEvent.click(screen.getByRole("button", { name: "Next page" }));
    expect(rawRows()[0]).toHaveTextContent('up{pod="kconmon-agent-010"}');
  });
});

describe("Console chart legend — the 1/86 pager is gone", () => {
  it("keeps a legend a reader can take in at a glance", async () => {
    await runRange(fleetMatrix(4));
    expect(lastOption().legend?.show).toBe(true);
  });

  it("drops it once it would page instead of list — the raw table IS the legend", async () => {
    await runRange(fleetMatrix(86));
    expect(lastOption().legend?.show).toBe(false);
  });

  it("gives the plot back the room the hidden legend was holding", async () => {
    await runRange(fleetMatrix(86));
    const hidden = lastOption().grid?.bottom ?? 0;
    cleanup();
    captured.options.length = 0;
    await runRange(fleetMatrix(4));
    expect(hidden).toBeLessThan(lastOption().grid?.bottom ?? 0);
  });

  it("hands the SERIES the minimal identity too, so legend and tooltip read it", async () => {
    // lib/chart-tooltip.ts's capped formatter prints `seriesName` verbatim, so
    // naming the series here is what fixes the tooltip as well.
    await runRange(fleetMatrix(2));
    expect(lastOption().series?.map((s) => s.name)).toEqual([
      'up{pod="kconmon-agent-000"}',
      'up{pod="kconmon-agent-001"}',
    ]);
  });
});

describe("Console Chart view — the honest empty and failed states survive", () => {
  it("draws no listing for a range that matched nothing", async () => {
    renderConsole({ answer: { status: "success", data: { resultType: "matrix", result: [] } } });
    pickRange();
    fireEvent.click(tab("Chart"));
    run();
    expect(await screen.findByText("No series to chart.")).toBeInTheDocument();
    expect(screen.queryByTestId("raw-table")).not.toBeInTheDocument();
    expect(screen.queryByTestId("echart")).not.toBeInTheDocument();
  });
});

/**
 * The owner, unable to read the Chart tab at all: «в консоли снова все
 * полосочки серые? не могу проверить именно по графикам т.к. они все ровные».
 *
 * Two separate faults, and one thing that turned out to be honest.
 *
 * GREY — the colours were assigned, and the swatch and the line always agreed;
 * the assignment itself was the bug. lib/chart-theme.ts's seriesColor folds
 * everything past the fifth series into `--chart-axis`, which is right for a
 * topk(5) curated chart and catastrophic here: `up` matches 86 series, so 81
 * lines came out in the same grey as the axis.
 *
 * FLAT — ECharts' value axis includes zero unless told otherwise, so a latency
 * series sitting at 42ms ±0.5ms was drawn as a straight line pinned to the top
 * of a 0…45ms axis. The variance was there; the axis was hiding it.
 *
 * A constant series is a different matter and stays flat, because it IS flat.
 */
describe("toChartModel — the Console's own plot, read honestly", () => {
  const twelve = fleetMatrix(12) as unknown as PromResult;

  it("gives every series on the reader's page a colour of its own", () => {
    const lines = (toChartModel(twelve, true).option.series as { color: string }[]).map((s) => s.color);

    expect(new Set(lines).size).toBe(12);
    // Not the axis grey — the specific colour 81 of 86 series used to be.
    expect(lines).not.toContain(CHART_FALLBACK.dark.axis);
  });

  it("keeps the row's swatch and the line's colour the same colour", () => {
    // The swatch is what ties a table row to a line once the legend is gone;
    // two independent colour assignments would eventually disagree.
    const model = toChartModel(twelve, false);

    expect(model.series.map((s) => s.color)).toEqual(
      (model.option.series as { color: string }[]).map((s) => s.color),
    );
  });

  it("still starts the ramp at the design system's first hue", () => {
    const model = toChartModel(fleetMatrix(2) as unknown as PromResult, true);

    expect(model.series[0].color).toBe(CHART_FALLBACK.dark.series[0]);
    expect(model.series[1].color).toBe(CHART_FALLBACK.dark.series[1]);
  });

  it("does not force a zero baseline, so a tight spread is visible at all", () => {
    const yAxis = toChartModel(twelve, true).option.yAxis as { scale?: boolean };

    expect(yAxis.scale).toBe(true);
  });

  it("still draws a one-point series, which is the OTHER way a plot looks empty", () => {
    // A single sample draws nothing with symbols off, and this page hands the
    // reader a step picker that can produce exactly one.
    const one = {
      status: "success",
      data: { resultType: "matrix", result: [{ metric: { __name__: "up" }, values: [[1_754_000_000, "1"]] }] },
    } as unknown as PromResult;

    expect((toChartModel(one, true).option.series as { showSymbol: boolean }[])[0].showSymbol).toBe(true);
  });
});

describe("PromQLConsolePage — Russian", () => {
  it("names its controls and keeps the idle/empty distinction in Russian", async () => {
    renderConsole({ locale: "ru" });

    expect(await screen.findByRole("heading", { name: "PromQL" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Выполнить" })).toBeInTheDocument();
    // JSON is a format name and does NOT move; the other two tabs do.
    expect(screen.getByRole("tab", { name: "Таблица" })).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "JSON" })).toBeInTheDocument();

    // Nothing run yet: the idle sentence, not the empty-result one.
    expect(screen.getByText("Выполните запрос, чтобы увидеть результат.")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Выполнить" }));
    // Ran, and matched nothing: a different sentence, with the different fact.
    expect(await screen.findByText("Данных нет: запрос вернул пустой результат.")).toBeInTheDocument();
  });

  it("names the listing under the chart in Russian, leaving Prometheus's own strings alone", async () => {
    renderConsole({ locale: "ru", answer: fleetMatrix(2) });
    fireEvent.click(screen.getByRole("radio", { name: "Диапазон" }));
    fireEvent.click(screen.getByRole("tab", { name: "График" }));
    fireEvent.click(screen.getByRole("button", { name: "Выполнить" }));

    await screen.findByTestId("echart");
    expect(screen.getByRole("columnheader", { name: "Точек" })).toBeInTheDocument();
    expect(screen.getByRole("columnheader", { name: "Последнее" })).toBeInTheDocument();
    expect(screen.getAllByRole("button", { name: "Показать все метки" })[0]).toBeInTheDocument();
    // The identity itself is Prometheus's — a label name is not a word to translate.
    expect(screen.getAllByTestId("raw-identity")[0]).toHaveTextContent('up{pod="kconmon-agent-000"}');
  });
});

/*
The table printed figures and nothing about WHEN they were read; the Chart tab had a timeline and
the Table tab had nothing at all (owner report). One instant for the whole table is a sentence
above it; instants that disagree become a column.
*/
describe("Console table — when the figures were read", () => {
  const READ_AT = 1785283200; // seconds, as Prometheus sends them
  const stamp = new Date(READ_AT * 1000).toLocaleString(undefined, { hour12: false });

  const vector = {
    status: "success",
    data: {
      resultType: "vector",
      result: [
        { metric: { job: "kubelet" }, value: [READ_AT, "1"] },
        { metric: { job: "node-exporter" }, value: [READ_AT, "1"] },
      ],
    },
  };

  it("says the instant an instant query was evaluated at, once above the table", async () => {
    renderConsole({ answer: vector });
    run();

    expect(await screen.findByText(`Read at ${stamp}`)).toBeInTheDocument();
    // Said once, not repeated onto every row.
    expect(screen.queryByRole("columnheader", { name: "time" })).toBeNull();
  });

  it("calls a range table's figures the LAST values, which is what they are", async () => {
    renderConsole({
      answer: {
        status: "success",
        data: {
          resultType: "matrix",
          result: [{ metric: { job: "kubelet" }, values: [[READ_AT - 60, "0"], [READ_AT, "1"]] }],
        },
      },
    });
    pickRange();
    run();

    expect(await screen.findByText(`Last values, read at ${stamp}`)).toBeInTheDocument();
  });

  it("puts a time column in instead when one series stopped earlier than the others", async () => {
    renderConsole({
      answer: {
        status: "success",
        data: {
          resultType: "matrix",
          result: [
            { metric: { job: "kubelet" }, values: [[READ_AT, "1"]] },
            { metric: { job: "gone" }, values: [[READ_AT - 3600, "1"]] },
          ],
        },
      },
    });
    pickRange();
    run();

    expect(await screen.findByRole("columnheader", { name: "time" })).toBeInTheDocument();
    expect(screen.queryByText(/read at/i)).toBeNull();
  });

  it("writes the stamp in the interface language", async () => {
    renderConsole({ locale: "ru", answer: vector });
    fireEvent.click(screen.getByRole("button", { name: "Выполнить" }));

    const ru = new Date(READ_AT * 1000).toLocaleString("ru-RU", { hour12: false });
    expect(await screen.findByText(`Снято на ${ru}`)).toBeInTheDocument();
  });
});
